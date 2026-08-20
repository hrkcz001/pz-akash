package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/httpapi"
	"github.com/hrkcz001/pz-akash/pzctl/internal/secrets"
)

// The agent's side of the HTTP contract. What is worth testing here is not that
// Go can fetch a URL, but the three decisions the client makes on the agent's
// behalf: which failures are worth retrying, that a half-transferred file never
// reaches the unpacker, and that an upload is attributable to the request that
// asked for it.

func testSecrets() *secrets.Set {
	return &secrets.Set{ServerFilesPassword: "sf-token", BackupsPassword: "bk-token"}
}

// testAgentConfig keeps the retry budget small: every attempt in these tests is a
// real sleep.
func testAgentConfig() config.Agent {
	a := config.Defaults().Agent
	a.RestoreDownloadRetries = 2
	a.RestoreDownloadTimeout = config.Duration(20 * time.Second)
	return a
}

func newTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, testSecrets(), testAgentConfig(), t.Logf)
}

func TestDownloadWritesTheFileAndSendsTheRealmToken(t *testing.T) {
	var gotAuth, gotPublicAuth string
	cli := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case httpapi.PathServerZip:
			gotAuth = httpapi.BearerToken(r.Header)
			w.Write([]byte("server-zip-bytes"))
		case httpapi.PathCommonZip:
			gotPublicAuth = httpapi.BearerToken(r.Header)
			w.Write([]byte("common"))
		default:
			http.NotFound(w, r)
		}
	}))

	dst := filepath.Join(t.TempDir(), "sub", "server.zip")
	n, err := cli.Download(context.Background(), httpapi.PathServerZip, httpapi.RealmServerFiles, dst)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len("server-zip-bytes")) {
		t.Errorf("size = %d, want %d", n, len("server-zip-bytes"))
	}
	body, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "server-zip-bytes" {
		t.Errorf("file = %q", body)
	}
	// The realm decides the token. server.zip carries the substituted passwords, so
	// it must not be fetchable with the backups token or with none.
	if gotAuth != "sf-token" {
		t.Errorf("server.zip Authorization = %q, want the server-files token", gotAuth)
	}

	if _, err := cli.Download(context.Background(), httpapi.PathCommonZip, httpapi.RealmPublic, filepath.Join(t.TempDir(), "common.zip")); err != nil {
		t.Fatal(err)
	}
	// A public realm sends no header at all: nothing should learn a token it does
	// not need, and the controller's public handler must work without one.
	if gotPublicAuth != "" {
		t.Errorf("common.zip Authorization = %q, want none", gotPublicAuth)
	}
}

func TestDownloadDoesNotRetryA404(t *testing.T) {
	var calls int32
	cli := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.NotFound(w, r)
	}))

	start := time.Now()
	_, err := cli.Download(context.Background(), httpapi.BackupPath("backup_20260819_120000.zip"),
		httpapi.RealmBackups, filepath.Join(t.TempDir(), "b.zip"))
	if err == nil {
		t.Fatal("Download succeeded against a 404")
	}
	// A missing restore target is an operator error. Retrying it for the full
	// restore_download_timeout buries the message the operator needs to see.
	if !IsNotFound(err) {
		t.Errorf("err = %v, want it recognisable as a 404", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("%d requests, want exactly 1", n)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v; a permanent failure must be reported immediately", elapsed)
	}
}

func TestDownloadDoesNotRetryA401(t *testing.T) {
	var calls int32
	cli := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	if _, err := cli.Download(context.Background(), httpapi.PathServerZip, httpapi.RealmServerFiles,
		filepath.Join(t.TempDir(), "s.zip")); err == nil {
		t.Fatal("Download succeeded against a 401")
	}
	// A wrong token will still be wrong in thirty seconds.
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("%d requests, want exactly 1", n)
	}
}

func TestDownloadRetriesA500(t *testing.T) {
	var calls int32
	cli := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			http.Error(w, "still starting", http.StatusBadGateway)
			return
		}
		w.Write([]byte("ok-on-the-second-try"))
	}))

	dst := filepath.Join(t.TempDir(), "server.zip")
	// The controller and the agent boot on different providers at the same time.
	// "Not answering yet" is the normal case; v1 gave up after six seconds and
	// booted a server with no configuration at all.
	if _, err := cli.Download(context.Background(), httpapi.PathServerZip, httpapi.RealmServerFiles, dst); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("%d requests, want 2", n)
	}
	body, _ := os.ReadFile(dst)
	if string(body) != "ok-on-the-second-try" {
		t.Errorf("file = %q", body)
	}
}

func TestDownloadLeavesNoFileWhenTheTransferIsTruncated(t *testing.T) {
	cli := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Declares more than it sends, then hangs up: exactly what a provider's
		// network does mid-download.
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("truncated"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}))

	dir := t.TempDir()
	dst := filepath.Join(dir, "server.zip")
	if _, err := cli.Download(context.Background(), httpapi.PathServerZip, httpapi.RealmServerFiles, dst); err == nil {
		t.Fatal("Download reported success for a truncated transfer")
	}
	// Neither the final name nor the .part may be left behind: unzip would report a
	// corrupt archive, and boot would blame the wrong thing.
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("stat %s = %v, want it absent", dst, err)
	}
	if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Errorf("the .part file was left behind")
	}
}

func TestWaitHealthyWaitsForTheController(t *testing.T) {
	var calls int32
	cli := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != httpapi.PathHealth {
			t.Errorf("health check hit %s", r.URL.Path)
		}
		if atomic.AddInt32(&calls, 1) == 1 {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	}))
	if err := cli.WaitHealthy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("%d health requests, want 2", n)
	}
}

func TestUploadBackupStreamsWithTheAttributionHeaders(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "backup_20260819_120000.zip")
	payload := "pretend this is a two-gigabyte world"
	writeFile(t, archive, payload)
	sum, err := sha256File(archive)
	if err != nil {
		t.Fatal(err)
	}

	var (
		gotMethod, gotName, gotDigest, gotReqID, gotPhase, gotAuth string
		gotLength                                                  int64
		gotBody                                                    string
	)
	cli := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := httpapi.BackupName(r.URL.Path)
		if !ok {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		gotMethod, gotName = r.Method, name
		gotDigest = r.Header.Get(httpapi.HeaderSHA256)
		gotReqID = r.Header.Get(httpapi.HeaderRequestID)
		gotPhase = r.Header.Get(httpapi.HeaderPhase)
		gotAuth = httpapi.BearerToken(r.Header)
		gotLength = r.ContentLength
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)

		json.NewEncoder(w).Encode(httpapi.UploadResult{Name: name, Size: r.ContentLength, SHA256: gotDigest})
	}))

	res, err := cli.UploadBackup(context.Background(), filepath.Base(archive), archive, sum, "req-7", "saving")
	if err != nil {
		t.Fatal(err)
	}
	if res.Name != filepath.Base(archive) || res.SHA256 != sum {
		t.Errorf("result = %+v, want name %s and digest %s", res, filepath.Base(archive), sum)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %s, want PUT — v1 posted a multipart form, which buffered the whole archive", gotMethod)
	}
	if gotName != "backup_20260819_120000.zip" {
		t.Errorf("name = %q", gotName)
	}
	// A declared length is what lets the controller refuse an archive that will
	// not fit before it writes a byte of it.
	if gotLength != int64(len(payload)) {
		t.Errorf("Content-Length = %d, want %d (not chunked)", gotLength, len(payload))
	}
	if gotBody != payload {
		t.Errorf("body = %q, want %q", gotBody, payload)
	}
	if gotDigest != sum {
		t.Errorf("digest header = %q, want %q", gotDigest, sum)
	}
	// The request ID is the bug 4 fix on the wire: without it the controller cannot
	// tell the backup it asked for from one that merely arrived.
	if gotReqID != "req-7" {
		t.Errorf("request id header = %q, want req-7", gotReqID)
	}
	if gotPhase != "saving" {
		t.Errorf("phase header = %q, want saving", gotPhase)
	}
	if gotAuth != "bk-token" {
		t.Errorf("Authorization = %q, want the backups token", gotAuth)
	}
}

func TestUploadBackupRejectsAMismatchedEcho(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "backup_20260819_130000.zip")
	writeFile(t, archive, "world")
	sum, err := sha256File(archive)
	if err != nil {
		t.Fatal(err)
	}

	cli := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		json.NewEncoder(w).Encode(httpapi.UploadResult{
			Name:   filepath.Base(archive),
			SHA256: "0000000000000000000000000000000000000000000000000000000000000000",
		})
	}))

	// The controller hashes what it wrote. A digest that disagrees means the bytes
	// on its disk are not the bytes we archived, and reporting that backup as done
	// would leave the operator trusting an archive that fails at restore time.
	if _, err := cli.UploadBackup(context.Background(), filepath.Base(archive), archive, sum, "req-8", "saving"); err == nil {
		t.Fatal("UploadBackup accepted a mismatched digest")
	}
}

func TestUploadBackupRetriesFromTheStartOfTheFile(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "backup_20260819_140000.zip")
	payload := "the whole archive, twice"
	writeFile(t, archive, payload)
	sum, err := sha256File(archive)
	if err != nil {
		t.Fatal(err)
	}

	var calls int32
	var second string
	cli := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if atomic.AddInt32(&calls, 1) == 1 {
			http.Error(w, "disk busy", http.StatusInternalServerError)
			return
		}
		second = string(b)
		json.NewEncoder(w).Encode(httpapi.UploadResult{Name: filepath.Base(archive), SHA256: sum})
	}))

	if _, err := cli.UploadBackup(context.Background(), filepath.Base(archive), archive, sum, "req-9", "saving"); err != nil {
		t.Fatal(err)
	}
	// Re-opened, not buffered: a multi-gigabyte archive has to be retryable without
	// the agent holding it in memory, which is what v1's multipart upload did on
	// the controller's side.
	if second != payload {
		t.Errorf("second attempt sent %q, want the full payload", second)
	}
}
