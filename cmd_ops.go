package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"genc/internal/crypt"
)

const maxAttempts = 5

type PushCmd struct {
	Src, Dst string
	Force    bool
}

type PushResult struct {
	Dst string
	Err string // empty means ok
}

func (r *Repo) resolveLocal(src string) (string, error) {
	return r.local.Run("rev-parse", "--verify", "-q", src)
}

// Push implements the remote helper's push command.
func (r *Repo) Push(cmds []PushCmd, first *Snapshot) ([]PushResult, error) {
	for attempt := 0; attempt < maxAttempts; attempt++ {
		s := first
		first = nil
		if s == nil {
			var err error
			if s, err = r.Load(); err != nil {
				return nil, err
			}
		}
		if !s.Initialized() {
			return nil, errNotInitialized
		}
		res, err := r.tryPush(s, cmds)
		if errors.Is(err, errConflict) {
			r.info("远端已被其他人更新，正在重试…")
			continue
		}
		return res, err
	}
	return nil, errorf("远端更新过于频繁，推送失败，请稍后重试")
}

func (r *Repo) tryPush(s *Snapshot, cmds []PushCmd) ([]PushResult, error) {
	if out, _ := r.local.Run("rev-parse", "--is-shallow-repository"); out == "true" {
		return nil, errorf("浅克隆仓库不能推送到加密远端（数据会不完整），请先 git fetch --unshallow")
	}
	old := s.Index.Refs
	if err := r.EnsureObjects(s, refValues(old)); err != nil {
		return nil, err
	}
	newRefs := map[string]string{}
	for k, v := range old {
		newRefs[k] = v
	}
	var results []PushResult
	var include []string
	changed := false
	for _, c := range cmds {
		if !reRefOK.MatchString(c.Dst) || strings.Contains(c.Dst, "..") {
			results = append(results, PushResult{c.Dst, "invalid ref name"})
			continue
		}
		if c.Src == "" {
			if _, ok := newRefs[c.Dst]; ok {
				delete(newRefs, c.Dst)
				changed = true
			}
			results = append(results, PushResult{c.Dst, ""})
			continue
		}
		sha, err := r.resolveLocal(c.Src)
		if err != nil || !isHexSHA(sha) {
			results = append(results, PushResult{c.Dst, "src refspec does not match any object"})
			continue
		}
		prev := old[c.Dst]
		if prev == sha {
			results = append(results, PushResult{c.Dst, ""})
			continue
		}
		if prev != "" && !c.Force {
			if strings.HasPrefix(c.Dst, "refs/tags/") {
				results = append(results, PushResult{c.Dst, "already exists"})
				continue
			}
			if _, err := r.local.Run("merge-base", "--is-ancestor", prev, sha); err != nil {
				results = append(results, PushResult{c.Dst, "non-fast-forward"})
				continue
			}
		}
		newRefs[c.Dst] = sha
		include = append(include, sha)
		changed = true
		results = append(results, PushResult{c.Dst, ""})
	}
	if !changed {
		return results, nil
	}

	entries := map[string]string{}
	for p, sha := range s.Tree {
		entries[p] = sha
	}
	idx := *s.Index
	idx.Refs = newRefs
	idx.Head = chooseHead(newRefs, idx.Head)
	idx.Seq++
	idx.Prev = sha256Hex(s.IndexRaw)
	var newPack string
	if len(include) > 0 {
		packFile, size, err := r.buildPack(include, refValues(old))
		if err != nil {
			return nil, err
		}
		if packFile != "" {
			defer os.Remove(packFile)
			p, parts, err := r.storePack(s.Key, s.Manifest.RepoID, packFile, size)
			if err != nil {
				return nil, err
			}
			for k, v := range parts {
				entries[k] = v
			}
			idx.Packs = append(append([]PackRef{}, idx.Packs...), p)
			newPack = p.ID
		}
	}
	idxRaw, idxSHA, err := r.encryptIndex(s.Key, &idx)
	if err != nil {
		return nil, err
	}
	entries[pathIndex] = idxSHA
	commit, err := r.commitTree(entries, s.Commit)
	if err != nil {
		return nil, err
	}
	if err := r.pushStorage(commit, s.Commit); err != nil {
		return nil, err
	}
	if newPack != "" {
		r.markImported(newPack)
	}
	return results, r.recordPublished(s.Manifest, s.ManifestRaw, idxRaw, &idx)
}

// AdminOp describes a change to membership and/or keys.
type AdminOp struct {
	Add        *Member
	AddName    string
	Remove     string // fp
	Rekey      string // fp whose key is replaced by Add
	NewKey     bool   // rotate the master key (implies Repack)
	Repack     bool   // rewrite all data into a single pack (orphan commit)
	Takeover   bool   // signer need not be a member (restore from backup)
	Op         string // log text
	SignerPass PassphraseFunc
}

func (op AdminOp) changesManifest() bool {
	return op.Add != nil || op.Remove != "" || op.NewKey || op.Takeover
}

// Admin applies op with retries on concurrent updates.
func (r *Repo) Admin(op AdminOp) (*Snapshot, error) {
	if op.NewKey {
		op.Repack = true
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		s, err := r.Load()
		if err != nil {
			return nil, err
		}
		if !s.Initialized() {
			return nil, errNotInitialized
		}
		err = r.tryAdmin(s, op)
		if errors.Is(err, errConflict) {
			r.info("远端已被其他人更新，正在重试…")
			continue
		}
		if err != nil {
			return nil, err
		}
		return r.Load()
	}
	return nil, errorf("远端更新过于频繁，操作失败，请稍后重试")
}

// signerFor returns one of the user's identities that may sign the next manifest.
func signerFor(m *Manifest, takeover bool, pass PassphraseFunc) (*Identity, error) {
	ids := FindIdentities()
	for _, id := range ids {
		pub, ok := m.member(id.FP())
		if takeover || (ok && AuthorizedKey(pub) == AuthorizedKey(id.Pub)) {
			if err := id.Unlock(pass); err != nil {
				return nil, errorf("无法使用私钥签名: %v", err)
			}
			return id, nil
		}
	}
	return nil, errorf("你的私钥不在成员名单中，不能执行此操作")
}

func (r *Repo) tryAdmin(s *Snapshot, op AdminOp) error {
	m := *s.Manifest
	idx := *s.Index
	idx.Names = map[string]string{}
	for k, v := range s.Index.Names {
		idx.Names[k] = v
	}
	members := append([]Member{}, m.Members...)
	var signer *Identity
	if op.changesManifest() {
		var err error
		if signer, err = signerFor(&m, op.Takeover, op.SignerPass); err != nil {
			return err
		}
	}
	if op.Rekey != "" && signer != nil && op.Rekey == signer.FP() {
		return errorf("不能用旧私钥更换自己的公钥（新清单的签名人必须是成员）。" +
			"请让其他成员执行；或先 genc add 你的新公钥，再用新私钥 genc remove 旧公钥")
	}
	if op.Remove != "" {
		if signer != nil && op.Remove == signer.FP() {
			return errorf("不能移除自己")
		}
		var kept []Member
		for _, mem := range members {
			if mem.FP != op.Remove {
				kept = append(kept, mem)
			}
		}
		if len(kept) == len(members) {
			return errorf("成员 %s 不存在", op.Remove)
		}
		members = kept
		delete(idx.Names, op.Remove)
	}
	if op.Add != nil {
		replaced := false
		for i, mem := range members {
			if mem.FP == op.Rekey && op.Rekey != "" {
				members[i] = *op.Add
				replaced = true
				if n, ok := idx.Names[op.Rekey]; ok {
					delete(idx.Names, op.Rekey)
					idx.Names[op.Add.FP] = n
				}
			} else if mem.FP == op.Add.FP {
				return errorf("该公钥已经是成员")
			}
		}
		if op.Rekey != "" && !replaced {
			return errorf("成员 %s 不存在", op.Rekey)
		}
		if !replaced {
			members = append(members, *op.Add)
		}
		if op.AddName != "" {
			idx.Names[op.Add.FP] = op.AddName
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].FP < members[j].FP })

	key := s.Key
	if op.NewKey {
		var err error
		if key, err = crypt.NewKey(); err != nil {
			return err
		}
		idx.KeyCreated = nowUTC()
	}

	entries := map[string]string{}
	parent := s.Commit
	if op.Repack {
		parent = "" // orphan commit: old ciphertext disappears from the branch history
	} else {
		for p, sha := range s.Tree {
			entries[p] = sha
		}
	}
	readme, err := r.writeBlob([]byte(readmeContents))
	if err != nil {
		return err
	}
	entries[pathReadme] = readme

	var newPack *PackRef
	if op.Repack {
		if err := r.EnsureObjects(s, refValues(idx.Refs)); err != nil {
			return err
		}
		idx.Packs = nil
		if len(idx.Refs) > 0 {
			packFile, size, err := r.buildPack(refValues(idx.Refs), nil)
			if err != nil {
				return err
			}
			if packFile != "" {
				defer os.Remove(packFile)
				p, parts, err := r.storePack(key, m.RepoID, packFile, size)
				if err != nil {
					return err
				}
				for k, v := range parts {
					entries[k] = v
				}
				idx.Packs = []PackRef{p}
				newPack = &p
			}
		}
	}

	byFP := ""
	if signer != nil {
		byFP = signer.FP()
	}
	idx.Seq++
	idx.Prev = sha256Hex(s.IndexRaw)
	idx.Log = append(append([]LogEntry{}, idx.Log...), LogEntry{Time: nowUTC(), By: byFP, Op: op.Op})
	idxRaw, idxSHA, err := r.encryptIndex(key, &idx)
	if err != nil {
		return err
	}
	entries[pathIndex] = idxSHA

	mraw := s.ManifestRaw
	newManifest := s.Manifest
	if op.changesManifest() {
		m.Members = members
		m.KeyID = key.IDHex()
		m.Prev = sha256Hex(s.ManifestRaw)
		m.Version++
		raw, sig, err := signManifest(&m, signer)
		if err != nil {
			return err
		}
		mraw, newManifest = raw, &m
		if entries[pathManifest], err = r.writeBlob(raw); err != nil {
			return err
		}
		if entries[pathSignature], err = r.writeBlob(sig); err != nil {
			return err
		}
		for p := range entries {
			if strings.HasPrefix(p, "keys/") && strings.HasSuffix(p, ".key") {
				delete(entries, p)
			}
		}
		for _, mem := range members {
			box, err := makeKeyBox(key, mem.Key)
			if err != nil {
				return err
			}
			if entries["keys/"+mem.FP+".key"], err = r.writeBlob(box); err != nil {
				return err
			}
		}
	} else {
		entries[pathManifest] = s.Tree[pathManifest]
		entries[pathSignature] = s.Tree[pathSignature]
		for p, sha := range s.Tree {
			if strings.HasPrefix(p, "keys/") {
				entries[p] = sha
			}
		}
	}

	if op.Repack {
		if err := r.verifyRewrite(key, m.RepoID, idx, entries); err != nil {
			return errorf("本地校验失败，未推送: %v", err)
		}
	}
	commit, err := r.commitTree(entries, parent)
	if err != nil {
		return err
	}
	if op.Repack {
		// keep the previous storage state locally as a safety net
		r.cache.Run("update-ref", "refs/genc-backup/"+strings.ReplaceAll(nowUTC(), ":", "-"), s.Commit)
	}
	if err := r.pushStorage(commit, s.Commit); err != nil {
		return err
	}
	if op.NewKey {
		if err := r.saveKey(key); err != nil {
			return err
		}
	}
	if newPack != nil {
		r.markImported(newPack.ID)
	}
	return r.recordPublished(newManifest, mraw, idxRaw, &idx)
}

// verifyRewrite decrypts what is about to be published into a scratch repository
// and checks that every ref and all reachable objects are present.
func (r *Repo) verifyRewrite(k crypt.Key, repoID string, idx Index, entries map[string]string) error {
	ct, err := r.readBlob(entries[pathIndex])
	if err != nil {
		return err
	}
	raw, err := crypt.DecryptBytes(ct, k, "index", indexContext(repoID))
	if err != nil {
		return err
	}
	var back Index
	if err := json.Unmarshal(raw, &back); err != nil || back.Seq != idx.Seq || len(back.Refs) != len(idx.Refs) {
		return errorf("索引校验不一致")
	}
	if len(idx.Refs) == 0 {
		return nil
	}
	scratch := r.tmpPath("verify")
	defer os.RemoveAll(scratch)
	if _, err := (Git{}).Run("init", "-q", "--bare", scratch); err != nil {
		return err
	}
	sg := Git{Dir: scratch}
	for _, p := range idx.Packs {
		path, err := r.decryptPack(nil, k, repoID, p, entries)
		if err != nil {
			return err
		}
		err = indexPack(sg, path)
		os.Remove(path)
		if err != nil {
			return err
		}
	}
	args := append([]string{"rev-list", "--objects", "--quiet"}, refValues(idx.Refs)...)
	if _, err := sg.Run(args...); err != nil {
		return errorf("数据不完整: %v", err)
	}
	return nil
}

// Init creates the encrypted storage on an empty remote.
func Init(r *Repo, name string) (*Identity, error) {
	if err := r.ensureCache(); err != nil {
		return nil, err
	}
	var refs bytes.Buffer
	c := r.cache.cmd(gitOpts{stdout: &refs}, "ls-remote", "--", r.URL)
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return nil, errorf("无法访问远端 %s: %v", r.URL, err)
	}
	if strings.TrimSpace(refs.String()) != "" {
		return nil, errorf("远端仓库不是空的。如果它已经是 genc 加密仓库，请使用 git clone genc::%s；"+
			"否则其中可能已有明文代码，请换一个全新的空仓库（明文历史一旦公开过就已泄露，加密无法挽回）", r.URL)
	}
	ids := FindIdentities()
	if len(ids) == 0 {
		return nil, errorf("没有找到 SSH 私钥，请先运行 genc keygen")
	}
	me := ids[0]
	if err := me.Unlock(r.Pass); err != nil {
		return nil, err
	}
	key, err := crypt.NewKey()
	if err != nil {
		return nil, err
	}
	m := &Manifest{Format: 1, RepoID: randHex(16), KeyID: key.IDHex(), Version: 1,
		Members: []Member{{FP: me.FP(), Key: AuthorizedKey(me.Pub)}}}
	mraw, sig, err := signManifest(m, me)
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = me.FP()
	}
	idx := &Index{Format: 1, RepoID: m.RepoID, Seq: 1, Refs: map[string]string{},
		Names: map[string]string{me.FP(): name}, KeyCreated: nowUTC(),
		Log: []LogEntry{{Time: nowUTC(), By: me.FP(), Op: "init"}}}
	entries := map[string]string{}
	idxRaw, idxSHA, err := r.encryptIndex(key, idx)
	if err != nil {
		return nil, err
	}
	entries[pathIndex] = idxSHA
	box, err := makeKeyBox(key, AuthorizedKey(me.Pub))
	if err != nil {
		return nil, err
	}
	for p, data := range map[string][]byte{pathReadme: []byte(readmeContents), pathManifest: mraw, pathSignature: sig, "keys/" + me.FP() + ".key": box} {
		if entries[p], err = r.writeBlob(data); err != nil {
			return nil, err
		}
	}
	commit, err := r.commitTree(entries, "")
	if err != nil {
		return nil, err
	}
	if err := r.saveKey(key); err != nil {
		return nil, err
	}
	if err := r.pushStorage(commit, ""); err != nil {
		os.Remove(filepath.Join(r.dir, "key"))
		return nil, err
	}
	return me, r.recordPublished(m, mraw, idxRaw, idx)
}
