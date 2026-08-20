package webhook

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The receiver's job is small and its failure modes are asymmetric, which is what
// these tests are shaped around.
//
// Letting a delivery through that should have been dropped is the halt loop: v1's
// state writes landed on the same branch as its triggers, so every status update
// arrived back as an operator request. Dropping a delivery that should have been
// let through is merely slow, because the poll loop reaches the same conclusion
// within controller.poll.idle. So the ref and path filters are tested for what they
// refuse, and the fallbacks — no file list, unparseable payload — are tested for
// erring towards acting.
//
// The one place where leniency is not allowed is the signature: this endpoint can
// start a funded deployment.

const secret = "s3cr3t"

// spy collects dispatched pushes. The handler calls OnPush from the HTTP
// goroutine, so this is locked.
type spy struct {
	mu   sync.Mutex
	got  []Push
	logs []string
}

func (s *spy) onPush(p Push) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, p)
}

func (s *spy) logf(f string, a ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logs = append(s.logs, f)
	_ = a
}

func (s *spy) pushes() []Push {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Push(nil), s.got...)
}

// newHandler builds a handler with the shape this deployment actually uses: the
// operator branch is main, triggers live in triggers/, and no author is filtered.
func newHandler(t *testing.T, tune func(*Options)) (*Handler, *spy) {
	t.Helper()
	s := &spy{}
	o := Options{
		Secret: secret, Branch: "main", TriggersDir: "triggers",
		OnPush: s.onPush, Logf: s.logf,
	}
	if tune != nil {
		tune(&o)
	}
	h, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h, s
}

// post signs a body and delivers it, the way GitHub does.
func post(t *testing.T, h *Handler, event string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, Path, strings.NewReader(string(body)))
	if event != "" {
		req.Header.Set("X-GitHub-Event", event)
	}
	req.Header.Set("X-GitHub-Delivery", "00000000-0000-0000-0000-000000000000")
	req.Header.Set("X-Hub-Signature-256", Sign(secret, body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// pushBody builds a payload in GitHub's shape. Constructing it through the same
// struct the parser reads would let a field-name mistake pass; this is the wire
// form, spelled out.
func pushBody(ref, pusher, author string, paths ...string) []byte {
	commit := map[string]any{
		"author":   map[string]any{"name": author, "username": author},
		"added":    paths,
		"modified": []string{},
		"removed":  []string{},
	}
	b, err := json.Marshal(map[string]any{
		"ref":         ref,
		"before":      "1111111111111111111111111111111111111111",
		"after":       "2222222222222222222222222222222222222222",
		"created":     false,
		"deleted":     false,
		"pusher":      map[string]any{"name": pusher},
		"head_commit": commit,
		"commits":     []any{commit},
	})
	if err != nil {
		panic(err)
	}
	return b
}

// --- construction ---

func TestNewRefusesAnUnauthenticatedEndpoint(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		o    Options
	}{
		{"no secret", Options{Branch: "main", TriggersDir: "triggers"}},
		{"no branch", Options{Secret: secret, TriggersDir: "triggers"}},
		{"no triggers dir", Options{Secret: secret, Branch: "main"}},
	} {
		if _, err := New(tc.o); err == nil {
			t.Errorf("%s: New accepted it", tc.name)
		}
	}
}

// --- signatures ---

func TestVerifyAcceptsWhatSignProduces(t *testing.T) {
	t.Parallel()
	body := pushBody("refs/heads/main", "op", "op", "triggers/start")
	if err := Verify(secret, Sign(secret, body), body); err != nil {
		t.Fatalf("a signature we produced did not verify: %v", err)
	}
	// GitHub writes the algorithm in lower case; accepting either is intentional
	// leniency about the label, not about the digest.
	if err := Verify(secret, "SHA256="+strings.TrimPrefix(Sign(secret, body), "sha256="), body); err != nil {
		t.Fatalf("uppercase algo label rejected: %v", err)
	}
}

func TestVerifyRejectsEverythingElse(t *testing.T) {
	t.Parallel()
	body := []byte(`{"ref":"refs/heads/main"}`)
	good := Sign(secret, body)

	for _, tc := range []struct {
		name, header string
		secret       string
		body         []byte
	}{
		{"missing header", "", secret, body},
		{"no algo prefix", strings.TrimPrefix(good, "sha256="), secret, body},
		{"wrong algo", "sha1=" + strings.TrimPrefix(good, "sha256="), secret, body},
		{"not hex", "sha256=nothexatall", secret, body},
		{"truncated digest", good[:len(good)-2], secret, body},
		{"wrong secret", Sign("other", body), secret, body},
		{"tampered body", good, secret, []byte(`{"ref":"refs/heads/main","x":1}`)},
		{"empty digest", "sha256=", secret, body},
	} {
		if err := Verify(tc.secret, tc.header, tc.body); err == nil {
			t.Errorf("%s: Verify accepted it", tc.name)
		}
	}
}

// --- payload parsing ---

func TestParsePushUnionsAndDedupesPaths(t *testing.T) {
	t.Parallel()
	body := []byte(`{
	  "ref":"refs/heads/main","after":"abc","pusher":{"name":"op"},
	  "head_commit":{"author":{"name":"Operator","username":"op"},"added":["triggers/halt"]},
	  "commits":[
	    {"added":["triggers/halt"],"modified":["config.yaml"],"removed":[]},
	    {"added":[],"modified":["config.yaml"],"removed":["triggers/start"]}
	  ]
	}`)
	got, err := ParsePush(body)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"triggers/halt", "config.yaml", "triggers/start"}
	if strings.Join(got.Paths, ",") != strings.Join(want, ",") {
		t.Errorf("paths = %v, want %v (union across commits, in order, deduped)", got.Paths, want)
	}
	if got.Author != "op" {
		t.Errorf("author = %q, want the head commit's username", got.Author)
	}
	if got.Pusher != "op" {
		t.Errorf("pusher = %q", got.Pusher)
	}
}

func TestParsePushFallsBackToTheAuthorName(t *testing.T) {
	t.Parallel()
	// A commit made by an identity with no GitHub account has a name and no
	// username. Dropping the author entirely would silently disable IgnoreAuthors
	// for exactly the identity a bot commit is most likely to have.
	body := []byte(`{"ref":"refs/heads/main",
	  "head_commit":{"author":{"name":"pz-controller"}}}`)
	got, err := ParsePush(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.Author != "pz-controller" {
		t.Errorf("author = %q, want the name when username is absent", got.Author)
	}
}

func TestParsePushRefusesAPayloadWithNoRef(t *testing.T) {
	t.Parallel()
	// Not a push. Failing here is what routes it to the reconcile-anyway path,
	// rather than letting a Push with an empty ref reach Decide and be dropped as
	// "not the operator branch" — a silent miss instead of a slow one.
	if _, err := ParsePush([]byte(`{"zen":"Design for failure."}`)); err == nil {
		t.Fatal("ParsePush accepted a payload with no ref")
	}
	if _, err := ParsePush([]byte(`{`)); err == nil {
		t.Fatal("ParsePush accepted malformed JSON")
	}
}
