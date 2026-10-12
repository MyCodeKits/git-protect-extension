package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
	"filippo.io/age/agessh"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// Identity is one of the user's SSH key pairs. The private part is loaded lazily
// because it may be protected by a passphrase.
type Identity struct {
	Path string
	Pub  ssh.PublicKey

	signer ssh.Signer
	age    age.Identity
}

// PassphraseFunc returns the passphrase for a key, or an error if none can be obtained.
type PassphraseFunc func(path string) ([]byte, error)

var errNeedPassphrase = errors.New("private key is protected by a passphrase")

// nonInteractive is used inside the git remote helper, where no prompt is possible.
func nonInteractive(path string) ([]byte, error) { return nil, errNeedPassphrase }

func terminalPassphrase(path string) ([]byte, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, errNeedPassphrase
	}
	fmt.Fprintf(os.Stderr, "请输入私钥口令 %s: ", path)
	p, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	return p, err
}

func fixedPassphrase(p string) PassphraseFunc {
	return func(path string) ([]byte, error) {
		if p == "" {
			return nil, errNeedPassphrase
		}
		return []byte(p), nil
	}
}

func Fingerprint(pub ssh.PublicKey) string {
	h := sha256.Sum256(pub.Marshal())
	return hex.EncodeToString(h[:8])
}

func (id *Identity) FP() string { return Fingerprint(id.Pub) }

func AuthorizedKey(pub ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
}

func protectHome() string {
	if h := os.Getenv("GIT_PROTECT_HOME"); h != "" {
		return h
	}
	d, err := os.UserConfigDir()
	if err != nil {
		d = os.TempDir()
	}
	return filepath.Join(d, "git-protect")
}

// identityPaths lists candidate private keys, most specific first.
func identityPaths() []string {
	var paths []string
	if p := os.Getenv("GIT_PROTECT_IDENTITY"); p != "" {
		return []string{p} // explicit choice: use only this key
	}
	paths = append(paths, filepath.Join(protectHome(), "id_ed25519"))
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".ssh", "id_ed25519"), filepath.Join(home, ".ssh", "id_rsa"))
	}
	return paths
}

// FindIdentities returns all usable key pairs without decrypting them.
func FindIdentities() []*Identity {
	var ids []*Identity
	seen := map[string]bool{}
	for _, p := range identityPaths() {
		id, err := openIdentity(p)
		if err != nil || seen[id.FP()] {
			continue
		}
		seen[id.FP()] = true
		ids = append(ids, id)
	}
	return ids
}

func openIdentity(path string) (*Identity, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	id := &Identity{Path: path}
	raw, err := ssh.ParseRawPrivateKey(pem)
	var missing *ssh.PassphraseMissingError
	switch {
	case errors.As(err, &missing):
		id.Pub = missing.PublicKey
		if id.Pub == nil {
			if pubBytes, e := os.ReadFile(path + ".pub"); e == nil {
				id.Pub, _, _, _, _ = ssh.ParseAuthorizedKey(pubBytes)
			}
		}
		if id.Pub == nil {
			return nil, fmt.Errorf("%s: cannot read public key", path)
		}
	case err != nil:
		return nil, err
	default:
		if err := id.setPrivate(raw); err != nil {
			return nil, err
		}
	}
	if t := id.Pub.Type(); t != ssh.KeyAlgoED25519 && t != ssh.KeyAlgoRSA {
		return nil, fmt.Errorf("%s: unsupported key type %s", path, t)
	}
	return id, nil
}

func (id *Identity) setPrivate(raw any) error {
	var err error
	switch k := raw.(type) {
	case *ed25519.PrivateKey:
		raw = *k
		return id.setPrivate(raw)
	case ed25519.PrivateKey:
		id.age, err = agessh.NewEd25519Identity(k)
	case *rsa.PrivateKey:
		id.age, err = agessh.NewRSAIdentity(k)
	default:
		return fmt.Errorf("unsupported private key type %T", raw)
	}
	if err != nil {
		return err
	}
	id.signer, err = ssh.NewSignerFromKey(raw)
	if err != nil {
		return err
	}
	id.Pub = id.signer.PublicKey()
	return nil
}

// Unlock loads the private key, asking for a passphrase if needed.
func (id *Identity) Unlock(pass PassphraseFunc) error {
	if id.signer != nil {
		return nil
	}
	pem, err := os.ReadFile(id.Path)
	if err != nil {
		return err
	}
	p, err := pass(id.Path)
	if err != nil {
		return err
	}
	raw, err := ssh.ParseRawPrivateKeyWithPassphrase(pem, p)
	if err != nil {
		return fmt.Errorf("%s: %v", id.Path, err)
	}
	return id.setPrivate(raw)
}

func (id *Identity) NeedsPassphrase() bool { return id.signer == nil }

func ParseMemberKey(line string) (ssh.PublicKey, error) {
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(line)))
	if err != nil {
		return nil, fmt.Errorf("无法解析公钥: %v", err)
	}
	switch pub.Type() {
	case ssh.KeyAlgoED25519:
	case ssh.KeyAlgoRSA:
		cp, ok := pub.(ssh.CryptoPublicKey)
		if !ok {
			return nil, errors.New("无法解析 RSA 公钥")
		}
		if rk, ok := cp.CryptoPublicKey().(*rsa.PublicKey); !ok || rk.N.BitLen() < 3072 {
			return nil, errors.New("RSA 公钥长度必须 ≥ 3072 位，建议使用 ssh-ed25519")
		}
	default:
		return nil, fmt.Errorf("不支持的公钥类型 %s，请使用 ssh-ed25519 或 ssh-rsa", pub.Type())
	}
	return pub, nil
}

func ageRecipient(pub ssh.PublicKey) (age.Recipient, error) {
	return agessh.ParseRecipient(AuthorizedKey(pub))
}

// GenerateIdentity creates a new ed25519 key pair in the git-protect home directory.
func GenerateIdentity(passphrase []byte) (string, error) {
	dir := protectHome()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "id_ed25519")
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("%s 已存在", path)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	var block any
	if len(passphrase) > 0 {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "git-protect", passphrase)
	} else {
		block, err = ssh.MarshalPrivateKey(priv, "git-protect")
	}
	if err != nil {
		return "", err
	}
	if err := writePrivateFile(path, pemEncode(block)); err != nil {
		return "", err
	}
	sp, _ := ssh.NewPublicKey(pub)
	if err := os.WriteFile(path+".pub", ssh.MarshalAuthorizedKey(sp), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
