package akash

// Client tests: what gets retried, what does not, and what an error says.
//
// The retry policy is the difference between a controller that survives a Console
// rate limit and one that flaps a deployment; the error text is the difference
// between bug 3 of v1 ("Expecting value: line 1 column 106 (char 105)", repeated
// for days with no indication of what the body was) and a diagnosis.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

const testAPIKey = "console-api-key-not-a-real-one"

// sleeps records what the client waited for, so a test can assert the server's
// Retry-After was honoured rather than merely that something slept.
type sleeps struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (s *sleeps) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waits = append(s.waits, d)
	return nil
}

func (s *sleeps) list() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.waits...)
}

// newTestClient builds a client whose backoff costs no wall-clock time.
func newTestClient(t *testing.T, f *fakeAPI, retries int) (*Client, *sleeps, *strings.Builder) {
	t.Helper()
	var (
		sl  = &sleeps{}
		log strings.Builder
		mu  sync.Mutex
	)
	c, err := New(Options{
		APIBase:   f.url(),
		APIKey:    testAPIKey,
		Retries:   retries,
		RetryWait: 2 * time.Second,
		Logf: func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			log.WriteString(fmt.Sprintf(format, args...) + "\n")
		},
		sleep: sl.sleep,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, sl, &log
}

func TestNewClientRejectsBadOptions(t *testing.T) {
	cases := []struct {
		name string
		o    Options
	}{
		{"no base", Options{APIKey: "k"}},
		{"not a URL", Options{APIBase: "console-api.akash.network", APIKey: "k"}},
		{"no key", Options{APIBase: "https://console-api.akash.network"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.o); err == nil {
				t.Fatalf("New(%+v) succeeded", tc.o)
			}
		})
	}
	// A trailing slash on the base would double up on every path.
	c, err := New(Options{APIBase: "https://console-api.akash.network/", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if c.base != "https://console-api.akash.network" {
		t.Errorf("base = %q, want the trailing slash trimmed", c.base)
	}
}

// TestClientHonoursRetryAfter: a 429 is the documented rate limit and the API says
// how long to wait. Guessing instead is how a controller earns a longer ban.
func TestClientHonoursRetryAfter(t *testing.T) {
	f := newFakeAPI(t)
	calls := 0
	f.on("GET", "/v1/deployments", func(*http.Request, []byte) (int, string) {
		calls++
		if calls == 1 {
			return 429, `{"error":"rate limited"}`
		}
		return 200, `{"data":[]}`
	})
	// The fake cannot set headers through the handler signature, so the header is
	// added by a wrapper around the whole server for this test.
	f.header("Retry-After", "3")

	c, sl, _ := newTestClient(t, f, 4)
	var out deploymentList
	if err := c.do(context.Background(), "GET", "/v1/deployments?limit=1000", nil, &out); err != nil {
		t.Fatalf("do: %v", err)
	}
	if calls != 2 {
		t.Errorf("made %d attempts, want 2", calls)
	}
	if got := sl.list(); len(got) != 1 || got[0] != 3*time.Second {
		t.Errorf("waits = %v, want one 3s wait from the Retry-After header", got)
	}
}

// TestClientDoesNotRetryRejections: a 4xx that is not 429 means the request itself
// was wrong. Repeating it spends quota to receive the same answer, and in the
// deploy path it spends the bid window too.
func TestClientDoesNotRetryRejections(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 422} {
		f := newFakeAPI(t)
		f.fail("POST", "/v1/deployments", status, `{"error":"nope"}`)
		c, sl, _ := newTestClient(t, f, 4)

		err := c.do(context.Background(), "POST", "/v1/deployments", map[string]any{"a": 1}, nil)
		if err == nil {
			t.Fatalf("HTTP %d was reported as success", status)
		}
		if Status(err) != status {
			t.Errorf("Status = %d, want %d", Status(err), status)
		}
		if n := f.countCalls("POST", "/v1/deployments"); n != 1 {
			t.Errorf("HTTP %d was retried %d times", status, n-1)
		}
		if got := sl.list(); len(got) != 0 {
			t.Errorf("HTTP %d slept %v before giving up", status, got)
		}
		if !strings.Contains(err.Error(), "nope") {
			t.Errorf("error %q does not carry what the API said", err)
		}
	}
}

// TestClientGivesUpAfterRetries: a persistent 5xx exhausts the budget and the
// message says how many attempts it took, because "it failed" and "it failed five
// times over two minutes" call for different responses from an operator.
func TestClientGivesUpAfterRetries(t *testing.T) {
	f := newFakeAPI(t)
	f.fail("GET", "/v1/providers", 503, `{"error":"unavailable"}`)
	c, sl, _ := newTestClient(t, f, 3)

	var out providerList
	err := c.do(context.Background(), "GET", "/v1/providers?scope=all", nil, &out)
	if err == nil {
		t.Fatal("a persistent 503 was reported as success")
	}
	if n := f.countCalls("GET", "/v1/providers"); n != 4 {
		t.Errorf("made %d attempts with retries=3, want 4", n)
	}
	if !strings.Contains(err.Error(), "after 4 attempts") {
		t.Errorf("error %q does not say how many attempts were made", err)
	}
	// Doubling from the 2s base, capped at a minute.
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}
	got := sl.list()
	if len(got) != len(want) {
		t.Fatalf("waits = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("wait %d = %s, want %s", i+1, got[i], want[i])
		}
	}
}

// TestClientDecodeErrorQuotesTheBody is bug 3 of v1, as a test. The controller
// logged a parser's column number for days; what it needed to log was the body.
func TestClientDecodeErrorQuotesTheBody(t *testing.T) {
	f := newFakeAPI(t)
	// An HTML error page from a gateway in front of the API: valid HTTP, valid 200,
	// not JSON at all.
	f.json("GET", "/v1/deployments", `<html><head><title>502 Bad Gateway</title></head></html>`)
	c, _, _ := newTestClient(t, f, 4)

	var out deploymentList
	err := c.do(context.Background(), "GET", "/v1/deployments?limit=1000", nil, &out)
	if err == nil {
		t.Fatal("an HTML body decoded as JSON")
	}
	if !strings.Contains(err.Error(), "502 Bad Gateway") {
		t.Errorf("error %q does not quote the body that failed to decode", err)
	}
	if !strings.Contains(err.Error(), "/v1/deployments") {
		t.Errorf("error %q does not name the path", err)
	}
	// A body that does not fit the schema will not fit it on a retry either.
	if n := f.countCalls("GET", "/v1/deployments"); n != 1 {
		t.Errorf("a decode failure was retried %d times", n-1)
	}
}

// TestClientTruncatesHostileBodies: an error body goes into a log line, so its
// length cannot be the remote end's choice.
func TestClientTruncatesHostileBodies(t *testing.T) {
	f := newFakeAPI(t)
	f.fail("GET", "/v1/providers", 400, strings.Repeat("A", 100_000))
	c, _, _ := newTestClient(t, f, 1)

	err := c.do(context.Background(), "GET", "/v1/providers?scope=all", nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("error is not an *APIError: %v", err)
	}
	// maxErrorBody plus the ellipsis that marks the truncation.
	if len(ae.Body) > maxErrorBody+4 {
		t.Errorf("kept %d bytes of the error body, want at most %d", len(ae.Body), maxErrorBody)
	}
	if !strings.HasSuffix(ae.Body, "…") {
		t.Error("a truncated body does not say that it was truncated")
	}
}

// TestClientSendsTheKeyAndNeverLogsIt. The api key is the only secret in this
// package: it authorises spending money from a managed wallet.
func TestClientSendsTheKeyAndNeverLogsIt(t *testing.T) {
	f := newFakeAPI(t)
	var seen string
	f.on("GET", "/v1/providers", func(r *http.Request, _ []byte) (int, string) {
		seen = r.Header.Get("x-api-key")
		return 503, `{"error":"unavailable"}`
	})
	c, _, log := newTestClient(t, f, 2)

	err := c.do(context.Background(), "GET", "/v1/providers?scope=all", nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if seen != testAPIKey {
		t.Errorf("the API received key %q, want the configured one", seen)
	}
	if strings.Contains(log.String(), testAPIKey) {
		t.Error("the api key appears in the retry log")
	}
	if strings.Contains(err.Error(), testAPIKey) {
		t.Error("the api key appears in an error message")
	}
	// Not even a fragment: a truncated secret is still a secret.
	if strings.Contains(log.String(), testAPIKey[:8]) {
		t.Error("part of the api key appears in the retry log")
	}
}

// TestClientDrainsBodyWithNoOutput: a 2xx whose body we do not care about is still
// a success, and the body is read so the connection can be reused.
func TestClientDrainsBodyWithNoOutput(t *testing.T) {
	f := newFakeAPI(t)
	f.json("DELETE", "/v1/deployments/", `{"data":{"dseq":"`+testDSeq+`","closed":true}}`)
	c, _, _ := newTestClient(t, f, 1)

	if err := c.do(context.Background(), "DELETE", "/v1/deployments/"+testDSeq, nil, nil); err != nil {
		t.Fatalf("do: %v", err)
	}
}

// TestClientStopsOnCancelledContext: a cancelled call cannot succeed, and retrying
// it delays a shutdown that is already under way.
func TestClientStopsOnCancelledContext(t *testing.T) {
	f := newFakeAPI(t)
	f.fail("GET", "/v1/providers", 503, `{"error":"unavailable"}`)
	c, _, _ := newTestClient(t, f, 5)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.do(ctx, "GET", "/v1/providers?scope=all", nil, nil)
	if err == nil {
		t.Fatal("a cancelled call reported success")
	}
	if n := f.countCalls("GET", "/v1/providers"); n > 1 {
		t.Errorf("made %d attempts after the context was cancelled", n)
	}
}

func TestRetryAfterHeader(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"", 0, false},
		{"3", 3 * time.Second, true},
		{"0", 0, true},
		{"not a number", 0, false},
		{"-5", 0, false},
	}
	for _, tc := range cases {
		h := http.Header{}
		if tc.in != "" {
			h.Set("Retry-After", tc.in)
		}
		got, ok := retryAfter(h)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("retryAfter(%q) = %s, %v; want %s, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestBackoffCapsAtAMinute(t *testing.T) {
	c := &Client{retryWait: 2 * time.Second}
	for _, tc := range []struct {
		attempt int
		want    time.Duration
	}{
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{5, 32 * time.Second},
		{6, time.Minute},
		{20, time.Minute},
	} {
		if got := c.backoff(tc.attempt); got != tc.want {
			t.Errorf("backoff(%d) = %s, want %s", tc.attempt, got, tc.want)
		}
	}
}

func TestSnipCollapsesNewlines(t *testing.T) {
	got := snip("  {\n  \"error\": \"bad\"\r\n}\t ")
	if strings.ContainsAny(got, "\n\r\t") {
		t.Errorf("snip left a line break in %q", got)
	}
	if !strings.Contains(got, `"error"`) {
		t.Errorf("snip dropped the content: %q", got)
	}
}
