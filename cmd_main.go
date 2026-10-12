package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"filippo.io/age"

	"git-protect/internal/crypt"
)

const version = "0.1.0"

func main() {
	base := strings.ToLower(strings.TrimSuffix(filepath.Base(os.Args[0]), filepath.Ext(os.Args[0])))
	if strings.HasPrefix(base, "git-remote-protect") {
		os.Exit(runHelper(os.Args[1:]))
	}
	os.Exit(runCLI(os.Args[1:]))
}

const usage = `git-protect ` + version + ` —— Git 仓库透明加密

用法: git protect <命令> [参数]（也可以直接运行 git-protect <命令>）

  init     --remote <地址> [--name 名字]   把当前仓库初始化为加密仓库
  clone    <地址> [目录]                   克隆（私钥有口令时使用；否则直接 git clone protect::<地址>）
  unlock                                  用本人私钥解锁（私钥有口令时需要）
  add      --key <公钥文件>|--github <用户名> [--name 名字]   添加成员
  remove   <名字|指纹>                     移除成员（自动更换密钥）
  rekey    <名字|指纹> --key <新公钥文件>    更换某成员的公钥
  rotate                                  更换密钥（全量重新加密，提交历史不变）
  gc                                      合并数据文件
  backup   [--out 文件]                    导出密钥备份
  restore  <备份文件> [--takeover]          从备份恢复密钥
  status | members | log | whoami          查看信息
  keygen   [--passphrase]                 生成成员密钥对
  doctor                                  环境检查
  trust    --reset                        清除本地信任记录（核对指纹后使用）
  ui                                      打开管理面板

日常 clone/commit/push/pull/diff/merge 直接使用 git 或 IDE。`

func runCLI(args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Println(usage)
		return 0
	}
	cmds := map[string]func([]string) error{
		"init": cmdInit, "clone": cmdClone, "unlock": cmdUnlock, "add": cmdAdd, "remove": cmdRemove,
		"rekey": cmdRekey, "rotate": cmdRotate, "gc": cmdGC, "backup": cmdBackup, "restore": cmdRestore,
		"status": cmdStatus, "members": cmdMembers, "log": cmdLog, "whoami": cmdWhoami,
		"keygen": cmdKeygen, "doctor": cmdDoctor, "trust": cmdTrust, "hook": cmdHook, "ui": cmdUI,
		"version": func([]string) error { fmt.Println("git-protect", version); return nil },
	}
	f, ok := cmds[args[0]]
	if !ok {
		fmt.Fprintf(os.Stderr, "未知命令 %q\n\n%s\n", args[0], usage)
		return 2
	}
	if err := f(args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "git-protect:", err)
		return 1
	}
	return 0
}

// ---- helpers ----

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("git-protect "+name, flag.ContinueOnError)
	return fs
}

func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	// allow flags after positional arguments
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func currentGitDir(dir string) (string, error) {
	args := []string{"rev-parse", "--absolute-git-dir"}
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	d, err := Git{Raw: true}.Run(args...)
	if err != nil {
		return "", errorf("当前目录不是 git 仓库")
	}
	return d, nil
}

// protectRemotes returns remote name -> storage url for remotes using protect::.
func protectRemotes(gitDir string) map[string]string {
	out, _ := Git{Dir: gitDir, Raw: true}.Run("config", "--get-regexp", `^remote\..*\.url$`)
	res := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.HasPrefix(f[1], "protect::") {
			name := strings.TrimSuffix(strings.TrimPrefix(f[0], "remote."), ".url")
			res[name] = strings.TrimPrefix(f[1], "protect::")
		}
	}
	return res
}

func openCLIRepo(dir string) (*Repo, error) {
	gitDir, err := currentGitDir(dir)
	if err != nil {
		return nil, err
	}
	remotes := protectRemotes(gitDir)
	if len(remotes) == 0 {
		return nil, errorf("当前仓库没有 protect:: 远程地址，请先运行 git protect init")
	}
	name := "origin"
	if _, ok := remotes[name]; !ok {
		if len(remotes) > 1 {
			return nil, errorf("有多个 git-protect 远程地址，请设置 origin")
		}
		for n := range remotes {
			name = n
		}
	}
	r, err := OpenRepo(gitDir, remotes[name])
	if err != nil {
		return nil, err
	}
	r.Pass = terminalPassphrase
	return r, nil
}

func withRepo(fn func(r *Repo) error) error {
	r, err := openCLIRepo("")
	if err != nil {
		return err
	}
	if err := r.Lock(); err != nil {
		return err
	}
	defer r.Unlock()
	return fn(r)
}

func confirm(prompt string) bool {
	fmt.Fprint(os.Stderr, prompt+" [y/N] ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

func resolveMember(s *Snapshot, who string) (string, error) {
	for _, m := range s.Manifest.Members {
		if m.FP == who || strings.HasPrefix(m.FP, who) && len(who) >= 6 || s.Index.Names[m.FP] == who {
			return m.FP, nil
		}
	}
	return "", errorf("找不到成员 %q（git protect members 查看）", who)
}

func memberName(s *Snapshot, fp string) string {
	if n := s.Index.Names[fp]; n != "" {
		return n
	}
	return fp
}

func readKeyArg(keyFile, keyText, githubUser string, pick int) (string, error) {
	switch {
	case keyFile != "":
		b, err := os.ReadFile(keyFile)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	case keyText != "":
		return keyText, nil
	case githubUser != "":
		cl := &http.Client{Timeout: 20 * time.Second}
		resp, err := cl.Get("https://github.com/" + githubUser + ".keys")
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return "", errorf("读取 GitHub 公钥失败: %s", resp.Status)
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var keys []string
		for _, l := range strings.Split(string(b), "\n") {
			if _, err := ParseMemberKey(l); err == nil {
				keys = append(keys, strings.TrimSpace(l))
			}
		}
		if len(keys) == 0 {
			return "", errorf("%s 在 GitHub 上没有可用的 ssh-ed25519 / ssh-rsa(≥3072) 公钥", githubUser)
		}
		if len(keys) > 1 && pick == 0 {
			for i, k := range keys {
				pub, _ := ParseMemberKey(k)
				fmt.Fprintf(os.Stderr, "  %d) %s %s\n", i+1, pub.Type(), Fingerprint(pub))
			}
			return "", errorf("%s 有多把公钥，请用 --pick N 选择", githubUser)
		}
		if pick < 1 {
			pick = 1
		}
		if pick > len(keys) {
			return "", errorf("--pick 超出范围")
		}
		return keys[pick-1], nil
	}
	return "", errorf("请用 --key <公钥文件>、--key-text 或 --github <用户名> 指定公钥")
}

// ---- commands ----

func cmdInit(args []string) error {
	fs := newFlags("init")
	remote := fs.String("remote", "", "存储仓库地址，例如 https://github.com/xxx/repo.git")
	name := fs.String("name", "", "你的显示名")
	remoteName := fs.String("remote-name", "origin", "本地远程名")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	if *remote == "" {
		return errorf("请指定 --remote <地址>")
	}
	url := strings.TrimPrefix(*remote, "protect::")
	gitDir, err := currentGitDir("")
	if err != nil {
		return err
	}
	r, err := OpenRepo(gitDir, url)
	if err != nil {
		return err
	}
	r.Pass = terminalPassphrase
	if st, _ := r.loadState(); st != nil && st.RepoID != "" {
		return errorf("此仓库已经初始化过 git-protect")
	}
	if err := r.Lock(); err != nil {
		return err
	}
	defer r.Unlock()
	me, err := Init(r, *name)
	if err != nil {
		return err
	}
	raw := Git{Dir: gitDir, Raw: true}
	if old, err := raw.Run("config", "--get", "remote."+*remoteName+".url"); err == nil {
		raw.Run("remote", "set-url", *remoteName, "protect::"+url)
		fmt.Printf("远程 %s: %s → protect::%s\n", *remoteName, old, url)
	} else if _, err := raw.Run("remote", "add", *remoteName, "protect::"+url); err != nil {
		return err
	}
	if w := installHook(r.local, false); w == "" {
		fmt.Println("已安装 pre-push 保护钩子（阻止明文推送）")
	}
	fmt.Printf("已初始化加密仓库。你的指纹：%s\n", me.FP())
	fmt.Println("下一步：git push -u " + *remoteName + " --all")
	fmt.Println("重要：请立即运行 git protect backup 备份密钥。所有成员私钥丢失且无备份 = 代码永久无法解密。")
	return nil
}

func cmdClone(args []string) error {
	fs := newFlags("clone")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return errorf("用法: git protect clone <地址> [目录]")
	}
	url := strings.TrimPrefix(pos[0], "protect::")
	dir := strings.TrimSuffix(filepath.Base(strings.TrimRight(url, "/")), ".git")
	if len(pos) > 1 {
		dir = pos[1]
	}
	if _, err := os.Stat(dir); err == nil {
		return errorf("目录 %s 已存在", dir)
	}
	raw := Git{Raw: true}
	if _, err := raw.Run("init", "-q", dir); err != nil {
		return err
	}
	if _, err := raw.Run("-C", dir, "remote", "add", "origin", "protect::"+url); err != nil {
		return err
	}
	r, err := openCLIRepo(dir)
	if err != nil {
		return err
	}
	if err := r.Lock(); err != nil {
		return err
	}
	s, err := r.Load()
	r.Unlock()
	if err != nil {
		return err
	}
	if !s.Initialized() {
		return errNotInitialized
	}
	fetch := exec.Command("git", "-C", dir, "fetch", "origin")
	fetch.Stdout, fetch.Stderr = os.Stdout, os.Stderr
	if err := fetch.Run(); err != nil {
		return errorf("git fetch 失败")
	}
	if h := s.Index.Head; strings.HasPrefix(h, "refs/heads/") {
		branch := strings.TrimPrefix(h, "refs/heads/")
		raw.Run("-C", dir, "remote", "set-head", "origin", branch)
		co := exec.Command("git", "-C", dir, "checkout", "-q", "-B", branch, "--track", "origin/"+branch)
		co.Stdout, co.Stderr = os.Stdout, os.Stderr
		if err := co.Run(); err != nil {
			return errorf("检出失败")
		}
	}
	fmt.Printf("已克隆到 %s\n", dir)
	return nil
}

func cmdUnlock(args []string) error {
	return withRepo(func(r *Repo) error {
		s, err := r.Load()
		if err != nil {
			return err
		}
		if !s.Initialized() {
			return errNotInitialized
		}
		fmt.Println("已解锁。")
		return nil
	})
}

func cmdAdd(args []string) error {
	fs := newFlags("add")
	keyFile := fs.String("key", "", "成员公钥文件（.pub）")
	keyText := fs.String("key-text", "", "成员公钥文本")
	gh := fs.String("github", "", "从 GitHub 读取该用户的公钥")
	pick := fs.Int("pick", 0, "GitHub 用户有多把公钥时选择第几把")
	name := fs.String("name", "", "显示名")
	yes := fs.Bool("yes", false, "跳过确认（仅在已核对指纹时使用）")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	key, err := readKeyArg(*keyFile, *keyText, *gh, *pick)
	if err != nil {
		return err
	}
	pub, err := ParseMemberKey(key)
	if err != nil {
		return err
	}
	if *name == "" {
		*name = *gh
	}
	if *name != "" && !reNameOK.MatchString(*name) {
		return errorf("名字无效")
	}
	fp := Fingerprint(pub)
	fmt.Printf("将添加成员 %s，指纹 %s\n", *name, fp)
	if !*yes && !confirm("请通过电话/当面与对方核对指纹，确认添加？") {
		return errorf("已取消")
	}
	return withRepo(func(r *Repo) error {
		_, err := r.Admin(AdminOp{Add: &Member{FP: fp, Key: AuthorizedKey(pub)}, AddName: *name,
			Op: "add " + fp, SignerPass: r.Pass})
		if err == nil {
			fmt.Println("已添加。对方现在可以 git clone protect::" + r.URL)
			fmt.Println("请告诉对方核对首次连接时显示的签名人指纹。")
		}
		return err
	})
}

func cmdRemove(args []string) error {
	fs := newFlags("remove")
	yes := fs.Bool("yes", false, "跳过确认")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errorf("用法: git protect remove <名字|指纹>")
	}
	return withRepo(func(r *Repo) error {
		s, err := r.Load()
		if err != nil {
			return err
		}
		if !s.Initialized() {
			return errNotInitialized
		}
		fp, err := resolveMember(s, pos[0])
		if err != nil {
			return err
		}
		fmt.Printf("将移除成员 %s（%s），并更换密钥、用新密钥重新加密全部数据。\n", memberName(s, fp), fp)
		if !*yes && !confirm("确认？") {
			return errorf("已取消")
		}
		if _, err := r.Admin(AdminOp{Remove: fp, NewKey: true, Op: "remove " + fp, SignerPass: r.Pass}); err != nil {
			return err
		}
		fmt.Println("已移除并更换密钥。提交历史未改变。")
		fmt.Println("重要：请同时在 GitHub 上收回他对该仓库的写权限。请运行 git protect backup 备份新密钥。")
		return nil
	})
}

func cmdRekey(args []string) error {
	fs := newFlags("rekey")
	keyFile := fs.String("key", "", "新公钥文件")
	keyText := fs.String("key-text", "", "新公钥文本")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errorf("用法: git protect rekey <名字|指纹> --key <新公钥文件>")
	}
	key, err := readKeyArg(*keyFile, *keyText, "", 0)
	if err != nil {
		return err
	}
	pub, err := ParseMemberKey(key)
	if err != nil {
		return err
	}
	return withRepo(func(r *Repo) error {
		s, err := r.Load()
		if err != nil {
			return err
		}
		fp, err := resolveMember(s, pos[0])
		if err != nil {
			return err
		}
		_, err = r.Admin(AdminOp{Add: &Member{FP: Fingerprint(pub), Key: AuthorizedKey(pub)}, Rekey: fp,
			Op: "rekey " + fp + " -> " + Fingerprint(pub), SignerPass: r.Pass})
		if err == nil {
			fmt.Printf("已更换 %s 的公钥，新指纹 %s\n", memberName(s, fp), Fingerprint(pub))
		}
		return err
	})
}

func cmdRotate(args []string) error {
	fs := newFlags("rotate")
	yes := fs.Bool("yes", false, "跳过确认")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	if !*yes && !confirm("将生成新密钥，并用新密钥重新加密全部数据后覆盖远端存储分支（提交历史不变）。确认？") {
		return errorf("已取消")
	}
	return withRepo(func(r *Repo) error {
		if _, err := r.Admin(AdminOp{NewKey: true, Op: "rotate", SignerPass: r.Pass}); err != nil {
			return err
		}
		fmt.Println("已更换密钥。其他成员下次 pull 时会自动获取新密钥。")
		fmt.Println("重要：请运行 git protect backup 备份新密钥。")
		return nil
	})
}

func cmdGC(args []string) error {
	return withRepo(func(r *Repo) error {
		if _, err := r.Admin(AdminOp{Repack: true, Op: "gc", SignerPass: r.Pass}); err != nil {
			return err
		}
		fmt.Println("已合并数据文件。")
		return nil
	})
}

type backupFile struct {
	Format int    `json:"format"`
	RepoID string `json:"repo_id"`
	URL    string `json:"url"`
	Key    string `json:"key"`
	Time   string `json:"time"`
}

func newRecoveryPassphrase() string {
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	b := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	var parts []string
	for i := 0; i < len(b); i += 4 {
		parts = append(parts, b[i:min(i+4, len(b))])
	}
	return strings.Join(parts, "-")
}

func writeBackup(r *Repo, s *Snapshot, out string) (string, string, error) {
	pass := newRecoveryPassphrase()
	rcp, err := age.NewScryptRecipient(pass)
	if err != nil {
		return "", "", err
	}
	payload, _ := json.Marshal(backupFile{Format: 1, RepoID: s.Manifest.RepoID, URL: r.URL,
		Key: fmt.Sprintf("%x", s.Key[:]), Time: nowUTC()})
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, rcp)
	if err != nil {
		return "", "", err
	}
	w.Write(payload)
	if err := w.Close(); err != nil {
		return "", "", err
	}
	if out == "" {
		out = "git-protect-recovery-" + s.Manifest.RepoID[:8] + "-" + time.Now().Format("20060102") + ".age"
	}
	if err := writePrivateFile(out, buf.Bytes()); err != nil {
		return "", "", err
	}
	abs, _ := filepath.Abs(out)
	return abs, pass, nil
}

func cmdBackup(args []string) error {
	fs := newFlags("backup")
	out := fs.String("out", "", "输出文件")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	return withRepo(func(r *Repo) error {
		s, err := r.Load()
		if err != nil {
			return err
		}
		if !s.Initialized() {
			return errNotInitialized
		}
		path, pass, err := writeBackup(r, s, *out)
		if err != nil {
			return err
		}
		fmt.Println("备份文件：" + path)
		fmt.Println("恢复口令：" + pass)
		fmt.Println("请把备份文件和口令分开离线保存。拿到两者的人可以解密全部代码。")
		fmt.Println("注意：更换密钥后旧备份失效，需要重新备份。")
		return nil
	})
}

func cmdRestore(args []string) error {
	fs := newFlags("restore")
	takeover := fs.Bool("takeover", false, "把本机私钥加为成员（所有成员私钥都丢失时使用）")
	passStdin := fs.Bool("passphrase-stdin", false, "从标准输入读取恢复口令")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errorf("用法: git protect restore <备份文件>（在已添加 protect:: 远程地址的仓库中运行）")
	}
	data, err := os.ReadFile(pos[0])
	if err != nil {
		return err
	}
	var pass string
	if *passStdin {
		l, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		pass = strings.TrimSpace(l)
	} else {
		p, err := terminalPassphrase("恢复口令")
		if err != nil {
			return err
		}
		pass = strings.TrimSpace(string(p))
	}
	id, err := age.NewScryptIdentity(pass)
	if err != nil {
		return err
	}
	rd, err := age.Decrypt(bytes.NewReader(data), id)
	if err != nil {
		return errorf("口令错误或备份文件损坏")
	}
	var b backupFile
	if err := json.NewDecoder(io.LimitReader(rd, 1<<16)).Decode(&b); err != nil {
		return errorf("备份文件格式错误")
	}
	key, err := crypt.ParseKeyHex(b.Key)
	if err != nil {
		return err
	}
	return withRepo(func(r *Repo) error {
		if _, err := r.fetchStorage(); err != nil {
			return err
		}
		tree, err := r.readTree(storageRef)
		if err != nil {
			return errNotInitialized
		}
		mraw, err := r.readBlob(tree[pathManifest])
		if err != nil {
			return err
		}
		sig, err := r.readBlob(tree[pathSignature])
		if err != nil {
			return err
		}
		m, err := verifyManifest(mraw, sig)
		if err != nil {
			return err
		}
		if m.RepoID != b.RepoID {
			return errorf("备份不属于这个仓库")
		}
		if m.KeyID != key.IDHex() {
			return errorf("备份的密钥已过期（仓库之后更换过密钥）")
		}
		if err := r.saveKey(key); err != nil {
			return err
		}
		if _, err := r.Load(); err != nil {
			return err
		}
		fmt.Println("已恢复密钥，可以正常 pull/push。")
		if *takeover {
			ids := FindIdentities()
			if len(ids) == 0 {
				return errorf("没有找到成员私钥，请先运行 git protect keygen")
			}
			me := ids[0]
			_, err := r.Admin(AdminOp{Add: &Member{FP: me.FP(), Key: AuthorizedKey(me.Pub)}, Takeover: true,
				NewKey: true, Op: "takeover " + me.FP(), SignerPass: r.Pass})
			if err != nil {
				return err
			}
			fmt.Println("已把你的私钥加为成员并更换密钥。其他成员的客户端会拒绝这次变更，需在核对你的指纹后运行 git protect trust --reset。")
		}
		return nil
	})
}

func keyAgeDays(s *Snapshot) int {
	t, err := time.Parse(time.RFC3339, s.Index.KeyCreated)
	if err != nil {
		return 0
	}
	return int(time.Since(t).Hours() / 24)
}

func cmdStatus(args []string) error {
	return withRepo(func(r *Repo) error {
		s, err := r.Load()
		if err != nil {
			return err
		}
		if !s.Initialized() {
			return errNotInitialized
		}
		fmt.Printf("远端：%s\n仓库 ID：%s\n状态：已加密，已解锁\n", r.URL, s.Manifest.RepoID)
		fmt.Printf("密钥 ID：%s（已使用 %d 天）\n", s.Manifest.KeyID, keyAgeDays(s))
		fmt.Printf("成员：%d 人，分支/标签：%d 个，数据文件：%d 个\n", len(s.Manifest.Members), len(s.Index.Refs), len(s.Index.Packs))
		if len(s.Index.Packs) > 200 {
			fmt.Println("提示：数据文件较多，建议运行 git protect gc")
		}
		return nil
	})
}

func cmdMembers(args []string) error {
	return withRepo(func(r *Repo) error {
		s, err := r.Load()
		if err != nil {
			return err
		}
		mine := map[string]bool{}
		for _, id := range FindIdentities() {
			mine[id.FP()] = true
		}
		for _, m := range s.Manifest.Members {
			tag := ""
			if mine[m.FP] {
				tag = "（你）"
			}
			fmt.Printf("%s  %s%s\n", m.FP, memberName(s, m.FP), tag)
		}
		return nil
	})
}

func cmdLog(args []string) error {
	return withRepo(func(r *Repo) error {
		s, err := r.Load()
		if err != nil {
			return err
		}
		for _, e := range s.Index.Log {
			fmt.Printf("%s  %-16s  %s\n", e.Time, memberName(s, e.By), e.Op)
		}
		return nil
	})
}

func cmdWhoami(args []string) error {
	ids := FindIdentities()
	if len(ids) == 0 {
		return errorf("没有找到成员私钥，请运行 git protect keygen")
	}
	for _, id := range ids {
		lock := ""
		if id.NeedsPassphrase() {
			lock = "（有口令）"
		}
		fmt.Printf("指纹 %s  %s%s\n公钥 %s\n\n", id.FP(), id.Path, lock, AuthorizedKey(id.Pub))
	}
	return nil
}

func cmdKeygen(args []string) error {
	fs := newFlags("keygen")
	withPass := fs.Bool("passphrase", false, "为私钥设置口令")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	var pass []byte
	if *withPass {
		p, err := terminalPassphrase("新私钥")
		if err != nil {
			return err
		}
		pass = p
	}
	path, err := GenerateIdentity(pass)
	if err != nil {
		return err
	}
	fmt.Println("已生成 " + path)
	return cmdWhoami(nil)
}

func cmdTrust(args []string) error {
	fs := newFlags("trust")
	reset := fs.Bool("reset", false, "清除信任记录")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	if !*reset {
		return errorf("用法: git protect trust --reset")
	}
	return withRepo(func(r *Repo) error {
		st, err := r.loadState()
		if err != nil {
			return err
		}
		*st = LocalState{Imported: st.Imported}
		os.Remove(filepath.Join(r.dir, "key"))
		if err := r.saveState(st); err != nil {
			return err
		}
		fmt.Println("已清除信任记录。下次连接时会重新显示签名人指纹，请核对。")
		return nil
	})
}

// cmdHook is called by the pre-push hook: git-protect hook pre-push <remote> <url>
func cmdHook(args []string) error {
	if len(args) < 3 || args[0] != "pre-push" {
		return nil
	}
	url := args[2]
	io.Copy(io.Discard, os.Stdin)
	if strings.HasPrefix(url, "protect::") {
		return nil
	}
	gitDir, err := currentGitDir("")
	if err == nil {
		allowed, _ := Git{Dir: gitDir, Raw: true}.Run("config", "--get-all", "protect.allowPlain")
		for _, a := range strings.Split(allowed, "\n") {
			if strings.TrimSpace(a) == url {
				return nil
			}
		}
	}
	return errorf("已阻止：这是 git-protect 加密仓库，禁止以明文推送到 %s。\n"+
		"  请使用 protect:: 远程地址。确需明文推送到内部地址，运行: git config --add protect.allowPlain <地址>", url)
}

func cmdDoctor(args []string) error {
	ok := true
	check := func(good bool, msg, fix string) {
		mark := "✔"
		if !good {
			mark, ok = "✘", false
		}
		fmt.Printf("%s %s\n", mark, msg)
		if !good && fix != "" {
			fmt.Println("    → " + fix)
		}
	}
	v, err := Git{Raw: true}.Run("--version")
	check(err == nil, "git: "+v, "请安装 Git ≥ 2.31")
	p, err := exec.LookPath("git-remote-protect")
	check(err == nil, "git-remote-protect: "+p, "把 git-remote-protect 所在目录加入 PATH，然后重启 IDE")
	g, err := exec.LookPath("git-protect")
	check(err == nil, "git-protect: "+g, "把 git-protect 所在目录加入 PATH（pre-push 保护钩子需要）")
	for _, extra := range bundledGits() {
		fmt.Printf("• 发现其他 git：%s（与系统共用 PATH，可以找到 helper）\n", extra)
	}
	ids := FindIdentities()
	check(len(ids) > 0, fmt.Sprintf("成员私钥：%d 把", len(ids)), "运行 git protect keygen")
	for _, id := range ids {
		note := ""
		if id.NeedsPassphrase() {
			note = "（有口令：clone 时请用 git protect clone，或先运行 git protect unlock）"
		}
		fmt.Printf("    %s  %s%s\n", id.FP(), id.Path, note)
	}
	if gitDir, err := currentGitDir(""); err == nil && len(protectRemotes(gitDir)) > 0 {
		w := installHook(Git{Dir: gitDir}, true)
		check(w == "", "pre-push 明文推送保护钩子", w)
		_, lfsErr := os.Stat(filepath.Join(gitDir, "lfs"))
		check(os.IsNotExist(lfsErr), "未使用 Git LFS", "git-protect 不支持 LFS：LFS 文件会以明文上传")
	}
	if !ok {
		return errorf("发现问题，请按提示处理")
	}
	return nil
}

func bundledGits() []string {
	if runtime.GOOS != "windows" {
		return nil
	}
	var res []string
	for _, root := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")} {
		if root == "" {
			continue
		}
		m, _ := filepath.Glob(filepath.Join(root, "Microsoft Visual Studio", "*", "*", "Common7", "IDE", "CommonExtensions", "Microsoft", "TeamFoundation", "Team Explorer", "Git", "cmd", "git.exe"))
		res = append(res, m...)
	}
	sort.Strings(res)
	return res
}
