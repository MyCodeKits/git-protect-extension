package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Environment variables that git sets for hooks/helpers and that would make
// our own git invocations operate on the wrong repository.
var gitEnvBlocklist = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_NAMESPACE",
	"GIT_PREFIX", "GIT_QUARANTINE_PATH", "GIT_INTERNAL_SUPER_PREFIX",
}

func cleanEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		blocked := false
		for _, b := range gitEnvBlocklist {
			if strings.EqualFold(name, b) {
				blocked = true
				break
			}
		}
		if !blocked {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

// Git runs git against one explicit repository.
type Git struct {
	Dir string   // value for --git-dir
	Env []string // extra environment
	Raw bool     // do not override user configuration (hooks, signing)
}

type gitOpts struct {
	stdin  io.Reader
	stdout io.Writer
	env    []string
}

func (g Git) cmd(o gitOpts, args ...string) *exec.Cmd {
	var full []string
	if !g.Raw {
		full = []string{
			// never run user hooks or sign anything in our storage repositories
			"-c", "core.hooksPath=" + filepath.Join(os.TempDir(), "genc-no-hooks-dir"),
			"-c", "commit.gpgSign=false", "-c", "tag.gpgSign=false", "-c", "push.gpgSign=false",
		}
	}
	if g.Dir != "" {
		full = append(full, "--git-dir="+g.Dir)
	}
	full = append(full, args...)
	c := exec.Command("git", full...)
	c.Env = cleanEnv(append(append([]string{}, g.Env...), o.env...)...)
	c.Stdin = o.stdin
	c.Stdout = o.stdout
	return c
}

// Run executes git, returning trimmed stdout; stderr is captured into the error.
func (g Git) Run(args ...string) (string, error) {
	return g.RunWith(gitOpts{}, args...)
}

func (g Git) RunWith(o gitOpts, args ...string) (string, error) {
	var out, errb bytes.Buffer
	if o.stdout == nil {
		o.stdout = &out
	}
	c := g.cmd(o, args...)
	c.Stderr = &errb
	if err := c.Run(); err != nil {
		return strings.TrimSpace(out.String()), fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// RunPassStderr lets git talk to the user (progress, credential prompts).
func (g Git) RunPassStderr(args ...string) error {
	c := g.cmd(gitOpts{stdout: os.Stderr}, args...)
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("git %s failed: %v", args[0], err)
	}
	return nil
}

// ExistingObjects returns which of the given object names exist in the repo.
func (g Git) ExistingObjects(shas []string) (map[string]bool, error) {
	res := map[string]bool{}
	if len(shas) == 0 {
		return res, nil
	}
	in := strings.Join(shas, "\n") + "\n"
	out, err := g.RunWith(gitOpts{stdin: strings.NewReader(in)}, "cat-file", "--batch-check=%(objectname) %(objecttype)")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] != "missing" {
			res[f[0]] = true
		}
	}
	return res, nil
}

func isHexSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
