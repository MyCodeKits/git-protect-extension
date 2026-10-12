package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// runHelper implements the git remote helper protocol (see `git help remote-helpers`).
// git runs it as: git-remote-protect <remote-name> <url>
func runHelper(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "git-protect: git-remote-protect 应由 git 调用，例如 git clone protect::https://github.com/xxx/repo.git")
		return 2
	}
	url := strings.TrimPrefix(args[1], "protect::")
	gitDir := os.Getenv("GIT_DIR")
	if gitDir == "" {
		fmt.Fprintln(os.Stderr, "git-protect: GIT_DIR not set")
		return 2
	}
	r, err := OpenRepo(gitDir, url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "git-protect:", err)
		return 1
	}
	if err := r.Lock(); err != nil {
		fmt.Fprintln(os.Stderr, "git-protect:", err)
		return 1
	}
	defer r.Unlock()
	h := &helper{r: r, in: bufio.NewReader(os.Stdin), out: bufio.NewWriter(os.Stdout)}
	if err := h.loop(); err != nil {
		h.out.Flush()
		fmt.Fprintln(os.Stderr, "git-protect:", err)
		return 1
	}
	return 0
}

type helper struct {
	r    *Repo
	in   *bufio.Reader
	out  *bufio.Writer
	snap *Snapshot
}

func (h *helper) readLine() (string, error) {
	line, err := h.in.ReadString('\n')
	if err == io.EOF && line != "" {
		err = nil
	}
	return strings.TrimRight(line, "\r\n"), err
}

func (h *helper) snapshot() (*Snapshot, error) {
	if h.snap != nil {
		return h.snap, nil
	}
	s, err := h.r.Load()
	if err != nil {
		return nil, err
	}
	if !s.Initialized() {
		return nil, errNotInitialized
	}
	installHook(h.r.local, true)
	h.snap = s
	return s, nil
}

func (h *helper) loop() error {
	for {
		line, err := h.readLine()
		if err == io.EOF || (err == nil && line == "") {
			return nil
		}
		if err != nil {
			return err
		}
		cmd := strings.Fields(line)
		switch cmd[0] {
		case "capabilities":
			fmt.Fprint(h.out, "option\nfetch\npush\n\n")
		case "option":
			fmt.Fprint(h.out, "unsupported\n")
		case "list":
			s, err := h.snapshot()
			if err != nil {
				return err
			}
			names := make([]string, 0, len(s.Index.Refs))
			for n := range s.Index.Refs {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				fmt.Fprintf(h.out, "%s %s\n", s.Index.Refs[n], n)
			}
			if s.Index.Head != "" {
				fmt.Fprintf(h.out, "@%s HEAD\n", s.Index.Head)
			}
			fmt.Fprint(h.out, "\n")
		case "fetch":
			var shas []string
			for {
				f := strings.Fields(line)
				if len(f) < 2 || f[0] != "fetch" || !isHexSHA(f[1]) {
					return fmt.Errorf("unexpected fetch command %q", line)
				}
				shas = append(shas, f[1])
				if line, err = h.readLine(); err != nil {
					return err
				}
				if line == "" {
					break
				}
			}
			s, err := h.snapshot()
			if err != nil {
				return err
			}
			if err := h.r.EnsureObjects(s, shas); err != nil {
				return err
			}
			fmt.Fprint(h.out, "\n")
		case "push":
			var cmds []PushCmd
			for {
				spec := strings.TrimPrefix(line, "push ")
				if spec == line {
					return fmt.Errorf("unexpected push command %q", line)
				}
				c := PushCmd{}
				if strings.HasPrefix(spec, "+") {
					c.Force, spec = true, spec[1:]
				}
				i := strings.LastIndexByte(spec, ':')
				if i < 0 {
					return fmt.Errorf("bad push spec %q", line)
				}
				c.Src, c.Dst = spec[:i], spec[i+1:]
				cmds = append(cmds, c)
				if line, err = h.readLine(); err != nil {
					return err
				}
				if line == "" {
					break
				}
			}
			s, err := h.snapshot()
			if err != nil {
				return err
			}
			h.snap = nil // state changes after a push
			res, err := h.r.Push(cmds, s)
			if err != nil {
				return err
			}
			for _, pr := range res {
				if pr.Err == "" {
					fmt.Fprintf(h.out, "ok %s\n", pr.Dst)
				} else {
					fmt.Fprintf(h.out, "error %s %s\n", pr.Dst, pr.Err)
				}
			}
			fmt.Fprint(h.out, "\n")
			if days, limit := keyAgeDays(s), rotateDays(h.r.local); days > limit {
				h.r.info("密钥已使用 %d 天，建议更换：运行 git protect rotate 或 git protect ui", days)
			}
		default:
			return fmt.Errorf("unsupported command %q", cmd[0])
		}
		if err := h.out.Flush(); err != nil {
			return err
		}
	}
}

// rotateDays is the key age after which pushes remind to rotate (git config protect.rotateDays).
func rotateDays(local Git) int {
	if v, err := (Git{Dir: local.Dir, Raw: true}).Run("config", "--type=int", "--get", "protect.rotateDays"); err == nil {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 180
}

const hookMarker = "# git-protect-pre-push"

// installHook adds a pre-push hook that blocks plaintext pushes from an encrypted repo.
// Returns a warning if an unrelated hook already exists.
func installHook(local Git, quiet bool) string {
	// ask git without our core.hooksPath override, so the user's configuration is honoured
	raw := Git{Dir: local.Dir, Raw: true}
	hooks, err := raw.Run("rev-parse", "--path-format=absolute", "--git-path", "hooks")
	if err != nil {
		return "无法定位 hooks 目录: " + err.Error()
	}
	path := filepath.Join(hooks, "pre-push")
	if b, err := os.ReadFile(path); err == nil {
		if strings.Contains(string(b), hookMarker) {
			return ""
		}
		msg := "已存在 pre-push 钩子 " + path + "，未安装明文推送保护。"
		if strings.Contains(string(b), "git lfs") || strings.Contains(string(b), "git-lfs") {
			msg += "检测到 Git LFS：LFS 文件会以明文上传，git-protect 不支持 LFS！"
		}
		if !quiet {
			fmt.Fprintln(os.Stderr, "git-protect: 警告："+msg)
		}
		return msg
	}
	os.MkdirAll(hooks, 0o755)
	script := "#!/bin/sh\n" + hookMarker + ": refuse to push this encrypted repository in plaintext\nexec git-protect hook pre-push \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		return "无法安装 pre-push 钩子: " + err.Error()
	}
	return ""
}
