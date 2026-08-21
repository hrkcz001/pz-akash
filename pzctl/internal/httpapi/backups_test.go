package httpapi

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// This is the guarantee the temp-file-then-rename ordering exists to provide: at
// no instant does a file named backup_*.zip exist with contents nobody has
// verified. A corrupt archive that is present is worse than one that is absent,
// because the absent one never gets chosen as a restore target.
func TestPutLeavesNothingBehindWhenTheDigestIsWrong(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	body := bytes.Repeat([]byte("z"), 256)
	name := "backup_20260819_013623.zip"

	_, err := h.store.Put(name, bytes.NewReader(body), int64(len(body)), sha256Hex([]byte("something else")))
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("err = %v, want ErrDigestMismatch", err)
	}
	if got := h.onDisk(); len(got) != 0 {
		t.Fatalf("the directory holds %v after a rejected upload; a .part file left "+
			"behind occupies the disk the next free-space check is about to measure", got)
	}
	if len(h.changes) != 0 {
		t.Fatalf("a rejected upload published an index %d time(s)", len(h.changes))
	}
}

func TestPutRejectsABodyShorterThanItsContentLength(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	body := []byte("truncated")

	// No digest, so the length check is the only thing that can catch this. Without
	// it a broken connection would land as a small but valid-looking backup.
	_, err := h.store.Put("backup_20260819_013623.zip", bytes.NewReader(body), 4096, "")
	if err == nil {
		t.Fatal("a short body was accepted")
	}
	if !strings.Contains(err.Error(), "Content-Length said") {
		t.Fatalf("err = %v, want it to name the length mismatch rather than the digest", err)
	}
	if got := h.onDisk(); len(got) != 0 {
		t.Fatalf("the directory holds %v after a truncated upload", got)
	}
}

func TestPutRefusesAnUploadOverTheLimitBothWaysItCanBeDeclared(t *testing.T) {
	const limit = 1024
	body := bytes.Repeat([]byte("z"), limit+1)

	t.Run("honest Content-Length", func(t *testing.T) {
		h := newHarness(t, harnessOptions{maxUpload: limit})
		_, err := h.store.Put("backup_20260819_013623.zip", bytes.NewReader(body), int64(len(body)), "")
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("err = %v, want ErrTooLarge", err)
		}
		if got := h.onDisk(); len(got) != 0 {
			t.Fatalf("the directory holds %v; nothing should have been written at all", got)
		}
	})

	// A chunked sender declares no length, and a lying Content-Length is a header
	// the sender controls. Either way the bound has to be enforced against the
	// bytes actually arriving, or upload_max_bytes is advice.
	for _, declared := range []int64{-1, 8} {
		t.Run("declared "+strconv.FormatInt(declared, 10), func(t *testing.T) {
			h := newHarness(t, harnessOptions{maxUpload: limit})
			_, err := h.store.Put("backup_20260819_013623.zip", bytes.NewReader(body), declared, "")
			if !errors.Is(err, ErrTooLarge) {
				t.Fatalf("err = %v, want ErrTooLarge", err)
			}
			if got := h.onDisk(); len(got) != 0 {
				t.Fatalf("the directory holds %v", got)
			}
		})
	}
}

func TestPutAcceptsExactlyTheLimit(t *testing.T) {
	const limit = 1024
	h := newHarness(t, harnessOptions{maxUpload: limit})
	body := bytes.Repeat([]byte("z"), limit)

	// The LimitReader is maxUpload+1 precisely so hitting the limit is not mistaken
	// for exceeding it. An off-by-one here rejects the largest legal backup, which
	// is the one most likely to be the real world.
	entry, err := h.store.Put("backup_20260819_013623.zip", bytes.NewReader(body), int64(len(body)), sha256Hex(body))
	if err != nil {
		t.Fatalf("an upload of exactly upload_max_bytes was refused: %v", err)
	}
	if entry.Size != limit {
		t.Fatalf("size = %d, want %d", entry.Size, limit)
	}
	indexMatchesDisk(t, h)
}

func TestPutRefusesToFillTheDiskBeforeItStartsWriting(t *testing.T) {
	h := newHarness(t, harnessOptions{minFree: 2 << 30})
	// 1 GiB left, 2 GiB must stay free. The check happens before a byte is written,
	// because the request this most often is, is the halt backup — and a halt that
	// fills the disk instead of saving the world is the worst outcome available.
	h.free = 1 << 30

	body := bytes.Repeat([]byte("z"), 4096)
	_, err := h.store.Put("backup_20260819_013623.zip", bytes.NewReader(body), int64(len(body)), "")
	if !errors.Is(err, ErrNoSpace) {
		t.Fatalf("err = %v, want ErrNoSpace", err)
	}
	if got := h.onDisk(); len(got) != 0 {
		t.Fatalf("the directory holds %v after a refusal that should have written nothing", got)
	}

	// And the arithmetic is `avail - size >= minFree`, not `avail >= minFree`: with
	// room for both the reserve and the upload it goes through.
	h.free = (2 << 30) + 8192
	if _, err := h.store.Put("backup_20260819_013623.zip", bytes.NewReader(body), int64(len(body)), ""); err != nil {
		t.Fatalf("an upload that fits alongside the reserve was refused: %v", err)
	}
	indexMatchesDisk(t, h)
}

func TestPutRejectsANameThatIsNotABackup(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	for _, name := range []string{
		"world.zip",
		"backup_20260819_013623.tar",
		"../backup_20260819_013623.zip",
		".backup_20260819_013623.zip.part",
		"",
	} {
		_, err := h.store.Put(name, strings.NewReader("x"), 1, "")
		if !errors.Is(err, ErrBadName) {
			t.Fatalf("Put(%q) err = %v, want ErrBadName", name, err)
		}
	}
	if got := h.onDisk(); len(got) != 0 {
		t.Fatalf("the directory holds %v", got)
	}
}

func TestPutIsIdempotentUnderARetry(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	name := "backup_20260819_013623.zip"
	body := bytes.Repeat([]byte("z"), 512)

	if _, err := h.store.Put(name, bytes.NewReader(body), int64(len(body)), sha256Hex(body)); err != nil {
		t.Fatal(err)
	}
	// A retry after a transfer the sender was not sure about. It has to be safe:
	// the agent retries an upload whose response it never saw, and the alternative
	// is an agent that gives up on the halt backup.
	if _, err := h.store.Put(name, bytes.NewReader(body), int64(len(body)), sha256Hex(body)); err != nil {
		t.Fatalf("a retry of the same upload failed: %v", err)
	}
	if got := h.store.Index().Names(); len(got) != 1 {
		t.Fatalf("index = %v, want one entry", got)
	}
	indexMatchesDisk(t, h)
}

func TestPutStreamsRatherThanBuffering(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	// The v1 defect this package exists to fix was `self.rfile.read(content_length)`
	// — the whole archive into a 2Gi container. countingReader reports the largest
	// single Read it served, which bounds how much of the body was ever in memory
	// at once. io.Copy's buffer is 32 KiB; anything near the body size means the
	// stream was materialised.
	const size = 4 << 20
	src := &countingReader{r: io.LimitReader(constByte('z'), size)}

	if _, err := h.store.Put("backup_20260819_013623.zip", src, size, ""); err != nil {
		t.Fatal(err)
	}
	if src.maxRead > 1<<20 {
		t.Fatalf("largest single read was %d bytes of a %d-byte body: the upload is "+
			"being buffered, not streamed", src.maxRead, size)
	}
}

type countingReader struct {
	r       io.Reader
	maxRead int
}

func (c *countingReader) Read(p []byte) (int, error) {
	if len(p) > c.maxRead {
		c.maxRead = len(p)
	}
	return c.r.Read(p)
}

// constByte is an endless stream of one byte, so a large body costs no memory to
// produce.
type constByte byte

func (b constByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

func TestPruneNeverEmptiesTheDirectory(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	for i := 0; i < 4; i++ {
		when := testNow.Add(-time.Duration(i) * 48 * time.Hour)
		h.write("backup_"+when.Format("20060102_150405")+".zip", 16, when)
	}
	if _, err := h.store.Rescan(); err != nil {
		t.Fatal(err)
	}
	newest := h.store.Index().Names()[0]

	// A policy that would delete everything: keep one day, keep zero. Backups here
	// are the only copy until an operator downloads one, so the newest survives any
	// policy — a retention rule that can empty the directory is a rule that loses
	// the world.
	deleted, err := h.store.Prune(state.RetentionPolicy{Days: 1, Count: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 3 {
		t.Fatalf("deleted %v, want the three older archives", deleted)
	}
	left := h.store.Index().Names()
	if len(left) != 1 || left[0] != newest {
		t.Fatalf("index = %v, want only the newest (%s)", left, newest)
	}
	indexMatchesDisk(t, h)
}

func TestPruneHonoursTheProtectedRestoreTarget(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	var names []string
	for i := 0; i < 3; i++ {
		when := testNow.Add(-time.Duration(i) * 48 * time.Hour)
		n := "backup_" + when.Format("20060102_150405") + ".zip"
		h.write(n, 16, when)
		names = append(names, n)
	}
	if _, err := h.store.Rescan(); err != nil {
		t.Fatal(err)
	}
	target := names[2] // the oldest, and the one the next boot is going to ask for

	deleted, err := h.store.Prune(state.RetentionPolicy{Count: 1}, target)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range deleted {
		if d == target {
			t.Fatalf("prune deleted the protected restore target %s", target)
		}
	}
	if !h.store.Has(target) {
		t.Fatalf("%s is gone from the disk", target)
	}
	indexMatchesDisk(t, h)
}

func TestPruneOnAnEmptyPolicyIsANoOp(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.write("backup_20260819_010000.zip", 16, testNow)
	h.write("backup_20260818_010000.zip", 16, testNow.Add(-24*time.Hour))
	if _, err := h.store.Rescan(); err != nil {
		t.Fatal(err)
	}
	h.changes = nil

	// Both rules disabled. Nothing is deleted and — the part worth asserting —
	// nothing is published, so a prune ticker on a disabled policy does not churn
	// the state branch with identical commits.
	deleted, err := h.store.Prune(state.RetentionPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 0 {
		t.Fatalf("deleted %v under a policy with no rules", deleted)
	}
	if len(h.changes) != 0 {
		t.Fatalf("a no-op prune published %d index update(s)", len(h.changes))
	}
}
