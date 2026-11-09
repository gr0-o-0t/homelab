package backup

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// `homelab backup --fetch` copies a backup from the leader's backups volume
// to a PC as a tar stream: ExportTar on the leader, ExtractTar on the PC.
// Plain (uncompressed) tar: the volume archives inside are already gzipped.

// ValidName checks a backup name: one plain path element, nothing to
// traverse with.
func ValidName(name string) error {
	if name == "." || name == ".." || !safeNameRe.MatchString(name) {
		return fmt.Errorf("%q is not a backup name (a directory listed by `homelab backup --list`)", name)
	}
	return nil
}

// ExportTar writes the backup root/name to w as a tar stream. The name must
// be a plain name of an existing backup (a directory with a valid manifest,
// not a symlink); only directories and regular files are exported, and
// reads are confined to the backup by os.Root.
func ExportTar(w io.Writer, root, name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	dir := filepath.Join(root, name)
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("no backup named %q in %s", name, root)
	}
	if _, err := ReadManifest(dir); err != nil {
		return err
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	fsys := r.FS()

	tw := tar.NewWriter(w)
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: p + "/", Mode: 0o750, ModTime: info.ModTime()})
		case d.Type().IsRegular():
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: p, Mode: 0o600,
				Size: info.Size(), ModTime: info.ModTime()}); err != nil {
				return err
			}
			f, err := fsys.Open(p)
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			_, err = io.CopyN(tw, f, info.Size())
			return err
		default:
			return fmt.Errorf("refusing to export %s/%s: not a regular file", name, p)
		}
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// ExtractTar unpacks a stream written by ExportTar into the existing
// directory dir. The stream is untrusted: an entry that is not a plain
// relative path, or is anything but a directory or a regular file (symlink,
// hard link, device), stops the extraction with an error; files are created
// exclusively, so a duplicate entry cannot overwrite one; and every write
// goes through os.Root, which cannot leave dir.
func ExtractTar(r io.Reader, dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the backup stream: %w", err)
		}
		name := strings.TrimSuffix(hdr.Name, "/")
		if !filepath.IsLocal(name) || strings.Contains(name, `\`) {
			return fmt.Errorf("refusing unsafe path %q in the backup stream", hdr.Name)
		}
		name = filepath.Clean(name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o750); err != nil {
				return err
			}
		case tar.TypeReg:
			if parent := filepath.Dir(name); parent != "." {
				if err := root.MkdirAll(parent, 0o750); err != nil {
					return err
				}
			}
			f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr) // #nosec G110 -- plain tar, nothing is decompressed; size is the backup's own
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("refusing %q in the backup stream: only directories and regular files are allowed", hdr.Name)
		}
	}
}
