package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"genc/internal/crypt"
)

func TestCollaborationAndAttacks(t *testing.T) {
	e := newEnv(t)
	alice, bob, carol := e.user("alice"), e.user("bob"), e.user("carol")
	url := "genc::" + e.remote

	// ---- alice creates a repository with secrets and initializes encryption
	wa := filepath.Join(e.root, "wa")
	alice.ok(e.root, "git", "init", "-q", wa)
	write(t, filepath.Join(wa, "src", "secret_module_name.go"), "package x // SECRET_CONTENT_7f3a\n")
	big := strings.Repeat("SECRET_CONTENT_7f3a random filler line\n", 3000)
	write(t, filepath.Join(wa, "big.txt"), big)
	alice.ok(wa, "git", "add", ".")
	alice.ok(wa, "git", "-c", "user.name=AliceSecretAuthor", "-c", "user.email=alice-secret@example.com",
		"commit", "-q", "-m", "SECRET_MSG_91c2", "--author", "AliceSecretAuthor <alice-secret@example.com>")
	alice.ok(wa, "git", "tag", "-a", "v-secret-tag", "-m", "SECRET_MSG_91c2 tag")
	alice.ok(wa, "git", "branch", "feature/secret-branch-x")

	alice.ok(wa, "genc", "init", "--remote", e.remote, "--name", "alice")
	if got := alice.ok(wa, "git", "remote", "get-url", "origin"); got != url {
		t.Fatalf("origin = %q", got)
	}
	alice.ok(wa, "git", "push", "-q", "-u", "origin", "--all")
	alice.ok(wa, "git", "push", "-q", "origin", "--tags")
	e.assertNoLeak("after first push")

	// ---- the pre-push hook blocks plaintext pushes from an encrypted repo
	plain := filepath.Join(e.root, "plain.git")
	e.git(e.root, "init", "-q", "--bare", plain)
	alice.fail(wa, "禁止以明文推送", "git", "push", plain, "main")
	if out := e.git(e.root, "--git-dir="+plain, "for-each-ref"); out != "" {
		t.Fatalf("plaintext push went through: %s", out)
	}

	// ---- a non-member cannot clone
	carol.fail(e.root, "不是该仓库的成员", "git", "clone", "-q", url, filepath.Join(e.root, "wc"))

	// ---- init refuses a remote that already has (possibly plaintext) content
	wp := filepath.Join(e.root, "wp")
	carol.ok(e.root, "git", "init", "-q", wp)
	write(t, filepath.Join(wp, "x.txt"), "x\n")
	carol.ok(wp, "git", "add", ".")
	carol.ok(wp, "git", "commit", "-q", "-m", "x")
	carol.ok(wp, "git", "push", "-q", plain, "main")
	carol.fail(wp, "不是空的", "genc", "init", "--remote", plain)

	// ---- add bob; bob clones and sees everything intact
	alice.ok(wa, "genc", "add", "--key", bob.pub(), "--name", "bob", "--yes")
	wb := filepath.Join(e.root, "wb")
	bob.ok(e.root, "git", "clone", "-q", url, wb)
	if b, _ := os.ReadFile(filepath.Join(wb, "big.txt")); string(b) != big {
		t.Fatal("bob: content mismatch")
	}
	if got := bob.ok(wb, "git", "log", "-1", "--format=%an|%s"); got != "AliceSecretAuthor|SECRET_MSG_91c2" {
		t.Fatalf("bob log: %q", got)
	}
	if got := bob.ok(wb, "git", "tag"); got != "v-secret-tag" {
		t.Fatalf("bob tags: %q", got)
	}
	bob.ok(wb, "git", "rev-parse", "--verify", "origin/feature/secret-branch-x")
	if got := alice.ok(wa, "git", "rev-parse", "HEAD"); got != bob.ok(wb, "git", "rev-parse", "HEAD") {
		t.Fatal("commit ids differ between alice and bob")
	}
	members := alice.ok(wa, "genc", "members")
	if !strings.Contains(members, "alice") || !strings.Contains(members, "bob") {
		t.Fatalf("members: %s", members)
	}

	// ---- normal collaboration: non-fast-forward is rejected, pull+merge works
	write(t, filepath.Join(wb, "b.txt"), "BOB_SECRET_LINE\n")
	bob.ok(wb, "git", "add", ".")
	bob.ok(wb, "git", "commit", "-q", "-m", "bob change")
	bob.ok(wb, "git", "push", "-q")
	write(t, filepath.Join(wa, "a.txt"), "alice change\n")
	alice.ok(wa, "git", "add", ".")
	alice.ok(wa, "git", "commit", "-q", "-m", "alice change")
	alice.fail(wa, "", "git", "push", "-q")
	alice.ok(wa, "git", "pull", "-q", "--no-rebase", "--no-edit")
	alice.ok(wa, "git", "push", "-q")
	bob.ok(wb, "git", "pull", "-q", "--ff-only")
	if _, err := os.Stat(filepath.Join(wb, "a.txt")); err != nil {
		t.Fatal("bob did not get alice's change")
	}
	e.assertNoLeak("after collaboration")

	// ---- concurrent pushes to different branches both succeed (retry on conflict)
	alice.ok(wa, "git", "checkout", "-q", "-b", "ca")
	bob.ok(wb, "git", "checkout", "-q", "-b", "cb")
	write(t, filepath.Join(wa, "ca.txt"), "a\n")
	write(t, filepath.Join(wb, "cb.txt"), "b\n")
	alice.ok(wa, "git", "add", ".")
	alice.ok(wa, "git", "commit", "-q", "-m", "ca")
	bob.ok(wb, "git", "add", ".")
	bob.ok(wb, "git", "commit", "-q", "-m", "cb")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	outs := make([]string, 2)
	wg.Add(2)
	go func() { defer wg.Done(); outs[0], errs[0] = alice.run(wa, "git", "push", "-q", "origin", "ca") }()
	go func() { defer wg.Done(); outs[1], errs[1] = bob.run(wb, "git", "push", "-q", "origin", "cb") }()
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent push %d failed: %v\n%s", i, err, outs[i])
		}
	}
	refs := bob.ok(wb, "git", "ls-remote", "origin")
	if !strings.Contains(refs, "refs/heads/ca") || !strings.Contains(refs, "refs/heads/cb") {
		t.Fatalf("concurrent push lost a branch:\n%s", refs)
	}
	alice.ok(wa, "git", "checkout", "-q", "main")
	bob.ok(wb, "git", "checkout", "-q", "main")
	alice.ok(wa, "git", "push", "-q", "origin", ":ca") // branch deletion
	if strings.Contains(bob.ok(wb, "git", "ls-remote", "origin"), "refs/heads/ca") {
		t.Fatal("branch deletion did not work")
	}

	// ---- key rotation: commit ids, dates, messages unchanged; others auto-unlock
	logFmt := []string{"log", "--all", "--format=%H|%an|%ae|%ad|%cd|%s"}
	bob.ok(wb, "git", "fetch", "-q", "--prune")
	before := bob.ok(wb, append([]string{"git"}, logFmt...)...)
	oldKey, _ := os.ReadFile(filepath.Join(wb, ".git", "genc", "key"))
	alice.ok(wa, "genc", "rotate", "--yes")
	bob.ok(wb, "git", "fetch", "-q", "--prune")
	after := bob.ok(wb, append([]string{"git"}, logFmt...)...)
	if before != after {
		t.Fatalf("history changed by rotation:\n%s\n---\n%s", before, after)
	}
	newKey, _ := os.ReadFile(filepath.Join(wb, ".git", "genc", "key"))
	if bytes.Equal(oldKey, newKey) {
		t.Fatal("bob did not receive the new key")
	}
	wa2 := filepath.Join(e.root, "wa2")
	alice.ok(e.root, "git", "clone", "-q", url, wa2)
	if got := alice.ok(wa2, append([]string{"git"}, logFmt...)...); got != after {
		t.Fatal("fresh clone after rotation differs")
	}
	if n := e.git(e.root, "--git-dir="+e.remote, "rev-list", "--count", "main"); n != "1" {
		t.Fatalf("rotation should leave a single storage commit, got %s", n)
	}
	e.assertNoLeak("after rotation")

	// ---- remove bob: he can no longer read anything new
	staleCommit := e.remoteHead()
	alice.ok(wa, "genc", "remove", "bob", "--yes")
	write(t, filepath.Join(wa, "after-remove.txt"), "new secret\n")
	alice.ok(wa, "git", "add", ".")
	alice.ok(wa, "git", "commit", "-q", "-m", "after remove")
	alice.ok(wa, "git", "push", "-q")
	bob.fail(wb, "不是该仓库的成员", "git", "fetch", "-q")
	// bob's old key cannot open the new index
	bobOld, _ := crypt.ParseKeyHex(strings.TrimSpace(string(newKey)))
	man := e.readManifest("main")
	if _, err := crypt.DecryptBytes(e.blob("main:index.enc"), bobOld, "index", "index|"+man.RepoID); err == nil {
		t.Fatal("removed member's key decrypts new index")
	}
	e.assertNoLeak("after remove")

	// ---- attacks by someone with write access to the remote but no key
	good := e.remoteHead()

	// (a) tampering with ciphertext
	tampered := e.forgeCommit(good, func(path string, data []byte) []byte {
		if path == "index.enc" {
			data = append([]byte{}, data...)
			data[len(data)-5] ^= 1
		}
		return data
	})
	e.setRemoteHead(tampered)
	alice.fail(wa, "", "git", "fetch", "-q")
	e.setRemoteHead(good)

	// (b) rollback to an older state
	e.setRemoteHead(staleCommit)
	alice.fail(wa, "回退", "git", "fetch", "-q")
	e.setRemoteHead(good)

	// (c) a forged manifest that adds carol, signed by carol
	forged := e.forgeTakeover(carol, good)
	e.setRemoteHead(forged)
	alice.fail(wa, "不受信任", "git", "fetch", "-q")
	alice.fail(wa, "不受信任", "git", "push", "-q")
	e.setRemoteHead(good)

	// (d) a swapped key box (wrong key for a member) is detected on fresh unlock
	k2, _ := crypt.NewKey()
	swapped := e.forgeCommit(good, func(path string, data []byte) []byte {
		if strings.HasPrefix(path, "keys/") && strings.HasSuffix(path, ".key") {
			return keyBox(t, k2, alice.pubLine())
		}
		return data
	})
	e.setRemoteHead(swapped)
	alice.fail(e.root, "不一致", "git", "clone", "-q", url, filepath.Join(e.root, "wa-swap"))
	e.setRemoteHead(good)
	alice.ok(wa, "git", "fetch", "-q")

	// ---- backup and restore on a machine without alice's private key
	out := alice.ok(wa, "genc", "backup", "--out", filepath.Join(e.root, "backup.age"))
	pass := regexp.MustCompile(`恢复口令：(\S+)`).FindStringSubmatch(out)
	if pass == nil {
		t.Fatalf("no passphrase in output: %s", out)
	}
	dave := e.user("dave")
	wd := filepath.Join(e.root, "wd")
	dave.ok(e.root, "git", "init", "-q", wd)
	dave.ok(wd, "git", "remote", "add", "origin", url)
	c := exec.Command("genc", "restore", filepath.Join(e.root, "backup.age"), "--passphrase-stdin")
	c.Dir, c.Env, c.Stdin = wd, append(os.Environ(), dave.vars...), strings.NewReader(pass[1]+"\n")
	if o, err := c.CombinedOutput(); err != nil {
		t.Fatalf("restore: %v\n%s", err, o)
	}
	dave.ok(wd, "git", "fetch", "-q", "origin")
	dave.ok(wd, "git", "checkout", "-q", "-b", "main", "origin/main")
	if _, err := os.Stat(filepath.Join(wd, "after-remove.txt")); err != nil {
		t.Fatal("restore did not give access")
	}
	wrong := exec.Command("genc", "restore", filepath.Join(e.root, "backup.age"), "--passphrase-stdin")
	wrong.Dir, wrong.Env, wrong.Stdin = wd, append(os.Environ(), dave.vars...), strings.NewReader("WRONG-PASS\n")
	if o, err := wrong.CombinedOutput(); err == nil {
		t.Fatalf("wrong passphrase accepted: %s", o)
	}

	// ---- gc merges data files; clone still works
	for i := 0; i < 3; i++ {
		write(t, filepath.Join(wa, fmt.Sprintf("n%d.txt", i)), "x\n")
		alice.ok(wa, "git", "add", ".")
		alice.ok(wa, "git", "commit", "-q", "-m", "n")
		alice.ok(wa, "git", "push", "-q")
	}
	alice.ok(wa, "genc", "gc")
	if st := alice.ok(wa, "genc", "status"); !strings.Contains(st, "数据文件：1 个") {
		t.Fatalf("gc did not merge packs: %s", st)
	}
	wa3 := filepath.Join(e.root, "wa3")
	alice.ok(e.root, "git", "clone", "-q", url, wa3)
	if alice.ok(wa3, "git", "rev-parse", "HEAD") != alice.ok(wa, "git", "rev-parse", "HEAD") {
		t.Fatal("clone after gc differs")
	}
	dave.ok(wd, "git", "fetch", "-q") // restored clients keep working after gc
	e.assertNoLeak("final")
}
