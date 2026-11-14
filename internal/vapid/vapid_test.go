package vapid

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadOrCreateIsStable(t *testing.T) {
	dir := t.TempDir()
	k1, created, err := LoadOrCreate(dir)
	if err != nil || !created || k1.PublicKey == "" || k1.PrivateKey == "" {
		t.Fatalf("create: %+v %v %v", k1, created, err)
	}
	k2, created, err := LoadOrCreate(dir)
	if err != nil || created || k2 != k1 {
		t.Fatalf("reload changed the key: %+v %v %v", k2, created, err)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(dir, FileName))
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("perm %v", fi.Mode().Perm())
		}
	}
}

func TestCorruptFileIsNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, FileName)
	os.WriteFile(p, []byte("garbage"), 0o600)
	if _, _, err := LoadOrCreate(dir); err == nil {
		t.Fatal("corrupt key file accepted")
	}
	if b, _ := os.ReadFile(p); string(b) != "garbage" {
		t.Fatal("corrupt key file was overwritten")
	}
}
