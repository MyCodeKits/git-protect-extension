// Package crypt implements the genc encrypted file format.
//
// Layout:
//
//	header  = "GENC" | version(1) | keyID(8) | salt(32)
//	body    = chunk*   each chunk = ChaCha20-Poly1305(<=64KiB plaintext) + 16B tag
//	nonce   = chunk counter (11B big endian) | last-chunk flag (1B)
//
// Every file gets its own key: HKDF-SHA256(subkey(purpose), salt, info=context|header),
// so nonces never repeat across files, and the header and the caller supplied
// context (e.g. "which pack, which part") are bound to the ciphertext.
package crypt

import (
	"bufio"
	"bytes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	Magic      = "GENC"
	Version    = 1
	KeySize    = 32
	KeyIDSize  = 8
	saltSize   = 32
	HeaderSize = len(Magic) + 1 + KeyIDSize + saltSize
	ChunkSize  = 64 * 1024
	tagSize    = chacha20poly1305.Overhead
)

var (
	ErrFormat   = errors.New("genc: not a genc encrypted file or unsupported version")
	ErrWrongKey = errors.New("genc: file was encrypted with a different key")
	ErrAuth     = errors.New("genc: decryption failed: data was tampered with or truncated")
)

// Key is a repository master key.
type Key [KeySize]byte

func NewKey() (Key, error) {
	var k Key
	_, err := rand.Read(k[:])
	return k, err
}

func derive(secret []byte, salt []byte, info string, n int) []byte {
	out := make([]byte, n)
	if _, err := io.ReadFull(hkdf.New(sha256.New, secret, salt, []byte(info)), out); err != nil {
		panic(err) // only fails if n is absurdly large
	}
	return out
}

// ID is a public identifier of the key, safe to publish.
func (k Key) ID() [KeyIDSize]byte {
	var id [KeyIDSize]byte
	copy(id[:], derive(k[:], nil, "genc/v1/keyid", KeyIDSize))
	return id
}

func (k Key) IDHex() string { id := k.ID(); return hex.EncodeToString(id[:]) }

func (k Key) subkey(purpose string) []byte {
	return derive(k[:], nil, "genc/v1/"+purpose, KeySize)
}

func fileAEAD(k Key, purpose, context string, header []byte) (cipher.AEAD, error) {
	info := "genc/v1/file\x00" + purpose + "\x00" + context + "\x00" + string(header)
	fk := derive(k.subkey(purpose), header[HeaderSize-saltSize:HeaderSize], info, KeySize)
	return chacha20poly1305.New(fk)
}

func nonce(counter uint64, last bool) []byte {
	n := make([]byte, chacha20poly1305.NonceSize)
	for i := 0; i < 8; i++ {
		n[10-i] = byte(counter >> (8 * i))
	}
	if last {
		n[11] = 1
	}
	return n
}

// Encrypt reads all of r and writes the encrypted file to w.
func Encrypt(w io.Writer, r io.Reader, k Key, purpose, context string) error {
	header := make([]byte, HeaderSize)
	copy(header, Magic)
	header[len(Magic)] = Version
	id := k.ID()
	copy(header[len(Magic)+1:], id[:])
	if _, err := rand.Read(header[HeaderSize-saltSize:]); err != nil {
		return err
	}
	aead, err := fileAEAD(k, purpose, context, header)
	if err != nil {
		return err
	}
	if _, err := w.Write(header); err != nil {
		return err
	}
	br := bufio.NewReaderSize(r, ChunkSize+1)
	buf := make([]byte, ChunkSize, ChunkSize+tagSize)
	for counter := uint64(0); ; counter++ {
		n, err := io.ReadFull(br, buf)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return err
		}
		last := n < ChunkSize
		if !last {
			if _, perr := br.Peek(1); perr == io.EOF {
				last = true
			} else if perr != nil {
				return perr
			}
		}
		if _, err := w.Write(aead.Seal(buf[:0], nonce(counter, last), buf[:n], nil)); err != nil {
			return err
		}
		if last {
			return nil
		}
	}
}

// ReadKeyID returns the key id stored in an encrypted file header.
func ReadKeyID(header []byte) (string, error) {
	if len(header) < HeaderSize || string(header[:len(Magic)]) != Magic || header[len(Magic)] != Version {
		return "", ErrFormat
	}
	return hex.EncodeToString(header[len(Magic)+1 : len(Magic)+1+KeyIDSize]), nil
}

// Decrypt authenticates and decrypts r into w. Output is written chunk by
// chunk, so callers must discard everything written if an error is returned.
func Decrypt(w io.Writer, r io.Reader, k Key, purpose, context string) error {
	header := make([]byte, HeaderSize)
	if _, err := io.ReadFull(r, header); err != nil {
		return ErrFormat
	}
	if _, err := ReadKeyID(header); err != nil {
		return err
	}
	id := k.ID()
	if subtle.ConstantTimeCompare(header[len(Magic)+1:len(Magic)+1+KeyIDSize], id[:]) != 1 {
		return ErrWrongKey
	}
	aead, err := fileAEAD(k, purpose, context, header)
	if err != nil {
		return err
	}
	br := bufio.NewReaderSize(r, ChunkSize+tagSize+1)
	buf := make([]byte, ChunkSize+tagSize)
	for counter := uint64(0); ; counter++ {
		n, err := io.ReadFull(br, buf)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return err
		}
		if n < tagSize {
			return ErrAuth // missing or truncated chunk
		}
		last := n < len(buf)
		if !last {
			if _, perr := br.Peek(1); perr == io.EOF {
				last = true
			} else if perr != nil {
				return perr
			}
		}
		pt, err := aead.Open(buf[:0], nonce(counter, last), buf[:n], nil)
		if err != nil {
			return ErrAuth
		}
		if last && len(pt) == 0 && counter > 0 {
			return ErrAuth // an empty final chunk is only valid for empty files
		}
		if _, err := w.Write(pt); err != nil {
			return err
		}
		if last {
			return nil
		}
	}
}

// EncryptBytes / DecryptBytes are conveniences for small payloads.
func EncryptBytes(pt []byte, k Key, purpose, context string) ([]byte, error) {
	var out bytes.Buffer
	err := Encrypt(&out, bytes.NewReader(pt), k, purpose, context)
	return out.Bytes(), err
}

func DecryptBytes(ct []byte, k Key, purpose, context string) ([]byte, error) {
	var out bytes.Buffer
	if err := Decrypt(&out, bytes.NewReader(ct), k, purpose, context); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func ParseKeyHex(s string) (Key, error) {
	var k Key
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != KeySize {
		return k, fmt.Errorf("genc: invalid key")
	}
	copy(k[:], b)
	return k, nil
}
