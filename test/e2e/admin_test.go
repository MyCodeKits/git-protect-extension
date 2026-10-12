package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// panel is a running `genc ui` process.
type panel struct {
	t     *testing.T
	base  string // http://127.0.0.1:port
	token string
}

func startPanel(t *testing.T, u *user, repo string) *panel {
	c := u.cmd(repo, "genc", "ui", "--no-browser")
	stdout, _ := c.StdoutPipe()
	c.Stderr = os.Stderr
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// give a normal "quit" time to exit cleanly (so coverage data is flushed), then kill
		done := make(chan struct{})
		go func() { c.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			c.Process.Kill()
			<-done
		}
	})
	lines := make(chan string)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	re := regexp.MustCompile(`(http://127\.0\.0\.1:\d+)/\?t=([0-9a-f]+)`)
	select {
	case l := <-lines:
		m := re.FindStringSubmatch(l)
		if m == nil {
			t.Fatalf("unexpected panel output %q", l)
		}
		go func() {
			for range lines {
			}
		}()
		return &panel{t: t, base: m[1], token: m[2]}
	case <-time.After(30 * time.Second):
		t.Fatal("panel did not start")
	}
	return nil
}

func (p *panel) raw(method, path, host, token, ctype, body string) int {
	req, _ := http.NewRequest(method, p.base+path, strings.NewReader(body))
	if host != "" {
		req.Host = host
	}
	if token != "" {
		req.Header.Set("X-Genc-Token", token)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		p.t.Fatal("panel must never send CORS headers")
	}
	return resp.StatusCode
}

// call performs an API operation; returns the decoded result and the error text.
func (p *panel) call(op string, req map[string]string) (map[string]any, string) {
	body, _ := json.Marshal(req)
	r, _ := http.NewRequest("POST", p.base+"/api/"+op, bytes.NewReader(body))
	r.Header.Set("X-Genc-Token", p.token)
	r.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		p.t.Fatal(err)
	}
	defer resp.Body.Close()
	var res map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		p.t.Fatalf("%s: bad response: %v", op, err)
	}
	if e, ok := res["error"].(string); ok {
		return res, e
	}
	return res, ""
}

func (p *panel) must(op string, req map[string]string) map[string]any {
	p.t.Helper()
	res, e := p.call(op, req)
	if e != "" {
		p.t.Fatalf("panel %s: %s", op, e)
	}
	return res
}

func TestPanelGuard(t *testing.T) {
	e := newEnv(t)
	alice := e.user("alice")
	wa := filepath.Join(e.root, "wa")
	alice.ok(e.root, "git", "init", "-q", wa)
	alice.ok(wa, "genc", "init", "--remote", e.remote)
	p := startPanel(t, alice, wa)
	host := strings.TrimPrefix(p.base, "http://")
	cases := []struct {
		name, method, path, host, token, ctype string
		want                                   int
	}{
		{"page without token", "GET", "/", "", "", "", 403},
		{"page wrong token", "GET", "/?t=bad", "", "", "", 403},
		{"page ok", "GET", "/?t=" + url.QueryEscape(p.token), "", "", "", 200},
		{"dns rebinding host", "GET", "/?t=" + p.token, "evil.example:" + strings.Split(host, ":")[1], "", "", 403},
		{"api without token", "POST", "/api/status", "", "", "application/json", 403},
		{"api wrong token", "POST", "/api/status", "", "bad", "application/json", 403},
		{"api form post (CSRF)", "POST", "/api/status", "", p.token, "text/plain", 403},
		{"api GET", "GET", "/api/status", "", p.token, "application/json", 403},
		{"api ok", "POST", "/api/status", "", p.token, "application/json", 200},
	}
	for _, c := range cases {
		if got := p.raw(c.method, c.path, c.host, c.token, c.ctype, "{}"); got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
	p.must("quit", nil)
}

func TestPassphraseKeysAndPanelAdmin(t *testing.T) {
	e := newEnv(t)
	alice := e.user("alice")
	url := "genc::" + e.remote

	wa := filepath.Join(e.root, "wa")
	alice.ok(e.root, "git", "init", "-q", wa)
	write(t, filepath.Join(wa, "f.txt"), "SECRET_CONTENT_7f3a\n")
	alice.ok(wa, "git", "add", ".")
	alice.ok(wa, "git", "commit", "-q", "-m", "SECRET_MSG_91c2")
	alice.ok(wa, "genc", "init", "--remote", e.remote, "--name", "alice")
	alice.ok(wa, "git", "push", "-q", "-u", "origin", "main")

	// erin's private key is protected by a passphrase
	erin := e.newUser("erin")
	writeProtectedKey(t, erin.keyPath(), "erin-pass")
	if !strings.Contains(erin.ok(e.root, "genc", "whoami"), "有口令") {
		t.Fatal("whoami should report the passphrase")
	}
	alice.ok(wa, "genc", "add", "--key", erin.pub(), "--name", "erin", "--yes")
	alice.fail(wa, "已经是成员", "genc", "add", "--key", erin.pub(), "--yes")

	// inside the git helper no prompt is possible: a clear message, never a hang
	erin.fail(e.root, "需要解锁", "git", "clone", "-q", url, filepath.Join(e.root, "we-fail"))

	// erin unlocks in the management panel
	we := filepath.Join(e.root, "we")
	erin.ok(e.root, "git", "init", "-q", we)
	erin.ok(we, "git", "remote", "add", "origin", url)
	p := startPanel(t, erin, we)
	if _, msg := p.call("unlock", map[string]string{"passphrase": "wrong"}); msg == "" {
		t.Fatal("wrong passphrase accepted")
	}
	st := p.must("unlock", map[string]string{"passphrase": "erin-pass"})
	if n := len(st["members"].([]any)); n != 2 {
		t.Fatalf("members = %d", n)
	}
	erin.ok(we, "git", "fetch", "-q", "origin") // key is cached: no passphrase needed any more
	erin.ok(we, "git", "checkout", "-q", "-b", "main", "origin/main")
	if b, _ := os.ReadFile(filepath.Join(we, "f.txt")); string(b) != "SECRET_CONTENT_7f3a\n" {
		t.Fatal("erin: wrong content")
	}

	// admin operations from the panel are signed with erin's protected key
	frank := e.user("frank")
	if _, msg := p.call("add", map[string]string{"key": frank.pubLine(), "name": "frank"}); msg == "" {
		t.Fatal("signing without the passphrase must fail")
	}
	fp := p.must("fingerprint", map[string]string{"key": frank.pubLine()})["fp"].(string)
	pub, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(frank.pubLine()))
	if fp != fingerprint(pub) {
		t.Fatal("fingerprint mismatch")
	}
	p.must("add", map[string]string{"key": frank.pubLine(), "name": "frank", "passphrase": "erin-pass"})
	wf := filepath.Join(e.root, "wf")
	frank.ok(e.root, "git", "clone", "-q", url, wf)
	p.must("rotate", map[string]string{"passphrase": "erin-pass"})
	frank.ok(wf, "git", "pull", "-q")
	p.must("remove", map[string]string{"fp": fp, "passphrase": "erin-pass"})
	frank.fail(wf, "不是该仓库的成员", "git", "pull", "-q")
	b := p.must("backup", nil)
	if b["passphrase"] == "" {
		t.Fatal("backup without passphrase")
	}
	os.Remove(b["path"].(string))
	p.must("quit", nil)

	// operation log, rekey, trust reset, doctor, status
	log := alice.ok(wa, "genc", "log")
	for _, op := range []string{"init", "add", "rotate", "remove"} {
		if !strings.Contains(log, op) {
			t.Fatalf("log missing %q:\n%s", op, log)
		}
	}
	// changing your own key: rekey by yourself is refused; add the new key, then remove the old one
	alice2 := e.user("alice2")
	alice.fail(wa, "不能用旧私钥更换自己的公钥", "genc", "rekey", "alice", "--key", alice2.pub())
	alice.ok(wa, "genc", "add", "--key", alice2.pub(), "--name", "alice-new", "--yes")
	alice2.ok(wa, "genc", "remove", "alice", "--yes")
	alice2.ok(e.root, "git", "clone", "-q", url, filepath.Join(e.root, "wa2"))
	alice.fail(e.root, "不是该仓库的成员", "git", "clone", "-q", url, filepath.Join(e.root, "wa-old"))
	// rekey of another member
	gina := e.user("gina")
	gina2 := e.user("gina2")
	alice2.ok(wa, "genc", "add", "--key", gina.pub(), "--name", "gina", "--yes")
	alice2.ok(wa, "genc", "rekey", "gina", "--key", gina2.pub())
	gina2.ok(e.root, "git", "clone", "-q", url, filepath.Join(e.root, "wg"))
	gina.fail(e.root, "不是该仓库的成员", "git", "clone", "-q", url, filepath.Join(e.root, "wg-old"))

	if out := alice2.ok(wa, "genc", "trust", "--reset"); !strings.Contains(out, "已清除") {
		t.Fatal(out)
	}
	alice2.ok(wa, "git", "fetch", "-q") // trust on first use again, unlock with the new key
	alice2.run(wa, "genc", "doctor")
	alice2.ok(wa, "genc", "status")
	e.assertNoLeak("admin")
}
