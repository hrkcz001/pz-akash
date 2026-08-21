package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// upload_timeout has to bound the body, and a bare context deadline does not:
// it fires while io.Copy sits blocked in a socket read that nothing interrupts,
// and the handler returns only when the client eventually gives up. ctxReader is
// what makes the number real, and this is the test that would fail if it were
// removed.
func TestUploadTimeoutActuallyInterruptsAStalledBody(t *testing.T) {
	h := newHarness(t, harnessOptions{uploadTimeout: 150 * time.Millisecond})

	// A body that delivers a little and then stops for much longer than the limit.
	body := io.MultiReader(bytes.NewReader(bytes.Repeat([]byte("z"), 64)), blockingReader{})
	req, err := http.NewRequest(http.MethodPut,
		h.http.URL+BackupPath("backup_20260819_013623.zip"), body)
	if err != nil {
		t.Fatal(err)
	}
	SetAuth(req, testSecs.BackupsPassword)
	// Chunked, because a stalled sender is exactly the case that declares no length.
	req.ContentLength = -1

	start := time.Now()
	resp, err := h.http.Client().Do(req)
	elapsed := time.Since(start)
	if err == nil {
		resp.Body.Close()
	}
	if elapsed > 5*time.Second {
		t.Fatalf("a stalled upload was not interrupted; it ran for %s", elapsed)
	}
	// Whether the client sees a status or a torn connection depends on which side
	// notices first, so the assertion is about the store, not the response: nothing
	// half-written is left under a name the restore path could choose.
	if got := h.onDisk(); len(got) != 0 {
		t.Fatalf("the directory holds %v after an interrupted upload", got)
	}
	found := false
	for _, l := range h.logs {
		if strings.Contains(l, "ran past the") {
			found = true
		}
	}
	if !found {
		t.Fatalf("nothing in the log names the timeout; logs = %v", h.logs)
	}
}

// blockingReader never delivers and never fails, which is what a hung sender
// looks like from the receiving end.
type blockingReader struct{}

func (blockingReader) Read([]byte) (int, error) {
	time.Sleep(10 * time.Second)
	return 0, io.EOF
}

func TestZeroUploadTimeoutMeansUnbounded(t *testing.T) {
	// Zero is legal and validation allows it: an operator restoring a very large
	// world over a slow link is a real case, and a timeout that kills it is worse
	// than none. What must not happen is a zero silently becoming a default.
	h := newHarness(t, harnessOptions{uploadTimeout: 0})
	if h.srv.uploadLimit != 0 {
		t.Fatalf("uploadLimit = %s, want 0 (unbounded)", h.srv.uploadLimit)
	}
	body := bytes.Repeat([]byte("z"), 4096)
	resp := h.do(http.MethodPut, BackupPath("backup_20260819_013623.zip"),
		testSecs.BackupsPassword, bytes.NewReader(body),
		[2]string{HeaderSHA256, sha256Hex(body)})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT = %d, want 201", resp.StatusCode)
	}
}

// The other three timeouts must never be zero on the wire, because two of them
// mean "no limit at all" there — a ReadHeaderTimeout of zero is a connection that
// may dribble headers forever.
func TestTimeoutsFallBackRatherThanBecomingNoLimit(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.srv.readHeader, h.srv.idle, h.srv.shutdown = 0, 0, 0
	if got := h.srv.readHeaderTimeout(); got <= 0 {
		t.Fatalf("readHeaderTimeout = %s, want a positive default", got)
	}
	if got := h.srv.idleTimeout(); got <= 0 {
		t.Fatalf("idleTimeout = %s, want a positive default", got)
	}
	if got := h.srv.shutdownGrace(); got <= 0 {
		t.Fatalf("shutdownGrace = %s, want a positive default", got)
	}
}

func TestListenAndServeStopsWhenTheContextIsCancelled(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	addr := freeAddr(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.srv.ListenAndServe(ctx, addr) }()

	// Wait for the listener rather than sleeping a guessed interval.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := http.Get("http://" + addr + PathHealth); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the server never came up")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		// http.ErrServerClosed is the expected shutdown and must not surface as a
		// failure: the controller's supervisor treats a non-nil error as a crash.
		if err != nil {
			t.Fatalf("ListenAndServe returned %v on a clean shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ListenAndServe did not return after its context was cancelled")
	}
}

func TestListenAndServeReportsAPortItCannotHave(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// A controller that cannot bind its port must fail loudly. v1's storage server
	// logged and carried on, so a port collision looked like a healthy controller
	// serving nothing.
	err = h.srv.ListenAndServe(context.Background(), ln.Addr().String())
	if err == nil {
		t.Fatal("binding an occupied port returned no error")
	}
	if errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("err = %v, want the bind failure", err)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// An archive that appeared since the last rescan is still an archive. Serving it
// is correct — it is there — and the rescan puts it in the index for everyone
// else, which is how an operator's scp lands in backups.json without a restart.
func TestServingAnUnindexedArchiveIndexesIt(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	name := "backup_20260819_010000.zip"
	h.write(name, 256, testNow)
	if h.store.Index().Has(name) {
		t.Fatal("the file was indexed without a rescan")
	}

	resp := h.do(http.MethodGet, BackupPath(name), testSecs.BackupsPassword, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d, want 200", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if len(got) != 256 {
		t.Fatalf("body is %d bytes, want 256", len(got))
	}
	e := h.store.Index().Find(name)
	if e == nil {
		t.Fatal("serving an unindexed archive did not index it")
	}
	if e.SHA256 == "" {
		t.Fatal("the fresh entry has no digest, so nothing can verify a restore from it")
	}
	indexMatchesDisk(t, h)
}

func TestMissingPackageIs404NotACrash(t *testing.T) {
	// packages_dir is empty: the image was built without the archives, or the
	// volume did not mount. A 404 per package is the honest answer and leaves the
	// rest of the service — health, the index — working, which is what an operator
	// needs in order to diagnose it.
	h := newHarness(t, harnessOptions{})
	for _, p := range []struct{ path, token string }{
		{PathClientZip, ""},
		{PathCommonZip, ""},
		{PathServerZip, testSecs.ServerFilesPassword},
	} {
		resp := h.do(http.MethodGet, p.path, p.token, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s with no file on disk = %d, want 404", p.path, resp.StatusCode)
		}
	}
	if resp := h.do(http.MethodGet, PathHealth, "", nil); resp.StatusCode != http.StatusOK {
		t.Fatal("health went down with the missing packages")
	}
}

func TestUsageReportsWhatTheDashboardWarnsOn(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.free = 3 << 30
	h.write("backup_20260819_010000.zip", 1024, testNow)
	h.write("backup_20260818_010000.zip", 2048, testNow.Add(-24*time.Hour))
	if _, err := h.store.Rescan(); err != nil {
		t.Fatal(err)
	}

	used, free, ok := h.store.Usage()
	if !ok {
		t.Fatal("Usage reported no figures")
	}
	if used != 3072 {
		t.Fatalf("used = %d, want 3072", used)
	}
	if free != 3<<30 {
		t.Fatalf("free = %d, want the probe's answer", free)
	}

	// A platform that cannot answer must say so rather than report zero free — a
	// zero would read as a full disk and refuse every upload.
	h.freeOK = false
	if _, _, ok := h.store.Usage(); ok {
		t.Fatal("Usage claimed figures the probe declined to give")
	}
}
