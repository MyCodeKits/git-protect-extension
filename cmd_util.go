package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func pemEncode(block any) []byte { return pem.EncodeToMemory(block.(*pem.Block)) }

// writeAtomic writes via a temp file + rename so readers never see partial files.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp-" + randHex(4)
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// writePrivateFile writes a secret readable only by the current user.
func writePrivateFile(path string, data []byte) error {
	if err := writeAtomic(path, data, 0o600); err != nil {
		return err
	}
	restrictACL(path)
	return nil
}

// restrictACL removes inherited permissions on Windows, where file modes are ignored.
func restrictACL(path string) {
	if runtime.GOOS != "windows" {
		return
	}
	user := os.Getenv("USERNAME")
	if user == "" {
		return
	}
	if d := os.Getenv("USERDOMAIN"); d != "" {
		user = d + `\` + user
	}
	exec.Command("icacls", path, "/inheritance:r", "/grant:r", user+":F").Run()
}

// fileLock is a simple cross-process lock based on exclusive file creation.
type fileLock struct{ path string }

func acquireLock(dir string) (*fileLock, error) {
	path := filepath.Join(dir, "lock")
	deadline := time.Now().Add(2 * time.Minute)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.WriteString(time.Now().Format(time.RFC3339))
			f.Close()
			return &fileLock{path}, nil
		}
		if fi, serr := os.Stat(path); serr == nil && time.Since(fi.ModTime()) > 10*time.Minute {
			os.Remove(path) // left behind by a crashed process
			continue
		}
		if time.Now().After(deadline) {
			return nil, errorf("另一个 genc 进程正在操作此仓库（锁文件 %s）", path)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func (l *fileLock) Release() {
	if l != nil {
		os.Remove(l.path)
	}
}
