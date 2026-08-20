package dns

// The HTTP layer, tested at the two places it decides something: whether a failure
// is worth another attempt, and whether a failure happened at all. The second is the
// one that matters historically — v1 read Cloudflare's success flag and threw the
// answer away, so a zone could stop being updated with nothing in the log to say so.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
)

// TestRefusalWithHTTP200IsAFailure is bug-for-bug the opposite of v1: a 200 whose
// body says success:false is a call that did nothing, and it has to arrive as an
// error carrying what Cloudflare said.
func TestRefusalWithHTTP200IsAFailure(t *testing.T) {
	f := newFakeCF(t)
	f.onFail(func(method, _ string) (int, string) {
		if method == "POST" {
			return 200, refuse(9041, "Invalid TTL for a proxied record")
		}
		return 0, ""
	})
	c := f.client(t, testZone())

	_, err := c.SyncGame(context.Background(), "203.0.113.7")
	if err == nil {
		t.Fatal("a call that changed nothing reported success")
	}
	if got := Status(err); got != 0 {
		t.Errorf("Status = %d, want 0: the refusal came in the body, not the status line", got)
	}
	if !Code(err, 9041) {
		t.Errorf("the error does not carry code 9041: %v", err)
	}
	for _, want := range []string{"refused", "Invalid TTL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not mention %q: %v", want, err)
		}
	}
	// And it is not retried: the request was wrong, and repeating it buys the same
	// answer for another unit of quota.
	if n := f.count("POST "); n != 1 {
		t.Errorf("a refused POST was attempted %d times, want 1", n)
	}
}

func TestRetriesAServerErrorThenSucceeds(t *testing.T) {
	f := newFakeCF(t)
	var tries int32
	f.onFail(func(method, _ string) (int, string) {
		if method == "POST" && atomic.AddInt32(&tries, 1) <= 2 {
			return 502, refuse(1000, "bad gateway")
		}
		return 0, ""
	})
	c := f.client(t, testZone())

	changes, err := c.SyncGame(context.Background(), "203.0.113.7")
	if err != nil {
		t.Fatalf("SyncGame: %v", err)
	}
	if len(changes) != 1 || changes[0].Action != Created {
		t.Fatalf("changes = %+v, want the record created on the third attempt", changes)
	}
	if n := f.count("POST "); n != 3 {
		t.Errorf("POST attempts = %d, want 3", n)
	}
	// retry_wait doubling: 1ms then 2ms, from testZone's retry_wait of 1ms.
	naps := f.naps()
	if len(naps) != 2 || naps[0] != time.Millisecond || naps[1] != 2*time.Millisecond {
		t.Errorf("backoffs = %v, want 1ms then 2ms", naps)
	}
}

func TestGivesUpAfterTheConfiguredRetries(t *testing.T) {
	f := newFakeCF(t)
	f.onFail(func(method, _ string) (int, string) {
		if method == "GET" {
			return 503, refuse(1000, "service unavailable")
		}
		return 0, ""
	})
	c := f.client(t, testZone())

	_, err := c.SyncGame(context.Background(), "203.0.113.7")
	if err == nil {
		t.Fatal("a permanently unavailable zone reported success")
	}
	// retries: 3 means four attempts — the first, plus three retries.
	if n := f.count("GET "); n != 4 {
		t.Errorf("GET attempts = %d, want 4 (one attempt plus dns.retries)", n)
	}
	if !strings.Contains(err.Error(), "after 4 attempts") {
		t.Errorf("the message does not say how many attempts were made: %v", err)
	}
	if got := Status(err); got != 503 {
		t.Errorf("Status = %d, want 503", got)
	}
}

func TestDoesNotRetryABadRequest(t *testing.T) {
	f := newFakeCF(t)
	f.onFail(func(method, _ string) (int, string) {
		if method == "GET" {
			return 400, refuse(6003, "Invalid request headers")
		}
		return 0, ""
	})
	c := f.client(t, testZone())

	_, err := c.SyncGame(context.Background(), "203.0.113.7")
	if err == nil {
		t.Fatal("a 400 reported success")
	}
	if n := f.count("GET "); n != 1 {
		t.Errorf("GET attempts = %d, want 1: a malformed request is malformed again", n)
	}
	if !Code(err, 6003) {
		t.Errorf("the error does not carry code 6003: %v", err)
	}
}

// TestHonoursRetryAfter: Cloudflare's rate limit is per-zone and it says how long to
// wait. Backing off on our own schedule instead is how a rate limit gets extended.
func TestHonoursRetryAfter(t *testing.T) {
	f := newFakeCF(t)
	f.sendRetryAfter("7")
	var tries int32
	f.onFail(func(method, _ string) (int, string) {
		if method == "GET" && atomic.AddInt32(&tries, 1) == 1 {
			return 429, refuse(10000, "rate limited")
		}
		return 0, ""
	})
	c := f.client(t, testZone())

	if _, err := c.SyncGame(context.Background(), "203.0.113.7"); err != nil {
		t.Fatalf("SyncGame: %v", err)
	}
	naps := f.naps()
	if len(naps) != 1 || naps[0] != 7*time.Second {
		t.Errorf("backoffs = %v, want the 7s the server asked for", naps)
	}
}

// TestAnUnauthenticatedCallFailsOnEveryPath: the fake demands the bearer token
// everywhere, so a path that forgot the header fails here rather than in production
// on whichever path nobody exercised.
func TestAnUnauthenticatedCallFails(t *testing.T) {
	f := newFakeCF(t)
	z := testZone()
	z.APIBase = f.srv.URL
	c, err := New(Options{
		Zone:  z,
		Token: "wrong-token",
		Logf:  func(string, ...any) {},
		sleep: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.SyncGame(context.Background(), "203.0.113.7"); err == nil {
		t.Fatal("a call with the wrong token succeeded")
	} else if got := Status(err); got != 403 {
		t.Errorf("Status = %d, want 403", got)
	}
	if n := f.count("GET "); n != 1 {
		t.Errorf("GET attempts = %d, want 1: a bad token stays bad", n)
	}
}

// TestABodyThatIsNotJSONIsNotRetried — a proxy or captive portal answering 200 with
// HTML. Worth naming in the error, because "decoding" plus a snippet of the body is
// what tells an operator the request never reached Cloudflare at all.
func TestABodyThatIsNotJSONIsNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html><body>Attention Required! | Cloudflare</body></html>"))
	}))
	defer srv.Close()

	z := testZone()
	z.APIBase = srv.URL
	c, err := New(Options{Zone: z, Token: "t", Logf: func(string, ...any) {},
		sleep: func(context.Context, time.Duration) error { return nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.SyncGame(context.Background(), "203.0.113.7")
	if err == nil {
		t.Fatal("an HTML body was accepted as a Cloudflare response")
	}
	if !strings.Contains(err.Error(), "decoding") || !strings.Contains(err.Error(), "Attention Required") {
		t.Errorf("the message does not show what arrived instead: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}

// TestATimeoutBoundsTheCall: nothing in this package is allowed to fail a deploy,
// and an unbounded call to a hung endpoint would do exactly that.
func TestATimeoutBoundsTheCall(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer func() { close(block); srv.Close() }()

	z := testZone()
	z.APIBase = srv.URL
	z.Timeout = config.Duration(50 * time.Millisecond)
	z.Retries = 0
	c, err := New(Options{Zone: z, Token: "t", Logf: func(string, ...any) {},
		sleep: func(context.Context, time.Duration) error { return nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	start := time.Now()
	if _, err := c.SyncGame(context.Background(), "203.0.113.7"); err == nil {
		t.Fatal("a hung endpoint reported success")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want a deadline", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("the call took %v — dns.timeout did not bound it", took)
	}
}

func TestBackoffGrowsAndStops(t *testing.T) {
	c := &Cloudflare{retryWait: time.Second}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
	for i, w := range want {
		if got := c.backoff(i + 1); got != w {
			t.Errorf("backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
	// Capped, so that a retry sequence still fits inside the patience of whoever is
	// waiting on the deploy.
	for _, attempt := range []int{6, 7, 20} {
		if got := c.backoff(attempt); got != 30*time.Second {
			t.Errorf("backoff(%d) = %v, want the 30s cap", attempt, got)
		}
	}
}

func TestRetryable(t *testing.T) {
	for _, status := range []int{408, 429, 500, 502, 503, 504} {
		if !retryable(status) {
			t.Errorf("%d should be retried", status)
		}
	}
	// 403 is a token without the scope, 400 a malformed request, 404 a record that
	// is not there: none of them change because we asked twice.
	for _, status := range []int{400, 401, 403, 404, 405, 409, 422} {
		if retryable(status) {
			t.Errorf("%d should not be retried", status)
		}
	}
}

func TestRetryAfterHeader(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"", 0, false},
		{"5", 5 * time.Second, true},
		{" 30 ", 30 * time.Second, true},
		{"0", 0, true},
		{"-1", 0, false},
		{"soon", 0, false},
	}
	for _, tc := range cases {
		h := http.Header{}
		if tc.in != "" {
			h.Set("Retry-After", tc.in)
		}
		got, ok := retryAfter(h)
		if ok != tc.ok || (tc.ok && got != tc.want) {
			t.Errorf("retryAfter(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
	// An HTTP-date is the other form the header takes.
	h := http.Header{}
	h.Set("Retry-After", time.Now().Add(time.Minute).UTC().Format(http.TimeFormat))
	got, ok := retryAfter(h)
	if !ok || got < 50*time.Second || got > time.Minute {
		t.Errorf("retryAfter(a date a minute out) = %v, %v", got, ok)
	}
}

func TestSplitTarget(t *testing.T) {
	cases := []struct {
		in   string
		host string
		port int
		bad  bool
	}{
		{in: "http://provider.example:32100", host: "provider.example", port: 32100},
		{in: "https://provider.example", host: "provider.example", port: 443},
		{in: "http://provider.example", host: "provider.example", port: 80},
		{in: "provider.example:32100", host: "provider.example", port: 32100},
		// A bare host has no port, and http is the assumption v1 made too.
		{in: "provider.example", host: "provider.example", port: 80},
		{in: "203.0.113.20:32100", host: "203.0.113.20", port: 32100},
		{in: " https://provider.example:8443/status ", host: "provider.example", port: 8443},
		{in: "", bad: true},
		{in: "   ", bad: true},
		{in: "http://:32100", bad: true},
		{in: "http://host:0", bad: true},
		{in: "http://host:99999", bad: true},
	}
	for _, tc := range cases {
		host, port, err := splitTarget(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("splitTarget(%q) = %q, %d; want an error", tc.in, host, port)
			}
			continue
		}
		if err != nil {
			t.Errorf("splitTarget(%q): %v", tc.in, err)
			continue
		}
		if host != tc.host || port != tc.port {
			t.Errorf("splitTarget(%q) = %q, %d; want %q, %d", tc.in, host, port, tc.host, tc.port)
		}
	}
}

func TestRecordType(t *testing.T) {
	cases := map[string]string{
		"203.0.113.7":            "A",
		"2001:db8::1":            "AAAA",
		"::1":                    "AAAA",
		"provider.example":       "CNAME",
		"host.provider.example.": "CNAME",
		// An IPv4-mapped IPv6 literal is still an IPv4 address, and Cloudflare wants
		// an A for it.
		"::ffff:203.0.113.7": "A",
	}
	for in, want := range cases {
		if got := recordType(in); got != want {
			t.Errorf("recordType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCodeAndStatusIgnoreOtherErrors(t *testing.T) {
	err := errors.New("something else entirely")
	if Code(err, 81044) || Status(err) != 0 {
		t.Error("Code/Status claimed to know something about a non-API error")
	}
	// Wrapped, which is how these arrive from upsert and setSetting.
	wrapped := errors.Join(errors.New("first"), &APIError{Status: 404, Messages: []apiMessage{{Code: 81044}}})
	if !Code(wrapped, 81044) {
		t.Error("Code does not see through a join")
	}
	if Status(wrapped) != 404 {
		t.Error("Status does not see through a join")
	}
}

func TestSnipKeepsAMessageToOneLine(t *testing.T) {
	if got := snip("  a\nb\tc\r\n  "); got != "a b c" {
		t.Errorf("snip = %q", got)
	}
	long := strings.Repeat("x", maxErrorBody+100)
	if got := snip(long); len(got) != maxErrorBody+len("…") {
		t.Errorf("snip did not bound a long body: %d chars", len(got))
	}
}
