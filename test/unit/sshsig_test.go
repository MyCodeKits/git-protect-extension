package unit

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	. "genc/internal/sshsig"
)

func signers(t *testing.T) []ssh.Signer {
	_, ed, _ := ed25519.GenerateKey(rand.Reader)
	s1, err := ssh.NewSignerFromKey(ed)
	if err != nil {
		t.Fatal(err)
	}
	rk, _ := rsa.GenerateKey(rand.Reader, 3072)
	s2, err := ssh.NewSignerFromKey(rk)
	if err != nil {
		t.Fatal(err)
	}
	return []ssh.Signer{s1, s2}
}

func TestSignVerify(t *testing.T) {
	msg := []byte("manifest contents")
	all := signers(t)
	for _, s := range all {
		sig, err := Sign(s, "ns", msg)
		if err != nil {
			t.Fatal(err)
		}
		if err := Verify(s.PublicKey(), "ns", msg, sig); err != nil {
			t.Fatalf("%s: %v", s.PublicKey().Type(), err)
		}
		if Verify(s.PublicKey(), "other", msg, sig) == nil {
			t.Fatal("namespace not checked")
		}
		if Verify(s.PublicKey(), "ns", []byte("manifest contentz"), sig) == nil {
			t.Fatal("message not checked")
		}
		for _, o := range all {
			if o != s && Verify(o.PublicKey(), "ns", msg, sig) == nil {
				t.Fatal("wrong key accepted")
			}
		}
	}
}

// Interop with OpenSSH, when an ssh-keygen with -Y support is available.
func TestOpenSSHInterop(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not found")
	}
	s := signers(t)[0]
	msg := []byte("hello genc\n")
	sig, _ := Sign(s, "genc-test", msg)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "msg"), msg, 0o600)
	os.WriteFile(filepath.Join(dir, "msg.sig"), sig, 0o600)
	allowed := "tester " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey()))) + "\n"
	os.WriteFile(filepath.Join(dir, "allowed"), []byte(allowed), 0o600)
	c := exec.Command("ssh-keygen", "-Y", "verify", "-f", filepath.Join(dir, "allowed"), "-I", "tester", "-n", "genc-test", "-s", filepath.Join(dir, "msg.sig"))
	in, _ := os.Open(filepath.Join(dir, "msg"))
	defer in.Close()
	c.Stdin = in
	out, err := c.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "illegal option") || strings.Contains(string(out), "unknown option") || strings.Contains(string(out), "usage") {
			t.Skip("ssh-keygen does not support -Y")
		}
		t.Fatalf("ssh-keygen rejected our signature: %s", out)
	}
}
