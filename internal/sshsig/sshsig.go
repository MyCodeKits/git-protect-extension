// Package sshsig implements OpenSSH's SSHSIG signature format
// (PROTOCOL.sshsig), the same format used by `ssh-keygen -Y sign` and git's
// SSH commit signing, so signatures can be audited with standard tools.
package sshsig

import (
	"bytes"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

const (
	magic    = "SSHSIG"
	hashAlg  = "sha512"
	beginPEM = "-----BEGIN SSH SIGNATURE-----"
	endPEM   = "-----END SSH SIGNATURE-----"
)

var ErrVerify = errors.New("sshsig: signature verification failed")

func signedData(namespace string, msg []byte) []byte {
	h := sha512.Sum512(msg)
	return append([]byte(magic), ssh.Marshal(struct {
		Namespace string
		Reserved  string
		HashAlg   string
		Hash      string
	}{namespace, "", hashAlg, string(h[:])})...)
}

type blob struct {
	Version   uint32
	PublicKey string
	Namespace string
	Reserved  string
	HashAlg   string
	Signature string
}

// Sign returns an armored SSH signature over msg.
func Sign(signer ssh.Signer, namespace string, msg []byte) ([]byte, error) {
	data := signedData(namespace, msg)
	var sig *ssh.Signature
	var err error
	if signer.PublicKey().Type() == ssh.KeyAlgoRSA {
		as, ok := signer.(ssh.AlgorithmSigner)
		if !ok {
			return nil, errors.New("sshsig: RSA signer does not support rsa-sha2-512")
		}
		sig, err = as.SignWithAlgorithm(nil, data, ssh.KeyAlgoRSASHA512)
	} else {
		sig, err = signer.Sign(nil, data)
	}
	if err != nil {
		return nil, err
	}
	raw := append([]byte(magic), ssh.Marshal(blob{
		Version:   1,
		PublicKey: string(signer.PublicKey().Marshal()),
		Namespace: namespace,
		HashAlg:   hashAlg,
		Signature: string(ssh.Marshal(sig)),
	})...)
	enc := base64.StdEncoding.EncodeToString(raw)
	var b strings.Builder
	b.WriteString(beginPEM + "\n")
	for len(enc) > 70 {
		b.WriteString(enc[:70] + "\n")
		enc = enc[70:]
	}
	b.WriteString(enc + "\n" + endPEM + "\n")
	return []byte(b.String()), nil
}

// Verify checks that armored is a valid signature over msg made by pub.
func Verify(pub ssh.PublicKey, namespace string, msg, armored []byte) error {
	s := strings.TrimSpace(strings.ReplaceAll(string(armored), "\r", ""))
	if !strings.HasPrefix(s, beginPEM) || !strings.HasSuffix(s, endPEM) {
		return fmt.Errorf("sshsig: malformed signature")
	}
	s = strings.TrimSuffix(strings.TrimPrefix(s, beginPEM), endPEM)
	raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil || !bytes.HasPrefix(raw, []byte(magic)) {
		return fmt.Errorf("sshsig: malformed signature")
	}
	var b blob
	if err := ssh.Unmarshal(raw[len(magic):], &b); err != nil {
		return fmt.Errorf("sshsig: malformed signature: %v", err)
	}
	if b.Version != 1 || b.Namespace != namespace || b.HashAlg != hashAlg {
		return ErrVerify
	}
	if !bytes.Equal([]byte(b.PublicKey), pub.Marshal()) {
		return ErrVerify
	}
	var sig ssh.Signature
	if err := ssh.Unmarshal([]byte(b.Signature), &sig); err != nil {
		return ErrVerify
	}
	if sig.Format == ssh.KeyAlgoRSA { // legacy SHA-1 RSA signatures are not accepted
		return ErrVerify
	}
	if err := pub.Verify(signedData(namespace, msg), &sig); err != nil {
		return ErrVerify
	}
	return nil
}
