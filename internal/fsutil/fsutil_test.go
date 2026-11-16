package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFile_ReplacesContentPrivately(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.yaml")
	if err := os.WriteFile(path, []byte("old content that is longer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("new\n")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new\n" {
		t.Errorf("content = %q", got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("temp file left behind: %v", entries)
	}
}

func TestWriteFile_CreatesParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "x.yaml")
	if err := WriteFile(path, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Dir(path)); err != nil || fi.Mode().Perm() != 0o750 {
		t.Errorf("parent: %v %v", fi, err)
	}
}

// A failed write leaves the existing file as it was and no temp file.
func TestWriteFile_FailureKeepsOriginal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.yaml")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A directory in the target's place makes the rename fail.
	target := filepath.Join(dir, "sub")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(target, []byte("x")); err == nil {
		t.Fatal("expected an error")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("temp file left behind: %v", entries)
	}
}
