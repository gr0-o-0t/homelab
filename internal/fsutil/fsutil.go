// Package fsutil holds the one way homelab writes its state files.
package fsutil

import (
	"os"
	"path/filepath"
)

// WriteFile replaces path with data, mode 0600, creating the parent directory
// (0750) as needed. It writes a temp file in the same directory, fsyncs it
// and renames it over path, then fsyncs the directory: a reader never sees a
// partial file, and a crash leaves either the old content or the new one.
// On failure the original file is untouched and the temp file removed.
func WriteFile(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*") // 0600
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	// The rename is durable only once the directory entry is.
	if d, derr := os.Open(dir); derr == nil { // #nosec G304 -- the caller's own state dir
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
