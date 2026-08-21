package httpapi

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// indexMatchesDisk is invariant I10 as an assertion: backups.json ≡ ls
// backups.dir. Every mutation test calls it, because the whole point of routing
// every write through one generator is that no sequence of operations can break
// it — and the only way to keep that true is to check it after each one.
func indexMatchesDisk(t *testing.T, h *harness) {
	t.Helper()
	var want []string
	for _, n := range h.onDisk() {
		if state.IsBackupName(n) {
			want = append(want, n)
		}
	}
	got := h.store.Index().Names()
	// Names() is newest-first; onDisk() is lexical. Backup names sort lexically by
	// timestamp, so comparing sorted sets is the honest comparison here.
	gotSorted := append([]string{}, got...)
	sort.Strings(gotSorted)
	if !reflect.DeepEqual(gotSorted, want) {
		t.Fatalf("I10 violated:\n  index: %v\n  disk:  %v", gotSorted, want)
	}
}

func TestRescanIndexesOnlyBackupNames(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	h.write("backup_20260819_010000.zip", 16, testNow)
	h.write("backup_20260818_010000.zip", 32, testNow.Add(-24*time.Hour))
	// Three shapes that must not be indexed: a v1 log file, a half-finished
	// upload, and something an operator dropped in by hand. Only the backup shape
	// is indexed, which is what lets a rescan need no manifest to tell it what the
	// directory holds.
	h.write("backup_log.txt", 4, testNow)
	h.write(".backup_20260819_020000.zip.part123", 8, testNow)
	h.write("world.zip", 8, testNow)

	idx, err := h.store.Rescan()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"backup_20260819_010000.zip", "backup_20260818_010000.zip"}
	if got := idx.Names(); !reflect.DeepEqual(got, want) {
		t.Fatalf("index = %v, want %v (newest first)", got, want)
	}
	indexMatchesDisk(t, h)
}

func TestRescanTimestampsFromMtimeNotTheFilename(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	// An archive an operator uploaded from their laptop: the name says three weeks
	// ago, the bytes arrived a minute ago. Taking the date from the name would let
	// the next prune expire it under retention_days — deleting the very archive
	// someone uploaded in order to restore from.
	uploaded := testNow.Add(-2 * time.Minute)
	h.write("backup_20260729_120000.zip", 16, uploaded)

	idx, err := h.store.Rescan()
	if err != nil {
		t.Fatal(err)
	}
	e := idx.Find("backup_20260729_120000.zip")
	if e == nil {
		t.Fatal("the uploaded archive is not in the index")
	}
	if !e.CreatedAt.Time.Equal(uploaded) {
		t.Fatalf("CreatedAt = %s, want the mtime %s", e.CreatedAt, uploaded.Format(time.RFC3339))
	}

	// And the consequence: a 7-day policy must not touch it.
	doomed := idx.Expired(state.RetentionPolicy{Days: 7}, testNow)
	if len(doomed) != 0 {
		t.Fatalf("a just-uploaded archive was expired by a 7-day policy: %v", doomed)
	}
}

func TestRescanReusesDigestsAndRehashesChangedFiles(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	name := "backup_20260819_010000.zip"
	h.write(name, 64, testNow)
	first, err := h.store.Rescan()
	if err != nil {
		t.Fatal(err)
	}
	sum := first.Find(name).SHA256
	if sum == "" {
		t.Fatal("the first rescan produced no digest")
	}

	// Same name, same size, same mtime: the digest is carried over and the file is
	// not read again. The only externally visible sign of a re-hash is the log
	// line, which is emitted exactly when at least one file was hashed.
	h.logs = nil
	second, err := h.store.Rescan()
	if err != nil {
		t.Fatal(err)
	}
	if got := second.Find(name).SHA256; got != sum {
		t.Fatalf("digest changed across an idle rescan: %s -> %s", sum, got)
	}
	for _, line := range h.logs {
		if strings.Contains(line, "hashed") {
			t.Fatalf("an idle rescan re-hashed: %q", line)
		}
	}

	// Now the contents change but the size does not — the case a size-only reuse
	// key would miss, and the reason mtime is part of it.
	p := filepath.Join(h.dir, name)
	if err := os.WriteFile(p, bytes.Repeat([]byte("Q"), 64), 0o644); err != nil {
		t.Fatal(err)
	}
	later := testNow.Add(time.Minute)
	if err := os.Chtimes(p, later, later); err != nil {
		t.Fatal(err)
	}
	third, err := h.store.Rescan()
	if err != nil {
		t.Fatal(err)
	}
	if got := third.Find(name).SHA256; got == sum {
		t.Fatal("the digest survived a rewrite of the file it describes")
	}
}

func TestPutPublishesAFreshIndexAndPreservesDownloadStamps(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	body := bytes.Repeat([]byte("z"), 512)
	name := "backup_20260819_013623.zip"

	entry, err := h.store.Put(name, bytes.NewReader(body), int64(len(body)), sha256Hex(body))
	if err != nil {
		t.Fatal(err)
	}
	if entry.Size != int64(len(body)) || entry.SHA256 != sha256Hex(body) {
		t.Fatalf("entry = %+v, want size %d and the body digest", entry, len(body))
	}
	if len(h.changes) != 1 {
		t.Fatalf("onChange fired %d times, want 1: an upload that does not publish "+
			"an index is an archive nothing else knows about", len(h.changes))
	}
	indexMatchesDisk(t, h)

	h.store.MarkDownloaded(name)
	stamped := h.store.Index().Find(name)
	if stamped.DownloadedAt.Zero() {
		t.Fatal("MarkDownloaded recorded nothing")
	}
	if got := stamped.DownloadedAt.Time.Location().String(); got != testLoc.String() {
		t.Fatalf("DownloadedAt is in %s, want identity.timezone (%s)", got, testLoc)
	}

	// A rescan must not lose it. The stamp is the only evidence a copy exists off
	// this disk, and a controller that forgot it would warn about an archive that
	// is safe — or, worse, prune one it should not.
	if _, err := h.store.Rescan(); err != nil {
		t.Fatal(err)
	}
	if h.store.Index().Find(name).DownloadedAt.Zero() {
		t.Fatal("a rescan cleared DownloadedAt")
	}
}

func TestMarkDownloadedRecordsOnlyTheFirstFetch(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	name := "backup_20260819_010000.zip"
	h.write(name, 8, testNow)
	if _, err := h.store.Rescan(); err != nil {
		t.Fatal(err)
	}

	h.store.MarkDownloaded(name)
	first := h.store.Index().Find(name).DownloadedAt

	h.clk.advance(2 * time.Hour)
	h.store.MarkDownloaded(name)
	second := h.store.Index().Find(name).DownloadedAt

	// The question the stamp answers is "does a copy exist off this disk", and that
	// becomes true once. Overwriting it on every fetch would turn it into "when was
	// this last downloaded", which nothing asks.
	if !first.Time.Equal(second.Time) {
		t.Fatalf("a second download moved the stamp: %s -> %s", first, second)
	}
}

func TestUndownloadedIsWhatTheDashboardWarnsAbout(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.write("backup_20260819_010000.zip", 8, testNow)
	h.write("backup_20260819_020000.zip", 8, testNow.Add(time.Hour))
	if _, err := h.store.Rescan(); err != nil {
		t.Fatal(err)
	}
	h.store.MarkDownloaded("backup_20260819_010000.zip")

	got := h.store.Undownloaded()
	if len(got) != 1 || got[0].Name != "backup_20260819_020000.zip" {
		t.Fatalf("Undownloaded = %+v, want only the archive nobody fetched", got)
	}
}

// published is the index as it comes back from the state branch: marshalled to JSON
// and parsed again. Seeding from anything else would be testing a document that
// cannot occur, because the only path a published index takes to a restarted
// controller is through backups.json.
func published(t *testing.T, idx *state.Backups) *state.Backups {
	t.Helper()
	raw, err := json.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	out := state.NewBackups()
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSeedKeepsTheDiskAsTheAuthorityAndTheIndexAsTheMemory(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	here := "backup_20260819_010000.zip"
	h.write(here, 64, testNow)
	if _, err := h.store.Rescan(); err != nil {
		t.Fatal(err)
	}
	h.store.MarkDownloaded(here)

	// What the state branch holds after a run that produced two archives, one of
	// which the operator downloaded — and then the lease ended and the volume went
	// with it, so only one file came back.
	prev := published(t, h.store.Index())
	prev.Upsert(state.Backup{
		Name: "backup_20260818_010000.zip", Size: 32,
		SHA256: strings.Repeat("ab", 32), CreatedAt: state.At(testNow.Add(-24 * time.Hour)),
	})

	// Cleared so what is counted at the end is Seed's own doing: MarkDownloaded
	// publishes, correctly, and it is part of the setup here.
	h.changes = nil

	got := h.store.Seed(prev)

	// The gone archive must not survive. A controller still listing it would offer a
	// download that 404s and, worse, could point restore_target at it.
	if got.Has("backup_20260818_010000.zip") {
		t.Fatal("Seed kept an index entry for a file that is not on disk")
	}
	e := got.Find(here)
	if e == nil {
		t.Fatalf("Seed lost the archive that is on disk: %v", got.Names())
	}
	// The download stamp is the one thing the disk cannot remember, and the reason
	// Seed takes the published index at all.
	if e.DownloadedAt.Zero() {
		t.Fatal("Seed dropped DownloadedAt, so the operator would be warned about a copy they already hold")
	}
	indexMatchesDisk(t, h)

	// And it does not publish. The caller is what publishes; calling back into it
	// from inside its own startup is a loop.
	if len(h.changes) != 0 {
		t.Fatalf("Seed fired onChange %d time(s)", len(h.changes))
	}
}

func TestSeedDoesNotRehashArchivesTheIndexAlreadyKnows(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	for _, n := range []string{
		"backup_20260819_010000.zip",
		"backup_20260819_020000.zip",
		"backup_20260819_030000.zip",
	} {
		h.write(n, 64, testNow)
	}
	if _, err := h.store.Rescan(); err != nil {
		t.Fatal(err)
	}
	prev := published(t, h.store.Index())

	// The regression this guards: backups.json stores stamps as RFC 3339, which is
	// second-granular, so an mtime kept at nanosecond precision comes back from the
	// branch as a different instant and the reuse key declares every archive
	// modified. The cost is not cosmetic — it is a full re-read of backups.dir, up to
	// retention_count times upload_max_bytes, on the one occasion when the
	// controller's job is to come back quickly.
	h.logs = nil
	got := h.store.Seed(prev)
	for _, line := range h.logs {
		if strings.Contains(line, "hashed") {
			t.Fatalf("seeding from the published index re-hashed the directory: %q", line)
		}
	}
	for _, e := range got.Items {
		if e.SHA256 != prev.Find(e.Name).SHA256 {
			t.Fatalf("%s: digest changed across a seed", e.Name)
		}
	}
}

func TestSeedWithNothingPublishedKeepsWhatTheDiskSays(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.write("backup_20260819_010000.zip", 8, testNow)
	if _, err := h.store.Rescan(); err != nil {
		t.Fatal(err)
	}
	// A first-ever start: the branch has no backups.json yet. The directory is still
	// the answer, because on a redeploy onto a warm volume the files are there
	// whether or not anything ever wrote an index for them.
	got := h.store.Seed(nil)
	if len(got.Items) != 1 {
		t.Fatalf("Seed(nil) = %v, want the archive that is on disk", got.Names())
	}
}
