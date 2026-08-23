package agent

// The two-route client.
//
// The controller publishes two addresses for itself: a proxied DNS name, and the
// provider's own host:port. They are not interchangeable. Cloudflare's free plan
// answers 413 to a request body over 100 MB, so a backup upload — one large request
// body — cannot go through the name at all, which is the whole reason the direct
// route is discovered and published. These tests pin that the preference is real,
// that a 413 is treated as "wrong route" rather than "wrong request", and that a
// dead direct address costs one attempt instead of the operation.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/hrkcz001/pz-akash/pzctl/internal/httpapi"
)

// twoRoute stands up two servers and returns a client preferring the first.
func twoRoute(t *testing.T, direct, public http.Handler) (*Client, string, string) {
	t.Helper()
	d := httptest.NewServer(direct)
	t.Cleanup(d.Close)
	p := httptest.NewServer(public)
	t.Cleanup(p.Close)
	return NewClient([]string{d.URL, p.URL}, testSecrets(), testAgentConfig(), t.Logf), d.URL, p.URL
}

// uploadFile writes a small archive and returns its path and digest.
func uploadFile(t *testing.T) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "backup.zip")
	if err := os.WriteFile(path, []byte("archive-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The controller verifies this; these tests only need it to be consistent.
	return path, "0000000000000000000000000000000000000000000000000000000000000000"
}

// TestUploadPrefersTheDirectRoute: the preference has to be observable, because the
// entire point is that one specific request must not go through the proxy.
func TestUploadPrefersTheDirectRoute(t *testing.T) {
	var directHits, publicHits atomic.Int32
	ok := func(hits *atomic.Int32) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{}`))
		}
	}
	cli, _, _ := twoRoute(t, ok(&directHits), ok(&publicHits))

	src, sum := uploadFile(t)
	if _, err := cli.UploadBackup(context.Background(), "b.zip", src, sum, "req-1", "manual"); err != nil {
		t.Fatalf("UploadBackup: %v", err)
	}
	if directHits.Load() != 1 || publicHits.Load() != 0 {
		t.Errorf("direct=%d public=%d; the upload must take the unproxied route first",
			directHits.Load(), publicHits.Load())
	}
}

// TestUploadFallsBackWhenTheDirectRouteIsStale: the direct address is discovered at
// runtime and can be wrong. When it is, the upload has to still happen — a stale
// address must cost an attempt, not the backup.
func TestUploadFallsBackWhenTheDirectRouteIsStale(t *testing.T) {
	var publicHits atomic.Int32
	dead := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A provider host that is up but no longer ours: a 502 is retryable, which is
		// what makes the rotation reach the second base.
		w.WriteHeader(http.StatusBadGateway)
	})
	live := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		publicHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	})
	cli, _, _ := twoRoute(t, dead, live)

	src, sum := uploadFile(t)
	if _, err := cli.UploadBackup(context.Background(), "b.zip", src, sum, "req-2", "halt"); err != nil {
		t.Fatalf("UploadBackup gave up with a working route available: %v", err)
	}
	if publicHits.Load() != 1 {
		t.Errorf("the proxied route was hit %d times, want 1", publicHits.Load())
	}
}

// TestUpload413IsARouteProblemNotARequestProblem is the specific bug this whole
// mechanism exists for. 413 is in the 4xx range, so the retry logic's own rule reads
// it as "will never work" — but it is Cloudflare refusing the body size, and another
// route can carry the identical request. Treating it as permanent means a backup that
// can never be uploaded and an error that blames the request.
func TestUpload413IsARouteProblemNotARequestProblem(t *testing.T) {
	var publicHits atomic.Int32
	// Reversed on purpose: the proxied route is first here, so the 413 is what has to
	// push the upload onto the other one.
	proxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "cloudflare: request body too large", http.StatusRequestEntityTooLarge)
	})
	direct := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		publicHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	})
	cli, _, _ := twoRoute(t, proxy, direct)

	src, sum := uploadFile(t)
	if _, err := cli.UploadBackup(context.Background(), "b.zip", src, sum, "req-3", "periodic"); err != nil {
		t.Fatalf("a 413 from one route ended the upload: %v", err)
	}
	if publicHits.Load() != 1 {
		t.Errorf("the second route was hit %d times, want 1", publicHits.Load())
	}
}

// TestSingleRouteKeeps413Permanent: with nowhere else to go, a 413 is still a
// permanent failure. Retrying it would only delay the report by the whole budget,
// and the operator needs to see the size limit, not a timeout.
func TestSingleRouteKeeps413Permanent(t *testing.T) {
	var hits atomic.Int32
	cli := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
	}))

	src, sum := uploadFile(t)
	_, err := cli.UploadBackup(context.Background(), "b.zip", src, sum, "req-4", "manual")
	if err == nil {
		t.Fatal("a 413 with one route was reported as success")
	}
	if hits.Load() != 1 {
		t.Errorf("tried %d times, want 1 — there was nowhere else to send it", hits.Load())
	}
}

// TestNewClientDropsEmptyAndDuplicateBases lets the caller pass Direct() and Base()
// without checking whether they differ, which they do not before the controller has
// discovered its own address.
func TestNewClientDropsEmptyAndDuplicateBases(t *testing.T) {
	same := "http://controller.example:8000"
	cli := NewClient([]string{"", same, same + "/", "  "}, testSecrets(), testAgentConfig(), t.Logf)
	if len(cli.bases) != 1 || cli.bases[0] != same {
		t.Errorf("bases = %q, want just %q", cli.bases, same)
	}
	if cli.Base() != same {
		t.Errorf("Base() = %q, want %q", cli.Base(), same)
	}

	// And no bases at all is an error rather than a request to "".
	empty := NewClient([]string{"", " "}, testSecrets(), testAgentConfig(), t.Logf)
	if err := empty.WaitHealthy(context.Background()); err == nil {
		t.Error("a client with no base URL reported a healthy controller")
	}
}

// TestDownloadRotatesToo: the restore download is not the call that needs the direct
// route, but it is the call that runs first on a fresh world. If the discovered
// address is stale, the boot has to survive it.
func TestDownloadRotatesToo(t *testing.T) {
	dead := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	live := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("server-zip-bytes"))
	})
	cli, _, _ := twoRoute(t, dead, live)

	dst := filepath.Join(t.TempDir(), "server.zip")
	n, err := cli.Download(context.Background(), httpapi.PathServerZip, httpapi.RealmServerFiles, dst)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if n != int64(len("server-zip-bytes")) {
		t.Errorf("downloaded %d bytes, want %d", n, len("server-zip-bytes"))
	}
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("the file was not written: %v", err)
	}
}
