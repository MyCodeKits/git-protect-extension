package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"filippo.io/age"
	"golang.org/x/crypto/ssh"

	"genc/internal/crypt"
	"genc/internal/sshsig"
)

var errorf = fmt.Errorf

const (
	storageRef     = "refs/heads/main"
	sigNamespace   = "genc-manifest@v1"
	pathManifest   = "keys/manifest.json"
	pathSignature  = "keys/manifest.sig"
	pathIndex      = "index.enc"
	pathReadme     = "README.md"
	partSize       = 48 << 20 // keep each stored file well under GitHub's 100MB limit
	maxPackParts   = 4096
	readmeContents = "# Encrypted repository\n\nThis repository is encrypted with genc.\nIts contents are only readable by members who hold a key.\n\n    git clone genc::<this repository url>\n"
)

type Member struct {
	FP  string `json:"fp"`
	Key string `json:"key"` // OpenSSH authorized_keys format
}

// Manifest is public (plaintext) and signed by a member. It pins the repository
// identity, the current key id and the member list.
type Manifest struct {
	Format  int      `json:"format"`
	RepoID  string   `json:"repo_id"`
	KeyID   string   `json:"key_id"`
	Version uint64   `json:"version"`
	Prev    string   `json:"prev"`
	Members []Member `json:"members"`
	Signer  string   `json:"signer"`
	Time    string   `json:"time"`
}

type PackRef struct {
	ID    string `json:"id"`
	Parts int    `json:"parts"`
	Size  int64  `json:"size"`
}

type LogEntry struct {
	Time string `json:"t"`
	By   string `json:"by"`
	Op   string `json:"op"`
}

// Index is encrypted; it holds everything that must stay secret.
type Index struct {
	Format     int               `json:"format"`
	RepoID     string            `json:"repo_id"`
	Seq        uint64            `json:"seq"`
	Prev       string            `json:"prev"`
	Refs       map[string]string `json:"refs"`
	Head       string            `json:"head"`
	Packs      []PackRef         `json:"packs"`
	Names      map[string]string `json:"names"`
	KeyCreated string            `json:"key_created"`
	Log        []LogEntry        `json:"log"`
}

// LocalState is what this clone has verified so far (trust-on-first-use + anti-rollback).
type LocalState struct {
	URL             string   `json:"url"`
	RepoID          string   `json:"repo_id"`
	ManifestVersion uint64   `json:"manifest_version"`
	ManifestHash    string   `json:"manifest_hash"`
	Members         []Member `json:"members"`
	IndexSeq        uint64   `json:"index_seq"`
	IndexHash       string   `json:"index_hash"`
	Imported        []string `json:"imported"`
}

// Snapshot is a verified, decrypted view of the remote storage branch.
type Snapshot struct {
	Commit      string
	Tree        map[string]string
	Manifest    *Manifest
	ManifestRaw []byte
	Index       *Index
	IndexRaw    []byte
	Key         crypt.Key
}

func (s *Snapshot) Initialized() bool { return s.Commit != "" }

type Repo struct {
	GitDir string
	URL    string
	dir    string
	local  Git
	cache  Git
	lock   *fileLock
	Pass   PassphraseFunc
	Quiet  bool // suppress informational stderr output
}

var (
	errNotInitialized = errors.New("远端不是 genc 加密仓库（没有找到存储分支），请先运行 genc init")
	errConflict       = errors.New("remote storage changed concurrently")
)

func OpenRepo(gitDir, url string) (*Repo, error) {
	gitDir, err := filepath.Abs(gitDir)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(url, "genc::") {
		return nil, errorf("非法的远程地址（重复的 genc:: 前缀）")
	}
	r := &Repo{GitDir: gitDir, URL: url, dir: filepath.Join(gitDir, "genc"), Pass: nonInteractive}
	r.local = Git{Dir: gitDir}
	r.cache = Git{Dir: filepath.Join(r.dir, "cache")}
	if err := os.MkdirAll(filepath.Join(r.dir, "tmp"), 0o700); err != nil {
		return nil, err
	}
	if out, err := r.local.Run("rev-parse", "--show-object-format"); err == nil && out != "sha1" {
		return nil, errorf("暂不支持 %s 对象格式的仓库", out)
	}
	return r, nil
}

func (r *Repo) Lock() error {
	l, err := acquireLock(r.dir)
	r.lock = l
	return err
}

func (r *Repo) Unlock() { r.lock.Release(); r.lock = nil }

func (r *Repo) info(format string, a ...any) {
	if !r.Quiet {
		fmt.Fprintf(os.Stderr, "genc: "+format+"\n", a...)
	}
}

func (r *Repo) tmpPath(name string) string {
	return filepath.Join(r.dir, "tmp", name+"-"+randHex(6))
}

// ---- local state and key ----

func (r *Repo) loadState() (*LocalState, error) {
	st := &LocalState{}
	b, err := os.ReadFile(filepath.Join(r.dir, "state.json"))
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, errorf("本地状态文件损坏: %v", err)
	}
	return st, nil
}

func (r *Repo) saveState(st *LocalState) error {
	b, _ := json.MarshalIndent(st, "", "  ")
	return writeAtomic(filepath.Join(r.dir, "state.json"), b, 0o600)
}

func (r *Repo) loadKey() (crypt.Key, bool) {
	b, err := os.ReadFile(filepath.Join(r.dir, "key"))
	if err != nil {
		return crypt.Key{}, false
	}
	k, err := crypt.ParseKeyHex(strings.TrimSpace(string(b)))
	return k, err == nil
}

func (r *Repo) saveKey(k crypt.Key) error {
	return writePrivateFile(filepath.Join(r.dir, "key"), []byte(hex.EncodeToString(k[:])+"\n"))
}

// ---- cache of the remote storage branch ----

func (r *Repo) ensureCache() error {
	if _, err := os.Stat(filepath.Join(r.dir, "cache", "HEAD")); err == nil {
		return nil
	}
	_, err := Git{}.Run("init", "-q", "--bare", filepath.Join(r.dir, "cache"))
	return err
}

// remoteHead returns the storage commit on the remote ("" if none).
func (r *Repo) remoteHead() (string, error) {
	var out bytes.Buffer
	c := r.cache.cmd(gitOpts{stdout: &out}, "ls-remote", "--", r.URL, storageRef)
	c.Stderr = os.Stderr // credential prompts and network errors are for the user
	if err := c.Run(); err != nil {
		return "", errorf("无法访问远端 %s: %v", r.URL, err)
	}
	for _, line := range strings.Split(out.String(), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == storageRef && isHexSHA(f[0]) {
			return f[0], nil
		}
	}
	return "", nil
}

func (r *Repo) fetchStorage() (string, error) {
	if err := r.ensureCache(); err != nil {
		return "", err
	}
	head, err := r.remoteHead()
	if err != nil || head == "" {
		return "", err
	}
	if have, _ := r.cache.ExistingObjects([]string{head}); !have[head] {
		if err := r.cache.RunPassStderr("fetch", "-q", "--no-tags", "--", r.URL, "+"+storageRef+":"+storageRef); err != nil {
			return "", err
		}
	}
	if _, err := r.cache.Run("update-ref", storageRef, head); err != nil {
		return "", err
	}
	return head, nil
}

func (r *Repo) readTree(commit string) (map[string]string, error) {
	out, err := r.cache.Run("ls-tree", "-r", "-z", commit)
	if err != nil {
		return nil, err
	}
	tree := map[string]string{}
	for _, ent := range strings.Split(out, "\x00") {
		if ent == "" {
			continue
		}
		tab := strings.IndexByte(ent, '\t')
		f := strings.Fields(ent[:max(tab, 0)])
		if tab < 0 || len(f) != 3 || f[1] != "blob" {
			return nil, errorf("存储分支包含无法识别的条目")
		}
		tree[ent[tab+1:]] = f[2]
	}
	return tree, nil
}

func (r *Repo) readBlob(sha string) ([]byte, error) {
	var out bytes.Buffer
	_, err := r.cache.RunWith(gitOpts{stdout: &out}, "cat-file", "blob", sha)
	return out.Bytes(), err
}

func (r *Repo) writeBlob(data []byte) (string, error) {
	return r.cache.RunWith(gitOpts{stdin: bytes.NewReader(data)}, "hash-object", "-w", "--stdin")
}

func (r *Repo) writeBlobFile(path string) (string, error) {
	return r.cache.Run("hash-object", "-w", "--", path)
}

// ---- verification ----

func sha256Hex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

var (
	reHex32  = regexp.MustCompile(`^[0-9a-f]{32}$`)
	reHex16  = regexp.MustCompile(`^[0-9a-f]{16}$`)
	reRefOK  = regexp.MustCompile(`^refs/[\x21-\x7e]+$`)
	reNameOK = regexp.MustCompile(`^[^\x00-\x1f\x7f]{1,64}$`)
)

func (m *Manifest) member(fp string) (ssh.PublicKey, bool) {
	for _, mem := range m.Members {
		if mem.FP == fp {
			k, err := ParseMemberKey(mem.Key)
			return k, err == nil
		}
	}
	return nil, false
}

func verifyManifest(raw, sig []byte) (*Manifest, error) {
	m := &Manifest{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(m); err != nil {
		return nil, errorf("签名清单格式错误: %v", err)
	}
	if m.Format != 1 || !reHex32.MatchString(m.RepoID) || !reHex16.MatchString(m.KeyID) || len(m.Members) == 0 {
		return nil, errorf("签名清单格式错误")
	}
	seen := map[string]bool{}
	for _, mem := range m.Members {
		k, err := ParseMemberKey(mem.Key)
		if err != nil || Fingerprint(k) != mem.FP || seen[mem.FP] {
			return nil, errorf("签名清单中的成员公钥无效")
		}
		seen[mem.FP] = true
	}
	signer, ok := m.member(m.Signer)
	if !ok {
		return nil, errorf("签名清单的签名人不是成员")
	}
	if err := sshsig.Verify(signer, sigNamespace, raw, sig); err != nil {
		return nil, errorf("签名清单的签名无效（可能被篡改）")
	}
	return m, nil
}

func sameMember(list []Member, fp, key string) bool {
	for _, m := range list {
		if m.FP == fp && m.Key == key {
			return true
		}
	}
	return false
}

// checkTrust enforces trust-on-first-use, signer continuity and anti-rollback.
func (r *Repo) checkTrust(st *LocalState, m *Manifest, raw []byte) error {
	h := sha256Hex(raw)
	if st.RepoID == "" {
		r.info("首次连接此加密仓库。签名人指纹 %s，成员 %d 人。请与邀请你的成员核对签名人指纹。", m.Signer, len(m.Members))
		return nil
	}
	switch {
	case m.RepoID != st.RepoID:
		return errorf("安全警告：远端仓库 ID 已改变（可能被替换）。如确认无误，运行 genc trust --reset")
	case m.Version < st.ManifestVersion:
		return errorf("安全警告：远端签名清单版本回退（%d < %d），可能是回滚攻击", m.Version, st.ManifestVersion)
	case m.Version == st.ManifestVersion && h != st.ManifestHash:
		return errorf("安全警告：远端签名清单与本地记录不一致（同一版本内容不同）")
	case m.Version > st.ManifestVersion:
		signerKey, _ := m.member(m.Signer)
		if !sameMember(st.Members, m.Signer, AuthorizedKey(signerKey)) {
			return errorf("安全警告：签名清单由不受信任的密钥 %s 签署，已拒绝。如确认无误，运行 genc trust --reset", m.Signer)
		}
	}
	return nil
}

// unlockKey opens this user's key box. Returns a helpful error if not possible.
func (r *Repo) unlockKey(tree map[string]string, m *Manifest) (crypt.Key, error) {
	ids := FindIdentities()
	var fps []string
	needPass := false
	for _, id := range ids {
		fps = append(fps, id.FP())
		pub, ok := m.member(id.FP())
		if !ok || AuthorizedKey(pub) != AuthorizedKey(id.Pub) {
			continue
		}
		boxSHA, ok := tree["keys/"+id.FP()+".key"]
		if !ok {
			return crypt.Key{}, errorf("存储分支缺少你的密钥文件")
		}
		if err := id.Unlock(r.Pass); err != nil {
			if errors.Is(err, errNeedPassphrase) {
				needPass = true
				continue
			}
			return crypt.Key{}, err
		}
		box, err := r.readBlob(boxSHA)
		if err != nil {
			return crypt.Key{}, err
		}
		rd, err := age.Decrypt(bytes.NewReader(box), id.age)
		if err != nil {
			return crypt.Key{}, errorf("无法解开你的密钥文件: %v", err)
		}
		kb, err := io.ReadAll(io.LimitReader(rd, crypt.KeySize+1))
		if err != nil || len(kb) != crypt.KeySize {
			return crypt.Key{}, errorf("密钥文件内容无效")
		}
		var k crypt.Key
		copy(k[:], kb)
		if k.IDHex() != m.KeyID {
			return crypt.Key{}, errorf("安全警告：密钥文件与签名清单不一致（可能被篡改），已拒绝")
		}
		return k, nil
	}
	if needPass {
		return crypt.Key{}, errorf("需要解锁：你的私钥有口令。请在终端运行 genc unlock，或在 genc 管理面板中点击\"解锁\"")
	}
	if len(fps) == 0 {
		return crypt.Key{}, errorf("没有找到你的成员私钥。请运行 genc keygen 生成，然后把公钥发给现有成员")
	}
	return crypt.Key{}, errorf("你还不是该仓库的成员。你的公钥指纹：%s。请把指纹和公钥（genc whoami）发给现有成员，让他运行 genc add", strings.Join(fps, ", "))
}

func validateIndex(idx *Index, m *Manifest, tree map[string]string) error {
	if idx.Format != 1 || idx.RepoID != m.RepoID {
		return errorf("索引与签名清单不匹配")
	}
	for name, sha := range idx.Refs {
		if !reRefOK.MatchString(name) || strings.Contains(name, "..") || !isHexSHA(sha) {
			return errorf("索引中包含非法的引用名")
		}
	}
	if idx.Head != "" && idx.Refs[idx.Head] == "" {
		idx.Head = ""
	}
	for _, p := range idx.Packs {
		if !reHex32.MatchString(p.ID) || p.Parts < 1 || p.Parts > maxPackParts {
			return errorf("索引中包含非法的数据文件")
		}
		for i := 0; i < p.Parts; i++ {
			if _, ok := tree[partPath(p.ID, i)]; !ok {
				return errorf("存储分支缺少数据文件 %s", partPath(p.ID, i))
			}
		}
	}
	for fp, name := range idx.Names {
		if !reHex16.MatchString(fp) || !reNameOK.MatchString(name) {
			return errorf("索引中包含非法的成员名")
		}
	}
	return nil
}

func partPath(id string, i int) string  { return fmt.Sprintf("data/%s-%d.enc", id, i) }
func indexContext(repoID string) string { return "index|" + repoID }
func partContext(repoID, id string, i, n int) string {
	return fmt.Sprintf("data|%s|%s|%d|%d", repoID, id, i, n)
}

// Load fetches and fully verifies the remote. Local trust is updated only on success.
func (r *Repo) Load() (*Snapshot, error) {
	head, err := r.fetchStorage()
	if err != nil {
		return nil, err
	}
	st, err := r.loadState()
	if err != nil {
		return nil, err
	}
	if head == "" {
		if st.RepoID != "" {
			return nil, errorf("安全警告：远端存储分支不存在了（被删除？），已拒绝")
		}
		return &Snapshot{}, nil
	}
	s := &Snapshot{Commit: head}
	if s.Tree, err = r.readTree(head); err != nil {
		return nil, err
	}
	mSHA, ok1 := s.Tree[pathManifest]
	sigSHA, ok2 := s.Tree[pathSignature]
	iSHA, ok3 := s.Tree[pathIndex]
	if !ok1 || !ok2 || !ok3 {
		return nil, errorf("远端存储分支不完整")
	}
	if s.ManifestRaw, err = r.readBlob(mSHA); err != nil {
		return nil, err
	}
	sig, err := r.readBlob(sigSHA)
	if err != nil {
		return nil, err
	}
	if s.Manifest, err = verifyManifest(s.ManifestRaw, sig); err != nil {
		return nil, err
	}
	if err := r.checkTrust(st, s.Manifest, s.ManifestRaw); err != nil {
		return nil, err
	}
	k, ok := r.loadKey()
	if !ok || k.IDHex() != s.Manifest.KeyID {
		if k, err = r.unlockKey(s.Tree, s.Manifest); err != nil {
			return nil, err
		}
		if err := r.saveKey(k); err != nil {
			return nil, err
		}
	}
	s.Key = k
	ct, err := r.readBlob(iSHA)
	if err != nil {
		return nil, err
	}
	if s.IndexRaw, err = crypt.DecryptBytes(ct, k, "index", indexContext(s.Manifest.RepoID)); err != nil {
		return nil, errorf("索引解密失败: %v", err)
	}
	s.Index = &Index{}
	if err := json.Unmarshal(s.IndexRaw, s.Index); err != nil {
		return nil, errorf("索引格式错误")
	}
	if err := validateIndex(s.Index, s.Manifest, s.Tree); err != nil {
		return nil, err
	}
	ih := sha256Hex(s.IndexRaw)
	if st.RepoID != "" {
		if s.Index.Seq < st.IndexSeq {
			return nil, errorf("安全警告：远端索引版本回退（%d < %d），可能是回滚攻击", s.Index.Seq, st.IndexSeq)
		}
		if s.Index.Seq == st.IndexSeq && ih != st.IndexHash {
			return nil, errorf("安全警告：远端索引与本地记录不一致")
		}
	}
	st.URL, st.RepoID = r.URL, s.Manifest.RepoID
	st.ManifestVersion, st.ManifestHash, st.Members = s.Manifest.Version, sha256Hex(s.ManifestRaw), s.Manifest.Members
	st.IndexSeq, st.IndexHash = s.Index.Seq, ih
	return s, r.saveState(st)
}

// ---- objects ----

func refValues(refs map[string]string) []string {
	var v []string
	seen := map[string]bool{}
	for _, sha := range refs {
		if !seen[sha] {
			seen[sha] = true
			v = append(v, sha)
		}
	}
	sort.Strings(v)
	return v
}

func missingObjects(g Git, shas []string) ([]string, error) {
	have, err := g.ExistingObjects(shas)
	if err != nil {
		return nil, err
	}
	var miss []string
	for _, s := range shas {
		if !have[s] {
			miss = append(miss, s)
		}
	}
	return miss, nil
}

// EnsureObjects downloads and imports packs until the local repo has all given objects.
func (r *Repo) EnsureObjects(s *Snapshot, shas []string) error {
	miss, err := missingObjects(r.local, shas)
	if err != nil || len(miss) == 0 {
		return err
	}
	st, err := r.loadState()
	if err != nil {
		return err
	}
	imported := map[string]bool{}
	for _, id := range st.Imported {
		imported[id] = true
	}
	for pass := 0; pass < 2 && len(miss) > 0; pass++ {
		for _, p := range s.Index.Packs {
			if pass == 0 && imported[p.ID] {
				continue
			}
			if err := r.importPack(s, p); err != nil {
				return err
			}
			if !imported[p.ID] {
				imported[p.ID] = true
				st.Imported = append(st.Imported, p.ID)
			}
		}
		if miss, err = missingObjects(r.local, shas); err != nil {
			return err
		}
	}
	if err := r.saveState(st); err != nil {
		return err
	}
	if len(miss) > 0 {
		return errorf("远端数据不完整，缺少对象 %s", miss[0])
	}
	return nil
}

// decryptPack writes the plaintext pack to a temp file.
func (r *Repo) decryptPack(s *Snapshot, k crypt.Key, repoID string, p PackRef, tree map[string]string) (string, error) {
	out := r.tmpPath("pack")
	f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	for i := 0; i < p.Parts; i++ {
		var ct bytes.Buffer
		if _, err := r.cache.RunWith(gitOpts{stdout: &ct}, "cat-file", "blob", tree[partPath(p.ID, i)]); err != nil {
			os.Remove(out)
			return "", err
		}
		if err := crypt.Decrypt(f, &ct, k, "data", partContext(repoID, p.ID, i, p.Parts)); err != nil {
			f.Close()
			os.Remove(out)
			return "", errorf("数据文件解密失败: %v", err)
		}
	}
	return out, nil
}

func (r *Repo) importPack(s *Snapshot, p PackRef) error {
	path, err := r.decryptPack(s, s.Key, s.Manifest.RepoID, p, s.Tree)
	if err != nil {
		return err
	}
	defer os.Remove(path)
	return indexPack(r.local, path)
}

func indexPack(g Git, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = g.RunWith(gitOpts{stdin: f}, "index-pack", "--stdin", "--fix-thin")
	return err
}

// buildPack packs objects reachable from include but not from exclude.
// Returns "" if there is nothing to pack.
func (r *Repo) buildPack(include, exclude []string) (string, int64, error) {
	var in strings.Builder
	for _, s := range include {
		in.WriteString(s + "\n")
	}
	for _, s := range exclude {
		in.WriteString("^" + s + "\n")
	}
	out := r.tmpPath("newpack")
	f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, err
	}
	_, err = r.local.RunWith(gitOpts{stdin: strings.NewReader(in.String()), stdout: f}, "pack-objects", "--revs", "--stdout", "-q")
	f.Close()
	if err != nil {
		os.Remove(out)
		return "", 0, err
	}
	hdr := make([]byte, 12)
	rf, err := os.Open(out)
	if err != nil {
		return "", 0, err
	}
	_, err = io.ReadFull(rf, hdr)
	fi, _ := rf.Stat()
	rf.Close()
	if err != nil || string(hdr[:4]) != "PACK" {
		os.Remove(out)
		return "", 0, errorf("生成数据包失败")
	}
	if hdr[8]|hdr[9]|hdr[10]|hdr[11] == 0 {
		os.Remove(out)
		return "", 0, nil
	}
	return out, fi.Size(), nil
}

// storePack encrypts a plaintext pack into the cache repo, returning tree entries.
func (r *Repo) storePack(k crypt.Key, repoID, packFile string, size int64) (PackRef, map[string]string, error) {
	p := PackRef{ID: randHex(16), Size: size, Parts: int((size + partSize - 1) / partSize)}
	if p.Parts > maxPackParts {
		return p, nil, errorf("数据包过大")
	}
	entries := map[string]string{}
	src, err := os.Open(packFile)
	if err != nil {
		return p, nil, err
	}
	defer src.Close()
	for i := 0; i < p.Parts; i++ {
		encPath := r.tmpPath("part")
		ef, err := os.OpenFile(encPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return p, nil, err
		}
		err = crypt.Encrypt(ef, io.LimitReader(src, partSize), k, "data", partContext(repoID, p.ID, i, p.Parts))
		ef.Close()
		if err != nil {
			os.Remove(encPath)
			return p, nil, err
		}
		sha, err := r.writeBlobFile(encPath)
		os.Remove(encPath)
		if err != nil {
			return p, nil, err
		}
		entries[partPath(p.ID, i)] = sha
	}
	return p, entries, nil
}

// ---- writing the storage branch ----

func (r *Repo) commitTree(entries map[string]string, parent string) (string, error) {
	var info strings.Builder
	paths := make([]string, 0, len(entries))
	for p := range entries {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		fmt.Fprintf(&info, "100644 %s\t%s\n", entries[p], p)
	}
	idxFile := r.tmpPath("index")
	defer os.Remove(idxFile)
	g := Git{Dir: r.cache.Dir, Env: []string{"GIT_INDEX_FILE=" + idxFile}}
	if _, err := g.RunWith(gitOpts{stdin: strings.NewReader(info.String())}, "update-index", "--add", "--index-info"); err != nil {
		return "", err
	}
	tree, err := g.Run("write-tree")
	if err != nil {
		return "", err
	}
	args := []string{"commit-tree", "--no-gpg-sign", tree, "-m", "genc update"}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	// a fixed identity so the public storage history reveals nothing about authors
	bot := Git{Dir: r.cache.Dir, Env: []string{
		"GIT_AUTHOR_NAME=genc", "GIT_AUTHOR_EMAIL=genc@localhost",
		"GIT_COMMITTER_NAME=genc", "GIT_COMMITTER_EMAIL=genc@localhost",
	}}
	return bot.Run(args...)
}

// pushStorage publishes commit, succeeding only if the remote still points at expect.
func (r *Repo) pushStorage(commit, expect string) error {
	var out bytes.Buffer
	c := r.cache.cmd(gitOpts{stdout: &out}, "push", "--porcelain", "-q",
		"--force-with-lease="+storageRef+":"+expect, "--", r.URL, commit+":"+storageRef)
	c.Stderr = os.Stderr
	err := c.Run()
	if err != nil {
		s := out.String()
		// stale lease, non-fast-forward, or the remote losing a ref-lock race all mean
		// "someone else updated the storage branch first": reload and retry.
		for _, m := range []string{"[rejected]", "stale info", "fetch first", "cannot lock ref", "failed to update ref", "incorrect old value"} {
			if strings.Contains(s, m) {
				return errConflict
			}
		}
		return errorf("推送到远端失败: %v %s", err, strings.TrimSpace(s))
	}
	_, err = r.cache.Run("update-ref", storageRef, commit)
	return err
}

func (r *Repo) encryptIndex(k crypt.Key, idx *Index) ([]byte, string, error) {
	raw, err := json.Marshal(idx)
	if err != nil {
		return nil, "", err
	}
	ct, err := crypt.EncryptBytes(raw, k, "index", indexContext(idx.RepoID))
	if err != nil {
		return nil, "", err
	}
	sha, err := r.writeBlob(ct)
	return raw, sha, err
}

func signManifest(m *Manifest, id *Identity) ([]byte, []byte, error) {
	m.Signer = id.FP()
	m.Time = time.Now().UTC().Format(time.RFC3339)
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	sig, err := sshsigSign(id, raw)
	return raw, sig, err
}

func sshsigSign(id *Identity, msg []byte) ([]byte, error) {
	if id.signer == nil {
		return nil, errNeedPassphrase
	}
	return sshsig.Sign(id.signer, sigNamespace, msg)
}

func makeKeyBox(k crypt.Key, key string) ([]byte, error) {
	pub, err := ParseMemberKey(key)
	if err != nil {
		return nil, err
	}
	rcp, err := ageRecipient(pub)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	w, err := age.Encrypt(&out, rcp)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(k[:]); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func chooseHead(refs map[string]string, cur string) string {
	if cur != "" && refs[cur] != "" {
		return cur
	}
	for _, c := range []string{"refs/heads/main", "refs/heads/master"} {
		if refs[c] != "" {
			return c
		}
	}
	var heads []string
	for name := range refs {
		if strings.HasPrefix(name, "refs/heads/") {
			heads = append(heads, name)
		}
	}
	sort.Strings(heads)
	if len(heads) > 0 {
		return heads[0]
	}
	return ""
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

func (r *Repo) markImported(ids ...string) error {
	st, err := r.loadState()
	if err != nil {
		return err
	}
	st.Imported = append(st.Imported, ids...)
	return r.saveState(st)
}

// recordPublished updates local trust after we published a new state ourselves.
func (r *Repo) recordPublished(m *Manifest, mraw []byte, idxRaw []byte, idx *Index) error {
	st, err := r.loadState()
	if err != nil {
		return err
	}
	st.URL, st.RepoID = r.URL, m.RepoID
	st.ManifestVersion, st.ManifestHash, st.Members = m.Version, sha256Hex(mraw), m.Members
	st.IndexSeq, st.IndexHash = idx.Seq, sha256Hex(idxRaw)
	return r.saveState(st)
}
