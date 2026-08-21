package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// Store owns backups.dir. It is the only thing in the system that writes there,
// and the only thing that produces the index — which is what makes invariant I10
// (`backups.json` ≡ `ls backups.dir`) a property of the code rather than a habit.
//
// v1 had three writers and two indexes. `storage_server.py` wrote uploads,
// `controller.sh` wrote SSH-fetched archives, and a cron-ish prune deleted them;
// the index was a `backup_log` text file appended by one of the three, while
// `restore_target` was a separate file written by a fourth. They drifted, and
// bug 4 is one of the shapes that drift takes: the controller believing it had a
// backup it could not serve.
//
// Every mutation here goes through the same path — write, then regenerate the
// index from the directory, then notify. There is no code path that updates the
// index without re-reading the disk, so an index entry for a file that does not
// exist is not a bug that can be introduced by editing this file wrongly.
type Store struct {
	dir string
	loc *time.Location

	maxUpload int64
	minFree   int64

	logf func(string, ...any)
	now  func() time.Time

	// freeBytes reports the space available on the filesystem holding dir. It is a
	// field so tests can present a full disk without needing one: the behaviour
	// worth testing is the refusal, and you cannot fill a CI runner's disk on
	// purpose without also breaking everything else running on it.
	freeBytes func(dir string) (int64, bool)

	mu  sync.Mutex
	idx *state.Backups

	// onChange is called with a copy of the fresh index after every mutation. The
	// controller uses it to publish backups.json to its state branch; nil is fine
	// and means nobody is listening.
	onChange func(*state.Backups)
}

// StoreOptions configures a Store.
type StoreOptions struct {
	// Dir is backups.dir. It is created if missing.
	Dir string

	// Loc is identity.timezone. Backup filenames are rendered in it, so an
	// operator reading a name sees Prague wall-clock regardless of where the
	// container ran — see state.NewBackupName.
	Loc *time.Location

	// MaxUpload is backups.upload_max_bytes; 0 means no limit.
	MaxUpload int64

	// MinFree is controller.storage.min_free_bytes: what an upload must leave
	// behind on the filesystem.
	MinFree int64

	Logf     func(string, ...any)
	Now      func() time.Time
	OnChange func(*state.Backups)

	// freeBytes overrides the disk-space probe in tests.
	freeBytes func(dir string) (int64, bool)
}

// NewStore opens (and creates) the directory and builds the initial index.
func NewStore(o StoreOptions) (*Store, error) {
	if o.Dir == "" {
		return nil, errors.New("httpapi: the backups directory is empty")
	}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("httpapi: %w", err)
	}
	s := &Store{
		dir:       o.Dir,
		loc:       o.Loc,
		maxUpload: o.MaxUpload,
		minFree:   o.MinFree,
		logf:      o.Logf,
		now:       o.Now,
		freeBytes: o.freeBytes,
		onChange:  o.OnChange,
		idx:       state.NewBackups(),
	}
	if s.loc == nil {
		s.loc = time.UTC
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.freeBytes == nil {
		s.freeBytes = diskFree
	}
	if _, err := s.Rescan(); err != nil {
		return nil, err
	}
	return s, nil
}

// Dir is the directory this store owns.
func (s *Store) Dir() string { return s.dir }

// Has reports whether an archive under this name is on disk right now.
//
// It stats rather than consulting the index, because the caller is an upload
// handler deciding whether it is about to overwrite something, and a file that
// arrived since the last rescan is still a file it would overwrite.
func (s *Store) Has(name string) bool {
	if !state.IsBackupName(name) {
		return false
	}
	info, err := os.Stat(filepath.Join(s.dir, name))
	return err == nil && info.Mode().IsRegular()
}

// Seed reconciles a previously published index with the directory and returns the
// result.
//
// The controller calls this once at startup, handing over what its state branch
// last said. The disk decides which archives exist — an index entry for a file that
// is gone is exactly the drift I10 forbids — but the published index is the only
// record of two things the disk cannot carry: which archives an operator has
// already downloaded, and the digests of the ones still present. Feeding it in as
// the previous index lets rescan's reuse rule do the rest, and that rule is
// conservative by construction: a digest is carried over only when size and mtime
// both still agree.
//
// It deliberately does not notify. The caller is the thing that publishes, and
// calling back into it from inside its own startup would be a loop.
func (s *Store) Seed(published *state.Backups) *state.Backups {
	s.mu.Lock()
	defer s.mu.Unlock()
	if published == nil {
		return copyIndex(s.idx)
	}
	prev := s.idx
	s.idx = copyIndex(published)
	idx, err := s.rescanLocked()
	if err != nil {
		// The directory is unreadable. Keeping the seeded copy would leave the store
		// claiming archives nothing has verified are there, so fall back to what the
		// last successful rescan — the one in NewStore — found.
		s.logf("backups: seeding from the published index failed, keeping the last scan: %v", err)
		s.idx = prev
		return copyIndex(prev)
	}
	return idx
}

// Index returns a copy of the current index. A copy, because the caller is going
// to marshal it or render it into a page while an upload may be finishing, and a
// shared slice is how that becomes a data race that only shows up under load.
func (s *Store) Index() *state.Backups {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyIndex(s.idx)
}

func copyIndex(in *state.Backups) *state.Backups {
	out := &state.Backups{Version: in.Version, UpdatedAt: in.UpdatedAt}
	out.Items = append(make([]state.Backup, 0, len(in.Items)), in.Items...)
	return out
}

// Rescan regenerates the index from the directory. This is the single generator
// I10 names: nothing else constructs a state.Backups from the disk.
//
// Digests are carried over from the previous index when the name, size and mtime
// all match, so a rescan after an upload does not re-read gigabytes that have not
// changed. That reuse is the reason mtime is part of the key rather than a
// cosmetic field: it is the only cheap evidence that a file is the same file.
func (s *Store) Rescan() (*state.Backups, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rescanLocked()
}

func (s *Store) rescanLocked() (*state.Backups, error) {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("httpapi: reading %s: %w", s.dir, err)
	}

	prev := s.idx
	next := state.NewBackups()
	var hashed int

	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !state.IsBackupName(name) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			// The file vanished between ReadDir and Info — a prune racing a
			// rescan. Leaving it out is exactly right; it is not there.
			continue
		}

		entry := state.Backup{
			Name: name,
			Size: info.Size(),
			// mtime, not the timestamp in the filename. They agree for an archive
			// this lease produced, and they differ for one an operator uploaded
			// from their laptop — where the filename may be weeks old. Expiring on
			// the filename would delete a just-uploaded archive on the next prune,
			// which is precisely the archive someone went to the trouble of
			// uploading in order to restore from.
			//
			// Truncated to the second, and rendered in identity.timezone, because
			// this value has to survive a round trip through the state branch
			// unchanged. backups.json stores stamps as RFC 3339, which is
			// second-granular; a nanosecond kept here would come back different,
			// and the reuse key below would then declare every archive modified and
			// rehash the whole directory on the one occasion — a restart — when the
			// controller's job is to come back quickly.
			CreatedAt: state.At(info.ModTime().Truncate(time.Second)).In(s.loc),
		}
		if old := prev.Find(name); old != nil {
			entry.DownloadedAt = old.DownloadedAt
			if old.SHA256 != "" && old.Size == entry.Size &&
				old.CreatedAt.Time.Equal(entry.CreatedAt.Time) {
				entry.SHA256 = old.SHA256
			}
		}
		if entry.SHA256 == "" {
			sum, err := hashFile(filepath.Join(s.dir, name))
			if err != nil {
				s.logf("backups: cannot hash %s, listing it without a digest: %v", name, err)
			} else {
				entry.SHA256 = sum
				hashed++
			}
		}
		next.Items = append(next.Items, entry)
	}

	next.Sort()
	next.UpdatedAt = state.At(s.now().In(s.loc))
	s.idx = next
	if hashed > 0 {
		s.logf("backups: indexed %d archive(s), hashed %d", len(next.Items), hashed)
	}
	return copyIndex(next), nil
}

// notify hands a copy of the index to whoever is publishing it. Called outside
// the lock: onChange reaches the FSM, and holding a store lock while another
// component decides to ask the store something is how a deadlock is built.
func (s *Store) notify(idx *state.Backups) {
	if s.onChange != nil && idx != nil {
		s.onChange(idx)
	}
}

// hashFile is the digest of a file on disk, streamed.
func hashFile(path string) (string, error) {
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

// names lists the backup files actually present, sorted, for tests and logs.
func (s *Store) names() []string {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() && state.IsBackupName(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}
