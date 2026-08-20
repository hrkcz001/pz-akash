package akash

// The test harness: a fake Console API and a clock that only moves when the code
// under test sleeps.
//
// Both exist for the same reason. Everything in this package either spends money
// or waits on a deadline, so the tests have to be able to drive a whole deploy —
// bids arriving late, a provider that leases and then never becomes routable, a
// lease that vanishes — without a network and without wall-clock time. A polling
// loop tested against real time is a test that takes minutes and passes by luck.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/secrets"
)

// realConfigPath is the config that actually ships. Testing the driver against it
// rather than a hand-built struct means a config edit that would break a deploy
// breaks a test first.
const realConfigPath = "../../config.yaml"

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	c, err := config.Load(realConfigPath)
	if err != nil {
		t.Fatalf("loading %s: %v", realConfigPath, err)
	}
	return c
}

// handler answers one request. It takes the whole request because a test may need
// the path — Adopt asks about several deployments through one prefix — and the
// body, to assert on what was posted.
type handler func(r *http.Request, body []byte) (int, string)

// fakeAPI is a stand-in for the Console API. Handlers are registered per path
// prefix and may be replaced mid-test, which is how "the third poll is the one
// that answers" gets expressed.
type fakeAPI struct {
	t *testing.T

	mu       sync.Mutex
	handlers map[string]handler
	// headers is sent with every response; see header().
	headers map[string]string
	calls   []string
	// bodies records what was sent, so a test can assert on the SDL that was
	// posted rather than only on the fact that something was.
	bodies []recorded

	srv *httptest.Server
}

type recorded struct {
	call string
	body []byte
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{t: t, handlers: map[string]handler{}}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

// on registers a handler for a method and path prefix, e.g. on("GET", "/v1/bids").
func (f *fakeAPI) on(method, prefix string, h handler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[method+" "+prefix] = h
}

// json registers a handler that always answers 200 with the same document.
func (f *fakeAPI) json(method, prefix, doc string) {
	f.on(method, prefix, func(*http.Request, []byte) (int, string) { return 200, doc })
}

// fail registers a handler that always answers one status and body.
func (f *fakeAPI) fail(method, prefix string, status int, doc string) {
	f.on(method, prefix, func(*http.Request, []byte) (int, string) { return status, doc })
}

// header sets a response header sent with every answer. Retry-After is the one
// that matters: it is the API telling us how long to wait, and the handler
// signature has no way to say it.
func (f *fakeAPI) header(k, v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.headers == nil {
		f.headers = map[string]string{}
	}
	f.headers[k] = v
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
	}
	// The Console API is authenticated by api key; a provider's own lease endpoint
	// is authenticated by a scoped JWT and would reject an api key. Asserting per
	// prefix is how a test catches the credential going to the wrong host.
	switch {
	case strings.HasPrefix(r.URL.Path, "/v1/"):
		if r.Header.Get("x-api-key") == "" {
			f.t.Errorf("%s %s carried no x-api-key header", r.Method, r.URL.Path)
		}
	case strings.HasPrefix(r.URL.Path, "/lease/"):
		if r.Header.Get("x-api-key") != "" {
			f.t.Errorf("%s %s leaked the console api key to a provider", r.Method, r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			f.t.Errorf("%s %s carried no bearer token", r.Method, r.URL.Path)
		}
	}

	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI())
	f.bodies = append(f.bodies, recorded{call: r.Method + " " + r.URL.RequestURI(), body: body})
	// Longest matching prefix wins, so "/v1/deployments/123" can be handled
	// separately from "/v1/deployments".
	var (
		best   handler
		bestSz int
	)
	for key, h := range f.handlers {
		method, prefix, _ := strings.Cut(key, " ")
		if method != r.Method || !strings.HasPrefix(r.URL.Path, prefix) {
			continue
		}
		if len(prefix) >= bestSz {
			best, bestSz = h, len(prefix)
		}
	}
	extra := make(map[string]string, len(f.headers))
	for k, v := range f.headers {
		extra[k] = v
	}
	f.mu.Unlock()

	if best == nil {
		f.t.Errorf("unexpected call %s %s", r.Method, r.URL.RequestURI())
		http.Error(w, `{"error":"no handler"}`, http.StatusNotImplemented)
		return
	}
	status, doc := best(r, body)
	w.Header().Set("Content-Type", "application/json")
	for k, v := range extra {
		w.Header().Set(k, v)
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(doc))
}

// url is the fake's base URL. Tests put it in a provider fixture's hostUri so the
// direct provider query lands here too.
func (f *fakeAPI) url() string { return f.srv.URL }

// countCalls returns how many requests matched a method and path prefix.
func (f *fakeAPI) countCalls(method, prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, method+" "+prefix) {
			n++
		}
	}
	return n
}

func (f *fakeAPI) lastBody(t *testing.T, into any, method, prefix string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.bodies) - 1; i >= 0; i-- {
		if strings.HasPrefix(f.bodies[i].call, method+" "+prefix) {
			if err := json.Unmarshal(f.bodies[i].body, into); err != nil {
				t.Fatalf("decoding the recorded %s body: %v", prefix, err)
			}
			return
		}
	}
	t.Fatalf("no %s %s call was recorded", method, prefix)
}

// clock is a fake clock that advances only when the code under test sleeps. A
// polling loop therefore reaches its deadline in exactly the number of polls the
// configuration implies, with no real waiting and no flakiness.
type clock struct {
	mu    sync.Mutex
	now   time.Time
	slept time.Duration
}

// testEpoch is where the fake clock starts. A fixed instant in a zone that is not
// the machine's: a timestamp that only looks right on the developer's laptop is a
// bug this project already had once.
var testEpoch = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

func newClock() *clock {
	return &clock{now: testEpoch}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d > 0 {
		c.now = c.now.Add(d)
		c.slept += d
	}
	return nil
}

// newTestDriver wires a Driver against the fake API and the fake clock.
func newTestDriver(t *testing.T, f *fakeAPI, cfg *config.Config) (*Driver, *clock) {
	t.Helper()
	if cfg == nil {
		cfg = testConfig(t)
	}
	cfg.Akash.APIBase = f.srv.URL

	cl, err := New(Options{
		APIBase: f.srv.URL,
		APIKey:  "test-key",
		Logf:    func(string, ...any) {},
		Retries: 1,
		sleep:   func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	clk := newClock()
	d, err := NewDriver(DriverOptions{
		Client:  cl,
		Cfg:     cfg,
		Secrets: secrets.Placeholders(),
		Logf:    func(string, ...any) {},
		Now:     clk.Now,
		Sleep:   clk.sleep,
	})
	if err != nil {
		t.Fatal(err)
	}
	return d, clk
}
