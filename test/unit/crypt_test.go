package unit

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"

	. "genc/internal/crypt"
)

func mustKey(t *testing.T) Key {
	k, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRoundTripSizes(t *testing.T) {
	k := mustKey(t)
	for _, n := range []int{0, 1, ChunkSize - 1, ChunkSize, ChunkSize + 1, 3 * ChunkSize, 3*ChunkSize + 17} {
		pt := make([]byte, n)
		rand.Read(pt)
		ct, err := EncryptBytes(pt, k, "data", "ctx")
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecryptBytes(ct, k, "data", "ctx")
		if err != nil || !bytes.Equal(got, pt) {
			t.Fatalf("size %d: roundtrip failed: %v", n, err)
		}
		if n >= 32 && bytes.Contains(ct, pt[:32]) {
			t.Fatalf("size %d: plaintext visible in ciphertext", n)
		}
	}
}

func TestSamePlaintextDifferentCiphertext(t *testing.T) {
	k := mustKey(t)
	a, _ := EncryptBytes([]byte("hello"), k, "data", "c")
	b, _ := EncryptBytes([]byte("hello"), k, "data", "c")
	if bytes.Equal(a, b) {
		t.Fatal("encryption must be randomized")
	}
}

func TestEveryByteIsAuthenticated(t *testing.T) {
	k := mustKey(t)
	pt := make([]byte, 2*ChunkSize+100)
	rand.Read(pt)
	ct, _ := EncryptBytes(pt, k, "data", "c")
	// flip one bit at a sample of positions across header and all chunks
	for _, pos := range []int{0, 4, 5, 12, 13, HeaderSize - 1, HeaderSize, HeaderSize + ChunkSize, len(ct) - 17, len(ct) - 1} {
		bad := append([]byte{}, ct...)
		bad[pos] ^= 1
		if _, err := DecryptBytes(bad, k, "data", "c"); err == nil {
			t.Fatalf("tampering at %d not detected", pos)
		}
	}
}

func TestTruncationAndExtension(t *testing.T) {
	k := mustKey(t)
	pt := make([]byte, 3*ChunkSize)
	rand.Read(pt)
	ct, _ := EncryptBytes(pt, k, "data", "c")
	chunk := ChunkSize + 16
	for _, cut := range []int{HeaderSize, HeaderSize + chunk, HeaderSize + 2*chunk, len(ct) - 1} {
		if _, err := DecryptBytes(ct[:cut], k, "data", "c"); err == nil {
			t.Fatalf("truncation at %d not detected", cut)
		}
	}
	if _, err := DecryptBytes(append(append([]byte{}, ct...), 0), k, "data", "c"); err == nil {
		t.Fatal("appended data not detected")
	}
}

func TestChunkReorder(t *testing.T) {
	k := mustKey(t)
	pt := make([]byte, 3*ChunkSize)
	rand.Read(pt)
	ct, _ := EncryptBytes(pt, k, "data", "c")
	chunk := ChunkSize + 16
	bad := append([]byte{}, ct...)
	copy(bad[HeaderSize:], ct[HeaderSize+chunk:HeaderSize+2*chunk])
	copy(bad[HeaderSize+chunk:], ct[HeaderSize:HeaderSize+chunk])
	if _, err := DecryptBytes(bad, k, "data", "c"); err == nil {
		t.Fatal("chunk reorder not detected")
	}
}

func TestBindings(t *testing.T) {
	k := mustKey(t)
	ct, _ := EncryptBytes([]byte("secret"), k, "data", "pack1|0")
	if _, err := DecryptBytes(ct, k, "data", "pack1|1"); err == nil {
		t.Fatal("context not bound")
	}
	if _, err := DecryptBytes(ct, k, "index", "pack1|0"); err == nil {
		t.Fatal("purpose not bound")
	}
	if _, err := DecryptBytes(ct, mustKey(t), "data", "pack1|0"); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("wrong key: got %v", err)
	}
	// forge the key id of another key: still must fail authentication
	k2 := mustKey(t)
	bad := append([]byte{}, ct...)
	id := k2.ID()
	copy(bad[len(Magic)+1:], id[:])
	if _, err := DecryptBytes(bad, k2, "data", "pack1|0"); err == nil {
		t.Fatal("forged key id accepted")
	}
}
