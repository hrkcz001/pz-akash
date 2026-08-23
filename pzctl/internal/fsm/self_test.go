package fsm

// Publishing the direct route.
//
// The mechanism is a network call on a housekeeping tick whose answer lands in a
// document field, so there are two separate requirements and both have teeth. The
// answer has to reach state.URLs.Direct(), because that is the route a backup upload
// takes and Cloudflare cannot carry one. And the asking has to stop — once on
// success, once per interval on failure — because an unbounded lookup on every tick
// is an API call every few seconds for the life of the lease.
//
// The zone name is asserted untouched in the same breath as the direct route,
// because the two are not alternatives: people use the name, it survives a redeploy,
// and only bulk traffic wants the host:port.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
)

// selfLooker is a driver whose own address is under the test's control. It counts
// calls, which is half of what these tests assert.
type selfLooker struct {
	*DryRun
	mu    sync.Mutex
	url   string
	err   error
	calls int
}

func (s *selfLooker) SelfURL(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.url, s.err
}

// answer changes what the next lookup finds, which is how the recovery case is
// written: an API that was down comes back.
func (s *selfLooker) answer(url string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.url, s.err = url, err
}

func (s *selfLooker) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// selfHarness wraps the harness's DryRun rather than replacing it, so everything
// else about the machine — escrow, deploys, adoption — still behaves.
func selfHarness(t *testing.T, look *selfLooker, tune func(*config.Config), opts ...func(*Deps)) *harness {
	t.Helper()
	opts = append(opts, func(d *Deps) {
		look.DryRun = d.Akash.(*DryRun)
		d.Akash = look
	})
	return newHarness(t, tune, opts...)
}

const discovered = "http://provider.example:31688"

// TestTheDiscoveredAddressIsPublishedAsTheDirectRoute is the whole feature in one
// assertion: an address nothing could have configured ends up where the agent looks
// for the route that bypasses the proxy.
func TestTheDiscoveredAddressIsPublishedAsTheDirectRoute(t *testing.T) {
	look := &selfLooker{url: discovered}
	h := selfHarness(t, look, dnsOn)

	// A poll publishes the zone name and asks nothing: discovery is housekeeping.
	h.poll()
	if got := h.published().URLs.Raw; got != "" {
		t.Fatalf("raw = %q before any lookup, want empty", got)
	}
	if n := look.count(); n != 0 {
		t.Fatalf("a poll made %d lookups, want 0 — a network call does not belong in advance", n)
	}

	h.tick()

	got := h.published().URLs
	if got.Raw != discovered {
		t.Errorf("raw = %q, want the discovered address %q", got.Raw, discovered)
	}
	if got.Direct() != discovered {
		t.Errorf("Direct() = %q, want %q — this is the route a backup upload takes", got.Direct(), discovered)
	}
	// The name is what people use and what survives a redeploy. Discovery adds a
	// route; it must not take one away.
	if got.Public != "https://vsrania.online" {
		t.Errorf("public = %q, want the zone name left alone", got.Public)
	}
	if got.Base() != "https://vsrania.online" {
		t.Errorf("Base() = %q, want the zone name", got.Base())
	}
	if !h.logged("direct route to this controller is") {
		h.dumpLogs()
		t.Error("nothing logged about the direct route; an operator cannot tell whether the bypass is active")
	}
}

// TestTheLookupStopsOnceItHasAnAnswer: the address of a lease does not change while
// the lease lives, so a second question has no possible new answer.
func TestTheLookupStopsOnceItHasAnAnswer(t *testing.T) {
	look := &selfLooker{url: discovered}
	h := selfHarness(t, look, dnsOn)

	h.tick()
	h.clk.add(10 * selfLookInterval)
	h.tick()
	h.tick()

	if n := look.count(); n != 1 {
		t.Errorf("asked %d times, want 1", n)
	}
}

// TestTheOverrideFlagIsNotSecondGuessed: --controller-url is a deliberate statement
// about exactly this value. Asking the API to check it would be both wasteful and
// capable of overruling the operator.
func TestTheOverrideFlagIsNotSecondGuessed(t *testing.T) {
	look := &selfLooker{url: discovered}
	h := selfHarness(t, look, dnsOn,
		func(d *Deps) { d.ControllerURL = "http://operator.example:31293" })

	h.tick()

	if n := look.count(); n != 0 {
		t.Errorf("asked %d times with an override in force, want 0", n)
	}
	if got := h.published().URLs.Raw; got != "http://operator.example:31293" {
		t.Errorf("raw = %q, want the operator's value", got)
	}
}

// TestAFailedLookupDegradesAndIsRetriedOnAnInterval: the published name still works
// for everything except a very large upload, so a broken lookup must cost the
// bypass and nothing else — and must not turn into an API call every few seconds.
func TestAFailedLookupDegradesAndIsRetriedOnAnInterval(t *testing.T) {
	look := &selfLooker{err: errors.New("deployment list unavailable")}
	h := selfHarness(t, look, dnsOn)

	h.tick()
	h.tick()
	h.tick()
	if n := look.count(); n != 1 {
		t.Errorf("asked %d times across three ticks, want 1 — the interval is what bounds it", n)
	}
	got := h.published().URLs
	if got.Raw != "" {
		t.Errorf("raw = %q after a failed lookup, want empty", got.Raw)
	}
	if got.Base() != "https://vsrania.online" {
		t.Errorf("Base() = %q, want the zone name still usable", got.Base())
	}

	// The API comes back.
	h.clk.add(selfLookInterval + time.Second)
	look.answer(discovered, nil)
	h.tick()

	if n := look.count(); n != 2 {
		t.Errorf("asked %d times after the interval elapsed, want 2", n)
	}
	if got := h.published().URLs.Direct(); got != discovered {
		t.Errorf("Direct() = %q after recovery, want %q", got, discovered)
	}
}

// TestAnEmptyAnswerPublishesNothingAndKeepsAsking: "" is the ordinary answer while a
// lease is not routable yet, and on a laptop. It is not a failure, and it is not a
// value — writing it would advertise an address of "" to the agent.
func TestAnEmptyAnswerPublishesNothingAndKeepsAsking(t *testing.T) {
	look := &selfLooker{} // "" and a nil error
	h := selfHarness(t, look, dnsOn)

	h.tick()
	if got := h.published().URLs.Raw; got != "" {
		t.Errorf("raw = %q, want empty", got)
	}
	if h.logged("direct route to this controller is") {
		h.dumpLogs()
		t.Error("an empty answer was announced as a direct route")
	}

	h.clk.add(selfLookInterval + time.Second)
	h.tick()
	if n := look.count(); n != 2 {
		t.Errorf("asked %d times, want 2 — an unroutable lease becomes routable", n)
	}
}

// TestWithNoZoneTheDiscoveredAddressIsTheOnlyAddress: this is the case the warning in
// resolveURLs was written for. Discovery answers it without an operator flag, and the
// answer has to reach both the branch and the SDL handed to the server.
func TestWithNoZoneTheDiscoveredAddressIsTheOnlyAddress(t *testing.T) {
	look := &selfLooker{url: discovered}
	h := selfHarness(t, look, func(c *config.Config) { c.DNS.Enabled = false })

	h.tick()

	got := h.published().URLs
	if got.Base() != discovered {
		t.Errorf("Base() = %q, want the discovered address — an agent has nowhere else to look", got.Base())
	}
	if got.Direct() != discovered {
		t.Errorf("Direct() = %q, want %q", got.Direct(), discovered)
	}
	// Promoted rather than left in Raw with an empty Public, which is what would make
	// Base() empty and the whole document useless.
	if got.Public != discovered || got.Raw != "" {
		t.Errorf("public = %q raw = %q, want the address promoted to public", got.Public, got.Raw)
	}
	// The agent reads the branch; the server's SDL is built from controllerURL(). A
	// disagreement here is a container told to talk to an address the branch denies.
	if got, want := h.m.controllerURL(), h.published().URLs.Base(); got != want {
		t.Errorf("SDL would get %q but the branch says %q", got, want)
	}
}
