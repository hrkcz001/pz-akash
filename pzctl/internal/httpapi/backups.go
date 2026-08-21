package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// Errors the handlers translate into status codes. They are values rather than
// strings so the mapping lives in one switch and a new one cannot silently become
// a 500.
var (
	// ErrBadName is a name that is not a backup filename. Refusing unknown shapes
	// is what makes the directory self-describing: everything in it is a backup,
	// so a rescan needs no manifest to tell it what to index.
	ErrBadName = errors.New("not a backup filename")

	// ErrTooLarge is an upload over backups.upload_max_bytes.
	ErrTooLarge = errors.New("upload exceeds the configured maximum")

	// ErrNoSpace is an upload the disk cannot hold with the reserve intact.
	ErrNoSpace = errors.New("not enough free space")

	// ErrDigestMismatch is a body whose bytes do not hash to the declared digest.
	// The archive is deleted rather than kept: a corrupt backup that is present is
	// worse than one that is absent, because the absent one does not get chosen as
	// a restore target.
	ErrDigestMismatch = errors.New("body does not match the declared digest")

	// ErrNotFound is a backup that is not on disk.
	ErrNotFound = errors.New("no such backup")
)

// Put streams body into the directory as name.
//
// This is the fix for the defect the package comment describes: v1 read the whole
// archive into the controller's RAM before writing a byte of it, in a container
// with a 2 GiB limit. Here the bytes go body → hasher → temp file with a fixed
// buffer, so a 3 GiB world costs the same memory as a 3 KiB one.
//
// declaredSize is Content-Length, or -1 when the sender used chunked encoding.
// wantSHA is the hex digest from HeaderSHA256, or "" to accept whatever arrives.
//
// The write lands on a temp file in the same directory and is renamed only after
// the digest checks out, so a torn upload is never visible under a name the
// restore path could choose. That ordering is the whole guarantee: at no instant
// does a file named backup_*.zip exist with contents nobody has verified.
func (s *Store) Put(name string, body io.Reader, declaredSize int64, wantSHA string) (*state.Backup, error) {
	if !state.IsBackupName(name) {
		return nil, fmt.Errorf("%w: %q", ErrBadName, name)
	}
	if s.maxUpload > 0 && declaredSize > s.maxUpload {
		return nil, fmt.Errorf("%w: %d bytes, limit %d", ErrTooLarge, declaredSize, s.maxUpload)
	}
	// Checked before writing rather than discovered at ENOSPC half-way through,
	// because the request this most often is, is the halt backup — and a halt that
	// fills the disk instead of saving the world is the worst outcome available.
	if declaredSize > 0 && s.minFree > 0 {
		if avail, ok := s.freeBytes(s.dir); ok && avail-declaredSize < s.minFree {
			return nil, fmt.Errorf("%w: %d bytes available, %d incoming, %d must stay free",
				ErrNoSpace, avail, declaredSize, s.minFree)
		}
	}

	tmp, err := os.CreateTemp(s.dir, "."+name+".part*")
	if err != nil {
		return nil, fmt.Errorf("httpapi: %w", err)
	}
	tmpName := tmp.Name()
	// Removed on every path that does not rename. A .part file left behind is not
	// indexed — IsBackupName rejects the dotted prefix — but it does occupy the
	// disk that the next upload's free-space check is about to measure.
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	h := sha256.New()
	written, err := s.copyBounded(tmp, body, h)
	if err != nil {
		return nil, err
	}
	// A body shorter than its declared length is a broken connection, not a small
	// backup. Without this check the digest comparison would catch it anyway — but
	// only when a digest was sent, and the error would name the wrong cause.
	if declaredSize >= 0 && written != declaredSize {
		return nil, fmt.Errorf("httpapi: %s: got %d bytes, Content-Length said %d",
			name, written, declaredSize)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if wantSHA != "" && !strings.EqualFold(got, wantSHA) {
		return nil, fmt.Errorf("%w: computed %s, header said %s", ErrDigestMismatch, got, wantSHA)
	}
	if err := tmp.Sync(); err != nil {
		return nil, fmt.Errorf("httpapi: %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("httpapi: %s: %w", name, err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return nil, fmt.Errorf("httpapi: %s: %w", name, err)
	}

	final := filepath.Join(s.dir, name)
	if err := os.Rename(tmpName, final); err != nil {
		return nil, fmt.Errorf("httpapi: %s: %w", name, err)
	}

	s.mu.Lock()
	idx, err := s.rescanLocked()
	s.mu.Unlock()
	if err != nil {
		// The bytes are safely on disk under the right name; only the index failed
		// to regenerate. Reporting success with a stale index would break I10, so
		// this is an error — but the log has to say the upload itself survived, or
		// the operator will retry a transfer that already succeeded.
		s.logf("backups: %s is on disk but the index could not be regenerated: %v", name, err)
		return nil, err
	}
	s.notify(idx)

	e := idx.Find(name)
	if e == nil {
		return nil, fmt.Errorf("httpapi: %s was written but is not in the fresh index", name)
	}
	out := *e
	return &out, nil
}

// copyBounded streams src to dst and h, refusing to exceed maxUpload.
//
// The bound matters even though Put already checked Content-Length: a chunked
// request declares nothing, and a lying Content-Length is a header the sender
// controls. Without a bound here, `upload_max_bytes` would be advice.
func (s *Store) copyBounded(dst io.Writer, src io.Reader, h hash.Hash) (int64, error) {
	r := src
	if s.maxUpload > 0 {
		// One byte over the limit, so hitting the limit exactly is not mistaken for
		// exceeding it and vice versa.
		r = io.LimitReader(src, s.maxUpload+1)
	}
	n, err := io.Copy(io.MultiWriter(dst, h), r)
	if err != nil {
		return n, fmt.Errorf("httpapi: receiving the upload: %w", err)
	}
	if s.maxUpload > 0 && n > s.maxUpload {
		return n, fmt.Errorf("%w: stopped at %d bytes, limit %d", ErrTooLarge, n, s.maxUpload)
	}
	return n, nil
}

// Open returns an open file and its index entry, for serving a download. The
// caller closes the file.
//
// The entry comes from the index rather than from a fresh stat so that what the
// download advertises — size, digest — is what the index says, and an agent
// verifying a restore against backups.json is comparing two views of the same
// record instead of two independent readings of the disk.
func (s *Store) Open(name string) (*os.File, *state.Backup, error) {
	if !state.IsBackupName(name) {
		return nil, nil, fmt.Errorf("%w: %q", ErrBadName, name)
	}
	s.mu.Lock()
	e := s.idx.Find(name)
	if e != nil {
		cp := *e
		e = &cp
	}
	s.mu.Unlock()

	f, err := os.Open(filepath.Join(s.dir, name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		return nil, nil, fmt.Errorf("httpapi: %w", err)
	}
	if e == nil {
		// On disk but not indexed: a file that appeared since the last rescan.
		// Serving it is correct — it is there — and the rescan puts it in the index
		// for everyone else.
		s.logf("backups: %s is on disk but was not indexed; rescanning", name)
		s.mu.Lock()
		idx, rescanErr := s.rescanLocked()
		s.mu.Unlock()
		if rescanErr == nil {
			s.notify(idx)
			if found := idx.Find(name); found != nil {
				cp := *found
				e = &cp
			}
		}
	}
	return f, e, nil
}

// MarkDownloaded records that an operator has taken a copy of name.
//
// This is the only durability signal the system has. Per the locked decision
// there is no persistent storage, so an archive nobody has downloaded exists in
// exactly one place — an ephemeral disk that a lease close destroys — and the
// dashboard's job is to say so loudly. The stamp is what lets it distinguish
// "backed up" from "backed up somewhere that will survive".
//
// Only the first download is recorded: the question being answered is "does a
// copy exist off this disk", and that becomes true once.
func (s *Store) MarkDownloaded(name string) {
	s.mu.Lock()
	e := s.idx.Find(name)
	if e == nil || !e.DownloadedAt.Zero() {
		s.mu.Unlock()
		return
	}
	e.DownloadedAt = state.At(s.now().In(s.loc))
	s.idx.UpdatedAt = e.DownloadedAt
	idx := copyIndex(s.idx)
	s.mu.Unlock()

	s.logf("backups: %s downloaded", name)
	s.notify(idx)
}

// Prune deletes the archives that fail the retention policy and returns their
// names. protect names archives that must survive — in practice the current
// restore_target, so a scheduled prune cannot delete the file the next boot is
// going to ask for.
//
// state.Backups.Expired refuses to expire the newest archive under any policy,
// which is what keeps a misconfigured retention from emptying the directory.
func (s *Store) Prune(policy state.RetentionPolicy, protect ...string) ([]string, error) {
	policy.Protect = append(append([]string{}, policy.Protect...), protect...)

	s.mu.Lock()
	doomed := s.idx.Expired(policy, s.now())
	s.mu.Unlock()

	if len(doomed) == 0 {
		return nil, nil
	}

	var deleted []string
	var errs []error
	for _, name := range doomed {
		if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("httpapi: deleting %s: %w", name, err))
			continue
		}
		deleted = append(deleted, name)
	}

	s.mu.Lock()
	idx, err := s.rescanLocked()
	s.mu.Unlock()
	if err != nil {
		errs = append(errs, err)
	} else {
		s.notify(idx)
	}
	if len(deleted) > 0 {
		s.logf("backups: pruned %d archive(s): %s", len(deleted), strings.Join(deleted, ", "))
	}
	return deleted, errors.Join(errs...)
}

// Undownloaded is what the dashboard warns about: archives that exist only here.
func (s *Store) Undownloaded() []state.Backup {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.idx.Undownloaded()
}

// Usage reports the bytes the indexed archives occupy and the free space left,
// for the dashboard's disk warning. ok is false where the platform cannot say.
func (s *Store) Usage() (used int64, free int64, ok bool) {
	s.mu.Lock()
	used = s.idx.TotalBytes()
	s.mu.Unlock()
	free, ok = s.freeBytes(s.dir)
	return used, free, ok
}
