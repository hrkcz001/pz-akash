package webhook

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- the filter chain ---

func TestDecide(t *testing.T) {
	t.Parallel()
	h, _ := newHandler(t, nil)

	for _, tc := range []struct {
		name string
		push Push
		act  bool
		// reason is a substring the decision must explain itself with, so a rule
		// cannot pass by returning the right answer for the wrong reason.
		reason string
	}{
		{
			name: "trigger added on the operator branch",
			push: Push{Ref: "refs/heads/main", Paths: []string{"triggers/start"}},
			act:  true, reason: "triggers/start",
		},
		{
			name: "bare branch name is accepted as a ref",
			push: Push{Ref: "main", Paths: []string{"triggers/halt"}},
			act:  true, reason: "triggers/halt",
		},
		{
			// This is bug 2's webhook half. Every controller publish pushes this
			// branch; in v1 the two shared one branch and each status write looked
			// like a fresh operator request.
			name:   "controller state branch",
			push:   Push{Ref: "refs/heads/state/controller", Paths: []string{"controller.json"}},
			reason: "is not the operator branch",
		},
		{
			name:   "agent state branch",
			push:   Push{Ref: "refs/heads/state/agent", Paths: []string{"agent.json"}},
			reason: "is not the operator branch",
		},
		{
			name:   "unrelated paths on the right branch",
			push:   Push{Ref: "refs/heads/main", Paths: []string{"config.yaml", "README.md"}},
			reason: "no paths under triggers/ changed",
		},
		{
			// A path that merely starts with the directory's name is not inside it.
			name:   "prefix collision",
			push:   Push{Ref: "refs/heads/main", Paths: []string{"triggers-old/start"}},
			reason: "no paths under triggers/ changed",
		},
		{
			name:   "branch deletion",
			push:   Push{Ref: "refs/heads/main", Deleted: true, Paths: []string{"triggers/start"}},
			reason: "deletion carries no trigger",
		},
		{
			// A truncated commit list, or a payload shape we did not anticipate.
			// Reading the triggers directory is cheap and idempotent, so act.
			name: "no file list",
			push: Push{Ref: "refs/heads/main"},
			act:  true, reason: "no file list",
		},
		{
			// A trigger removed is a trigger consumed — by us, and the SHA ring
			// drops the echo. But the path filter must not be what drops it: an
			// operator deleting a stale trigger by hand is a real change.
			name: "trigger removed",
			push: Push{Ref: "refs/heads/main", Paths: []string{"triggers/pause"}},
			act:  true, reason: "triggers/pause",
		},
	} {
		got := h.Decide(tc.push)
		if got.Act != tc.act {
			t.Errorf("%s: Act = %v, want %v (reason %q)", tc.name, got.Act, tc.act, got.Reason)
			continue
		}
		if !strings.Contains(got.Reason, tc.reason) {
			t.Errorf("%s: reason = %q, want it to mention %q", tc.name, got.Reason, tc.reason)
		}
	}
}

// TestDecideDoesNotFilterOnAuthorByDefault pins the choice documented on
// Options.IgnoreAuthors. In this deployment the controller commits as the
// operator's own identity, so an author filter enabled by default would drop
// exactly the deliveries the endpoint exists to receive.
func TestDecideDoesNotFilterOnAuthorByDefault(t *testing.T) {
	t.Parallel()
	h, _ := newHandler(t, nil)
	d := h.Decide(Push{
		Ref: "refs/heads/main", Pusher: "hrkcz001", Author: "hrkcz001",
		Paths: []string{"triggers/start"},
	})
	if !d.Act {
		t.Fatalf("a push by the operator's own identity was dropped: %s", d.Reason)
	}
}

func TestDecideHonoursIgnoreAuthorsWhenConfigured(t *testing.T) {
	t.Parallel()
	h, _ := newHandler(t, func(o *Options) {
		o.IgnoreAuthors = []string{" PZ-Controller ", ""}
	})
	// Matched case-insensitively and with the configured value trimmed, because a
	// git identity in a config file is written by hand.
	for _, who := range []Push{
		{Ref: "refs/heads/main", Pusher: "pz-controller", Paths: []string{"triggers/start"}},
		{Ref: "refs/heads/main", Author: "PZ-Controller", Paths: []string{"triggers/start"}},
	} {
		if d := h.Decide(who); d.Act {
			t.Errorf("a push by the ignored identity was acted on: %+v", who)
		}
	}
	if d := h.Decide(Push{Ref: "refs/heads/main", Pusher: "hrkcz001",
		Paths: []string{"triggers/start"}}); !d.Act {
		t.Errorf("the filter dropped an identity it was not given: %s", d.Reason)
	}
}

// --- the HTTP surface ---

func TestServeHTTPRejectsNonPOST(t *testing.T) {
	t.Parallel()
	h, s := newHandler(t, nil)
	req := httptest.NewRequest(http.MethodGet, Path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET returned %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow = %q, want POST", got)
	}
	if n := len(s.pushes()); n != 0 {
		t.Errorf("%d push(es) dispatched from a GET", n)
	}
}

// TestServeHTTPRejectsABadSignature is the one case where the endpoint must be
// strict: it can start a deployment that costs money.
func TestServeHTTPRejectsABadSignature(t *testing.T) {
	t.Parallel()
	h, s := newHandler(t, nil)
	body := pushBody("refs/heads/main", "op", "op", "triggers/start")

	for _, tc := range []struct{ name, sig string }{
		{"absent", ""},
		{"wrong secret", Sign("not-the-secret", body)},
		{"garbage", "sha256=zz"},
	} {
		req := httptest.NewRequest(http.MethodPost, Path, strings.NewReader(string(body)))
		req.Header.Set("X-GitHub-Event", "push")
		if tc.sig != "" {
			req.Header.Set("X-Hub-Signature-256", tc.sig)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s signature returned %d, want 401", tc.name, rec.Code)
		}
	}
	if n := len(s.pushes()); n != 0 {
		t.Fatalf("%d unauthenticated push(es) reached the controller", n)
	}
}

func TestServeHTTPAnswersPingWithoutDispatching(t *testing.T) {
	t.Parallel()
	h, s := newHandler(t, nil)
	rec := post(t, h, "ping", []byte(`{"zen":"Non-blocking is better than blocking.","hook_id":1}`))
	if rec.Code != http.StatusOK {
		t.Errorf("ping returned %d, want 200 — the settings page shows a red cross", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "pong") {
		t.Errorf("ping body = %q", rec.Body.String())
	}
	if n := len(s.pushes()); n != 0 {
		t.Errorf("a ping dispatched %d push(es)", n)
	}
}

func TestServeHTTPRefusesAnOversizePayload(t *testing.T) {
	t.Parallel()
	h, s := newHandler(t, func(o *Options) { o.MaxBody = 64 })
	body := []byte(`{"ref":"refs/heads/main","pad":"` + strings.Repeat("x", 200) + `"}`)
	// Correctly signed, so this tests the limit rather than the signature. The
	// limit is applied first precisely so an unauthenticated caller cannot make us
	// allocate in order to be told to go away.
	rec := post(t, h, "push", body)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize payload returned %d, want 413", rec.Code)
	}
	if n := len(s.pushes()); n != 0 {
		t.Errorf("an oversize payload dispatched %d push(es)", n)
	}
}

func TestServeHTTPIgnoresOtherEvents(t *testing.T) {
	t.Parallel()
	h, s := newHandler(t, nil)
	rec := post(t, h, "issues", []byte(`{"action":"opened"}`))
	if rec.Code != http.StatusAccepted {
		t.Errorf("an issues event returned %d, want 202", rec.Code)
	}
	if n := len(s.pushes()); n != 0 {
		t.Errorf("an issues event dispatched %d push(es)", n)
	}
}

// TestServeHTTPActsOnASignedButUnparseablePayload is the deliberate asymmetry: a
// shape we failed to anticipate must cost latency, not a lost trigger.
func TestServeHTTPActsOnASignedButUnparseablePayload(t *testing.T) {
	t.Parallel()
	h, s := newHandler(t, nil)
	rec := post(t, h, "push", []byte(`{"ref":`))
	if rec.Code != http.StatusOK {
		t.Errorf("returned %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "reconciling anyway") {
		t.Errorf("body = %q, want it to say it is reconciling anyway", rec.Body.String())
	}
	got := s.pushes()
	if len(got) != 1 {
		t.Fatalf("dispatched %d push(es), want 1", len(got))
	}
	// The synthesised push must name the operator branch, or Decide would drop it
	// on the way back out.
	if got[0].Ref != "refs/heads/main" {
		t.Errorf("synthesised ref = %q, want the operator branch", got[0].Ref)
	}
	if d := h.Decide(got[0]); !d.Act {
		t.Errorf("the synthesised push does not survive its own filter chain: %s", d.Reason)
	}
}

func TestServeHTTPDispatchesATriggerPush(t *testing.T) {
	t.Parallel()
	h, s := newHandler(t, nil)
	body := pushBody("refs/heads/main", "hrkcz001", "hrkcz001", "triggers/start")
	rec := post(t, h, "push", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("returned %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "acting") {
		t.Errorf("body = %q, want it to say it is acting", rec.Body.String())
	}
	got := s.pushes()
	if len(got) != 1 {
		t.Fatalf("dispatched %d push(es), want 1", len(got))
	}
	// After is what the controller records in its dedup ring, so a delivery that
	// loses it would defeat the durable half of the self-push filter.
	if got[0].After != "2222222222222222222222222222222222222222" {
		t.Errorf("After = %q, want the payload's head SHA", got[0].After)
	}
}

// TestServeHTTPAcceptsOurOwnSmokeTest covers the missing-event-header path, which
// `pzctl controller --self-test` uses to prove the endpoint is reachable and the
// secret matches without inventing a GitHub header.
func TestServeHTTPAcceptsOurOwnSmokeTest(t *testing.T) {
	t.Parallel()
	h, s := newHandler(t, nil)
	rec := post(t, h, "", pushBody("refs/heads/main", "op", "op", "triggers/start"))
	if rec.Code != http.StatusOK {
		t.Fatalf("returned %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if n := len(s.pushes()); n != 1 {
		t.Errorf("dispatched %d push(es), want 1", n)
	}
}

// TestServeHTTPDropsAStateBranchDeliveryEndToEnd is bug 2's loop, closed at the
// HTTP boundary rather than at Decide: a correctly signed delivery for a branch we
// publish must reach nothing.
func TestServeHTTPDropsAStateBranchDeliveryEndToEnd(t *testing.T) {
	t.Parallel()
	h, s := newHandler(t, nil)
	rec := post(t, h, "push",
		pushBody("refs/heads/state/controller", "hrkcz001", "hrkcz001", "controller.json"))
	if rec.Code != http.StatusOK {
		t.Errorf("returned %d, want 200 — GitHub must not retry it", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ignored") {
		t.Errorf("body = %q, want it to say the delivery was ignored", rec.Body.String())
	}
	if n := len(s.pushes()); n != 0 {
		t.Fatalf("a state-branch push dispatched %d event(s) — this is the halt loop", n)
	}
}
