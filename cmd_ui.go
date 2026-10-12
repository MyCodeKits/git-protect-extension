package main

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

//go:embed ui.html
var uiPage string

type uiServer struct {
	dir    string // repository directory
	token  string
	host   string
	mu     sync.Mutex
	last   time.Time
	cancel context.CancelFunc
}

func cmdUI(args []string) error {
	fs := newFlags("ui")
	repoDir := fs.String("repo", "", "仓库目录（默认当前目录）")
	noOpen := fs.Bool("no-browser", false, "不自动打开浏览器")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	if _, err := openCLIRepo(*repoDir); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	u := &uiServer{dir: *repoDir, token: randHex(32), host: ln.Addr().String(), last: time.Now(), cancel: cancel}
	mux := http.NewServeMux()
	mux.HandleFunc("/", u.page)
	mux.HandleFunc("/api/", u.api)
	srv := &http.Server{Handler: u.guard(mux), ReadHeaderTimeout: 10 * time.Second}
	url := fmt.Sprintf("http://%s/?t=%s", u.host, u.token)
	fmt.Println("genc 管理面板：" + url)
	fmt.Println("关闭页面中的\"退出\"按钮或按 Ctrl+C 结束。")
	if !*noOpen {
		openBrowser(url)
	}
	go func() {
		for ctx.Err() == nil {
			time.Sleep(time.Minute)
			u.mu.Lock()
			idle := time.Since(u.last) > 30*time.Minute
			u.mu.Unlock()
			if idle {
				cancel()
			}
		}
	}()
	go func() { <-ctx.Done(); srv.Close() }()
	if err := srv.Serve(ln); err != http.ErrServerClosed {
		return err
	}
	return nil
}

// guard rejects anything that is not a same-origin request from our own page:
// the Host header must match (defeats DNS rebinding) and API calls need the token header
// (which cross-origin pages cannot send without a CORS preflight we never answer).
func (u *uiServer) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != u.host {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'")
		u.mu.Lock()
		u.last = time.Now()
		u.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func (u *uiServer) tokenOK(t string) bool {
	return subtle.ConstantTimeCompare([]byte(t), []byte(u.token)) == 1
}

func (u *uiServer) page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" || !u.tokenOK(r.URL.Query().Get("t")) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, strings.Replace(uiPage, "{{TOKEN}}", html.EscapeString(u.token), 1))
}

type uiReq struct {
	Passphrase string `json:"passphrase"`
	Key        string `json:"key"`
	Name       string `json:"name"`
	FP         string `json:"fp"`
}

func (u *uiServer) api(w http.ResponseWriter, r *http.Request) {
	if !u.tokenOK(r.Header.Get("X-Genc-Token")) || r.Method != http.MethodPost ||
		!strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req uiReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	op := strings.TrimPrefix(r.URL.Path, "/api/")
	if op == "quit" {
		writeJSON(w, map[string]any{"ok": true})
		go func() { time.Sleep(200 * time.Millisecond); u.cancel() }()
		return
	}
	res, err := u.run(op, req)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error(), "needPassphrase": strings.Contains(err.Error(), "口令") || strings.Contains(err.Error(), "passphrase")})
		return
	}
	writeJSON(w, res)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func (u *uiServer) run(op string, req uiReq) (any, error) {
	r, err := openCLIRepo(u.dir)
	if err != nil {
		return nil, err
	}
	r.Pass = fixedPassphrase(req.Passphrase)
	if err := r.Lock(); err != nil {
		return nil, err
	}
	defer r.Unlock()
	switch op {
	case "status", "unlock":
		s, err := r.Load()
		if err != nil {
			return nil, err
		}
		if !s.Initialized() {
			return nil, errNotInitialized
		}
		return u.status(r, s), nil
	case "add":
		pub, err := ParseMemberKey(req.Key)
		if err != nil {
			return nil, err
		}
		if req.Name != "" && !reNameOK.MatchString(req.Name) {
			return nil, errorf("名字无效")
		}
		s, err := r.Admin(AdminOp{Add: &Member{FP: Fingerprint(pub), Key: AuthorizedKey(pub)}, AddName: req.Name,
			Op: "add " + Fingerprint(pub), SignerPass: r.Pass})
		if err != nil {
			return nil, err
		}
		return u.status(r, s), nil
	case "fingerprint":
		pub, err := ParseMemberKey(req.Key)
		if err != nil {
			return nil, err
		}
		return map[string]string{"fp": Fingerprint(pub)}, nil
	case "remove":
		s, err := r.Admin(AdminOp{Remove: req.FP, NewKey: true, Op: "remove " + req.FP, SignerPass: r.Pass})
		if err != nil {
			return nil, err
		}
		return u.status(r, s), nil
	case "rotate":
		s, err := r.Admin(AdminOp{NewKey: true, Op: "rotate", SignerPass: r.Pass})
		if err != nil {
			return nil, err
		}
		return u.status(r, s), nil
	case "backup":
		s, err := r.Load()
		if err != nil {
			return nil, err
		}
		path, pass, err := writeBackup(r, s, "")
		if err != nil {
			return nil, err
		}
		return map[string]string{"path": path, "passphrase": pass}, nil
	}
	return nil, errorf("未知操作")
}

func (u *uiServer) status(r *Repo, s *Snapshot) map[string]any {
	mine := map[string]bool{}
	for _, id := range FindIdentities() {
		mine[id.FP()] = true
	}
	var members []map[string]any
	for _, m := range s.Manifest.Members {
		members = append(members, map[string]any{"fp": m.FP, "name": memberName(s, m.FP), "me": mine[m.FP]})
	}
	var log []LogEntry
	for i := len(s.Index.Log) - 1; i >= 0 && len(log) < 20; i-- {
		e := s.Index.Log[i]
		e.By = memberName(s, e.By)
		log = append(log, e)
	}
	hook := installHook(r.local, true)
	return map[string]any{
		"url": r.URL, "repoId": s.Manifest.RepoID, "keyId": s.Manifest.KeyID, "keyAgeDays": keyAgeDays(s),
		"members": members, "refs": len(s.Index.Refs), "packs": len(s.Index.Packs), "log": log, "hookWarning": hook,
	}
}

func openBrowser(url string) {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		c = exec.Command("open", url)
	default:
		c = exec.Command("xdg-open", url)
	}
	c.Stdout, c.Stderr = nil, nil
	if err := c.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "无法自动打开浏览器，请手动打开上面的地址")
	}
}
