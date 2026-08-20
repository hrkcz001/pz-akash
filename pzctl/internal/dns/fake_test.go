package dns

// A fake Cloudflare, faithful to the parts of the v4 API this package drives: the
// success envelope, the record store, the zone settings, and the phase entrypoint
// rulesets. It records every call, because half of what these tests assert is that
// a sync which changes nothing writes nothing.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
)

const testZoneID = "zone123"

type fakeCF struct {
	t  *testing.T
	mu sync.Mutex

	srv *httptest.Server

	// records is the zone's record store, keyed by id. Values carry a Type this
	// package never writes (MX, TXT) so the tests can prove those survive.
	records map[string]record
	nextID  int

	settings map[string]string
	rulesets map[string][]map[string]any
	zones    []Zone

	// calls is every request, as "METHOD /path"; bodies is the raw request body of
	// each, index for index. sleeps is every backoff the client was asked to take.
	calls  []string
	bodies []string
	sleeps []time.Duration
	// fail, when set, decides the response for a call. Returning a non-zero status
	// short-circuits the handler. It runs outside the fake's lock, so a hook with
	// state of its own has to carry its own.
	fail func(method, path string) (status int, body string)
	// retryAfter, when set, is sent with every fail-hook response. Cloudflare sends
	// it with a 429, and honouring it rather than our own backoff is the difference
	// between waiting out a rate limit and extending it.
	retryAfter string
}

func newFakeCF(t *testing.T) *fakeCF {
	f := &fakeCF{
		t:       t,
		records: map[string]record{},
		settings: map[string]string{
			"ssl":            "off",
			"browser_check":  "on",
			"security_level": "medium",
		},
		rulesets: map[string][]map[string]any{},
		zones: []Zone{
			{ID: testZoneID, Name: "vsrania.online", Status: "active"},
		},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

// client builds a Cloudflare client pointed at the fake, with the zone block the
// caller wants and a sleep that records what it was asked to wait rather than
// waiting: the retry policy is worth asserting, the wall-clock is not.
func (f *fakeCF) client(t *testing.T, zone config.DNS) *Cloudflare {
	zone.APIBase = f.srv.URL
	c, err := New(Options{
		Zone:  zone,
		Token: "test-token",
		Logf:  func(string, ...any) {},
		sleep: func(_ context.Context, d time.Duration) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.sleeps = append(f.sleeps, d)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c == nil {
		t.Fatal("New returned no client for an enabled zone")
	}
	return c
}

// zone is the shipping dns: block, with the API base filled in by client().
func testZone() config.DNS {
	return config.DNS{
		Enabled:       true,
		Provider:      "cloudflare",
		Domain:        "vsrania.online",
		ZoneID:        testZoneID,
		Proxied:       true,
		SSLMode:       "flexible",
		IncludeWWW:    true,
		GameRecord:    "pz",
		GameTTL:       60,
		RelaxSecurity: true,
		Timeout:       config.Duration(5 * time.Second),
		Retries:       3,
		RetryWait:     config.Duration(time.Millisecond),
	}
}

// --- assertions ---

func (f *fakeCF) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// writes returns the calls that changed something.
func (f *fakeCF) writes() []string {
	var out []string
	for _, c := range f.log() {
		switch {
		case strings.HasPrefix(c, "POST "), strings.HasPrefix(c, "PUT "),
			strings.HasPrefix(c, "PATCH "), strings.HasPrefix(c, "DELETE "):
			out = append(out, c)
		}
	}
	return out
}

// onFail installs the hook that decides a call's response, and the Retry-After it
// answers with. Locked, because a test that changes either between requests would
// otherwise be a data race with a handler goroutine still finishing the previous one.
func (f *fakeCF) onFail(fn func(method, path string) (int, string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = fn
}

func (f *fakeCF) sendRetryAfter(v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retryAfter = v
}

func (f *fakeCF) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
	f.bodies = nil
	f.sleeps = nil
}

// naps returns the backoffs the client took.
func (f *fakeCF) naps() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.sleeps...)
}

// count returns how many calls match a "METHOD /path" prefix.
func (f *fakeCF) count(prefix string) int {
	n := 0
	for _, c := range f.log() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// bodyOf returns the raw request body of the first call matching "METHOD /path…",
// by prefix, so a test can assert what went over the wire.
func (f *fakeCF) bodyOf(prefix string) string {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return f.bodies[i]
		}
	}
	f.t.Fatalf("no call matching %q in %v", prefix, f.calls)
	return ""
}

// byName returns the records at a name, address records and others alike.
func (f *fakeCF) byName(name string) []record {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []record
	for _, r := range f.records {
		if strings.EqualFold(r.Name, name) {
			out = append(out, r)
		}
	}
	return out
}

// one returns the single record at name, failing the test when there is not
// exactly one. Most of what these tests care about is that a name resolves to one
// thing.
func (f *fakeCF) one(name string) record {
	f.t.Helper()
	got := f.byName(name)
	if len(got) != 1 {
		f.t.Fatalf("%s holds %d records, want 1: %+v", name, len(got), got)
	}
	return got[0]
}

func (f *fakeCF) seed(r record) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("rec%d", f.nextID)
	r.ID = id
	f.records[id] = r
	return id
}

func (f *fakeCF) seedRules(phase string, rules ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rulesets[phase] = rules
}

func (f *fakeCF) rules(phase string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rulesets[phase]
}

func (f *fakeCF) setting(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.settings[name]
}

// --- the handler ---

func (f *fakeCF) handle(w http.ResponseWriter, r *http.Request) {
	// The raw body is kept, not just the decoded one: `proxied` is the field in this
	// package whose absence would be indistinguishable from its correct value, and
	// the only way to assert it was sent is to look at the bytes.
	var raw []byte
	if r.Body != nil {
		raw, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
		r.Body = io.NopCloser(bytes.NewReader(raw))
	}

	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.bodies = append(f.bodies, string(raw))
	fail := f.fail
	retryAfter := f.retryAfter
	f.mu.Unlock()

	if tok := r.Header.Get("Authorization"); tok != "Bearer test-token" {
		// Every call must be authenticated; a package that forgot the header on one
		// path would otherwise pass its tests and fail in production on that path.
		f.reject(w, 403, 9109, "invalid token: "+tok)
		return
	}
	if fail != nil {
		if status, body := fail(r.Method, r.URL.Path); status != 0 {
			w.Header().Set("Content-Type", "application/json")
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.WriteHeader(status)
			fmt.Fprint(w, body)
			return
		}
	}

	path := strings.TrimPrefix(r.URL.Path, "/")
	switch {
	case path == "zones":
		f.ok(w, f.zones)
	case strings.HasPrefix(path, "zones/"+testZoneID+"/dns_records"):
		f.records_(w, r, strings.TrimPrefix(path, "zones/"+testZoneID+"/dns_records"))
	case strings.HasPrefix(path, "zones/"+testZoneID+"/settings/"):
		f.settings_(w, r, strings.TrimPrefix(path, "zones/"+testZoneID+"/settings/"))
	case strings.HasPrefix(path, "zones/"+testZoneID+"/rulesets/phases/"):
		rest := strings.TrimPrefix(path, "zones/"+testZoneID+"/rulesets/phases/")
		f.ruleset_(w, r, strings.TrimSuffix(rest, "/entrypoint"))
	default:
		f.reject(w, 404, 7003, "no route for "+path)
	}
}

func (f *fakeCF) records_(w http.ResponseWriter, r *http.Request, rest string) {
	id := strings.TrimPrefix(rest, "/")
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case r.Method == "GET":
		name := r.URL.Query().Get("name")
		if name == "" {
			f.reject(w, 400, 7003, "the name filter is required by this fake")
			return
		}
		// Cloudflare's name filter is an exact match and returns every type.
		out := []record{}
		for _, rec := range f.records {
			if strings.EqualFold(rec.Name, name) {
				out = append(out, rec)
			}
		}
		f.ok(w, out)

	case r.Method == "POST":
		var in record
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			f.reject(w, 400, 6003, "bad body: "+err.Error())
			return
		}
		if in.Proxied && in.TTL != 1 {
			// The real API's rule, and worth enforcing: a proxied record with an
			// explicit TTL is rejected, and the sync must not depend on that being
			// tolerated.
			f.reject(w, 400, 9041, "ttl must be 1 for a proxied record")
			return
		}
		f.nextID++
		in.ID = fmt.Sprintf("rec%d", f.nextID)
		f.records[in.ID] = in
		f.ok(w, in)

	case r.Method == "PUT":
		if _, ok := f.records[id]; !ok {
			f.reject(w, 404, 81044, "record does not exist")
			return
		}
		var in record
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			f.reject(w, 400, 6003, "bad body: "+err.Error())
			return
		}
		if in.Proxied && in.TTL != 1 {
			f.reject(w, 400, 9041, "ttl must be 1 for a proxied record")
			return
		}
		in.ID = id
		f.records[id] = in
		f.ok(w, in)

	case r.Method == "DELETE":
		if _, ok := f.records[id]; !ok {
			f.reject(w, 404, 81044, "record does not exist")
			return
		}
		delete(f.records, id)
		f.ok(w, map[string]string{"id": id})

	default:
		f.reject(w, 405, 7003, "method "+r.Method)
	}
}

func (f *fakeCF) settings_(w http.ResponseWriter, r *http.Request, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, known := f.settings[name]
	if !known {
		f.reject(w, 404, 1006, "unknown setting "+name)
		return
	}
	switch r.Method {
	case "GET":
		f.ok(w, map[string]any{"id": name, "value": cur, "editable": true})
	case "PATCH":
		var in struct{ Value string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			f.reject(w, 400, 6003, "bad body: "+err.Error())
			return
		}
		f.settings[name] = in.Value
		f.ok(w, map[string]any{"id": name, "value": in.Value, "editable": true})
	default:
		f.reject(w, 405, 7003, "method "+r.Method)
	}
}

func (f *fakeCF) ruleset_(w http.ResponseWriter, r *http.Request, phase string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case "GET":
		rules, ok := f.rulesets[phase]
		if !ok {
			// A zone nobody has written a rule on has no entrypoint ruleset, and the
			// real API answers 404 rather than an empty list.
			f.reject(w, 404, 1002, "ruleset not found")
			return
		}
		f.ok(w, map[string]any{"id": "rs-" + phase, "phase": phase, "rules": rules})
	case "PUT":
		var in struct {
			Rules []map[string]any `json:"rules"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			f.reject(w, 400, 6003, "bad body: "+err.Error())
			return
		}
		for _, rule := range in.Rules {
			// The real API rejects the server-managed fields. This is the assertion
			// that a foreign rule round-tripped through us comes back acceptable.
			for _, k := range []string{"version", "last_updated"} {
				if _, bad := rule[k]; bad {
					f.reject(w, 400, 20041, "rule field "+k+" is read-only")
					return
				}
			}
		}
		if in.Rules == nil {
			in.Rules = []map[string]any{}
		}
		f.rulesets[phase] = in.Rules
		f.ok(w, map[string]any{"id": "rs-" + phase, "phase": phase, "rules": in.Rules})
	default:
		f.reject(w, 405, 7003, "method "+r.Method)
	}
}

// ok writes a success envelope. Callers hold f.mu; nothing in here touches it.
func (f *fakeCF) ok(w http.ResponseWriter, result any) {
	raw, err := json.Marshal(result)
	if err != nil {
		f.t.Fatalf("marshalling a fake result: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"success":true,"errors":[],"messages":[],"result":%s}`, raw)
}

func (f *fakeCF) reject(w http.ResponseWriter, status, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, err := json.Marshal(map[string]any{
		"success":  false,
		"errors":   []map[string]any{{"code": code, "message": msg}},
		"messages": []any{},
		"result":   nil,
	})
	if err != nil {
		f.t.Fatalf("marshalling a fake error: %v", err)
	}
	w.Write(body)
}

// refuse writes the shape that hid v1's failures: HTTP 200 with success false.
func refuse(code int, msg string) string {
	return fmt.Sprintf(`{"success":false,"errors":[{"code":%d,"message":%q}],"result":null}`, code, msg)
}
