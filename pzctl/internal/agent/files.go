package agent

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Archive handling. v1 shelled out to `unzip -o` and `zip -q -r -`, which meant
// the exit status was the only diagnostic and the archive shape was defined by
// whichever directory the shell happened to be in.
//
// The shape is preserved exactly: a backup is the *contents* of the Saves
// directory, with paths relative to it, because that is what v1 produced and what
// every existing backup in the controller's storage looks like. Changing it would
// make the operator's downloaded archives unrestorable.

// zipDir writes a deflate archive of everything under src into dst, and returns
// the archive size and its hex SHA-256.
//
// The digest is computed on the way out rather than by re-reading the file: for a
// multi-gigabyte world that halves the disk I/O, and it cannot disagree with what
// was written.
func zipDir(src, dst string) (int64, string, error) {
	st, err := os.Stat(src)
	if err != nil {
		return 0, "", err
	}
	if !st.IsDir() {
		return 0, "", fmt.Errorf("%s is not a directory", src)
	}

	f, err := os.Create(dst)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()

	hasher := sha256.New()
	counter := &countingWriter{}
	zw := zip.NewWriter(io.MultiWriter(f, hasher, counter))

	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// Zip entries are slash-separated regardless of the host, so an archive
		// built on Windows during a test unpacks correctly in the container.
		name := filepath.ToSlash(rel)

		info, err := d.Info()
		if err != nil {
			return err
		}
		// A symlink inside the world directory would be stored as a file
		// containing its target, which silently corrupts the save. PZ does not
		// create any, so this is a guard rather than a feature.
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		hdr.Name = name
		if d.IsDir() {
			hdr.Name += "/"
			hdr.Method = zip.Store
		} else {
			hdr.Method = zip.Deflate
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			// A file PZ deleted between the walk and the open is not a reason to
			// fail the whole backup; the world is a live directory.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		defer in.Close()
		_, err = io.Copy(w, in)
		return err
	})
	if err != nil {
		zw.Close()
		os.Remove(dst)
		return 0, "", err
	}
	if err := zw.Close(); err != nil {
		os.Remove(dst)
		return 0, "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(dst)
		return 0, "", err
	}
	return counter.n, hex.EncodeToString(hasher.Sum(nil)), nil
}

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// unzip extracts src into dst, creating directories as needed and overwriting
// existing files.
//
// Entry names are validated rather than trusted: an archive from the controller
// is not hostile, but a "../../.ssh/authorized_keys" entry in a restored backup
// would be, and the check costs nothing.
func unzip(src, dst string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer zr.Close()

	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, e := range zr.File {
		target, err := safeJoin(dst, e.Name)
		if err != nil {
			return fmt.Errorf("%s: %w", src, err)
		}
		if e.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := writeZipEntry(e, target); err != nil {
			return fmt.Errorf("%s: extract %s: %w", src, e.Name, err)
		}
	}
	return nil
}

func writeZipEntry(e *zip.File, target string) error {
	rc, err := e.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	mode := e.Mode().Perm()
	if mode == 0 {
		mode = 0o644
	}
	// Truncate rather than remove-and-create: the file may be open elsewhere,
	// and `unzip -o` overwrote in place.
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, rc); err != nil {
		return err
	}
	return f.Close()
}

// safeJoin resolves name under root and refuses anything that escapes it.
func safeJoin(root, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty archive entry name")
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("archive entry %q is an absolute path", name)
	}
	target := filepath.Join(root, clean)
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry %q escapes the destination", name)
	}
	return target, nil
}

// sha256File hashes a file, for verifying a download against the controller's
// index before unpacking it over a live world.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// emptyDir removes everything inside dir, creating dir if it does not exist.
//
// It deletes the contents rather than the directory itself because the directory
// may be a mount point or the target of the lowercase symlink, and replacing it
// would break both.
func emptyDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// dirSize totals the bytes under dir, for the log line that says how big a world
// is before zipping it.
func dirSize(dir string) int64 {
	var total int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
