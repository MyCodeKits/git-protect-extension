package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An existing project that already has a (private, plaintext) remote and a team.
// The owner moves it to an encrypted remote; a teammate switches their existing
// clone over without re-cloning.
func TestExistingProjectMigration(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.user("alice"), e.user("bob")
	oldRemote := filepath.Join(e.root, "old-private.git")
	e.git(e.root, "init", "-q", "--bare", oldRemote)

	// the project as it exists today
	wa := filepath.Join(e.root, "wa")
	alice.ok(e.root, "git", "init", "-q", wa)
	write(t, filepath.Join(wa, "main.go"), "package main // SECRET_CONTENT_7f3a\n")
	alice.ok(wa, "git", "add", ".")
	alice.ok(wa, "git", "commit", "-q", "-m", "SECRET_MSG_91c2")
	alice.ok(wa, "git", "remote", "add", "origin", oldRemote)
	alice.ok(wa, "git", "push", "-q", "-u", "origin", "main")
	wb := filepath.Join(e.root, "wb")
	bob.ok(e.root, "git", "clone", "-q", oldRemote, wb)
	head := alice.ok(wa, "git", "rev-parse", "HEAD")

	// owner: keep the old remote under another name, then encrypt to a new empty repository
	alice.ok(wa, "git", "remote", "rename", "origin", "old")
	alice.ok(wa, "git-protect", "init", "--remote", e.remote, "--name", "alice")
	alice.ok(wa, "git", "push", "-q", "-u", "origin", "--all")
	alice.ok(wa, "git", "push", "-q", "origin", "--tags")
	alice.fail(wa, "禁止以明文推送", "git", "push", "old", "main") // the plaintext remote is now blocked
	alice.ok(wa, "git", "remote", "remove", "old")
	alice.ok(wa, "git-protect", "add", "--key", bob.pub(), "--name", "bob", "--yes")
	e.assertNoLeak("migrated")

	// teammate: switch the existing clone, no re-clone, same commit ids
	bob.ok(wb, "git", "remote", "set-url", "origin", "protect::"+e.remote)
	bob.ok(wb, "git", "fetch", "-q", "origin")
	bob.ok(wb, "git", "branch", "-q", "--set-upstream-to=origin/main")
	if got := bob.ok(wb, "git", "rev-parse", "origin/main"); got != head {
		t.Fatalf("commit ids changed by migration: %s != %s", got, head)
	}
	if hook, _ := os.ReadFile(filepath.Join(wb, ".git", "hooks", "pre-push")); !strings.Contains(string(hook), "git-protect") {
		t.Fatal("switched clone did not get the plaintext-push guard")
	}
	write(t, filepath.Join(wb, "b.txt"), "BOB_SECRET_LINE\n")
	bob.ok(wb, "git", "add", ".")
	bob.ok(wb, "git", "commit", "-q", "-m", "bob after migration")
	bob.ok(wb, "git", "push", "-q")
	alice.ok(wa, "git", "pull", "-q", "--ff-only")
	if _, err := os.Stat(filepath.Join(wa, "b.txt")); err != nil {
		t.Fatal("alice did not receive bob's push")
	}
	e.assertNoLeak("after teammate push")
}
