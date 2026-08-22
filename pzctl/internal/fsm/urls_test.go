package fsm

import (
	"strings"
	"testing"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// Invariant I15: the agent knows the current controller URL. The controller writes
// it to its state branch every resolve, and the agent reads it from git.
//
// It was specified and not implemented, and the shape of the miss is why these tests
// read the branch through h.published() rather than checking m.doc: the controller
// published three empty URL fields for its whole life, which from the outside looks
// exactly like "not resolved yet". Nothing failed and no log line complained, so the
// first fresh world on the v2 stack paid for a lease before the agent could say
// "PZ_CONTROLLER_URL is unset and the controller has not published its URLs yet".
//
// So these assert the value an agent can actually read, and one of them asserts the
// warning that has to appear when there is no value to publish at all.
//
// h.poll() rather than h.settle() throughout: load() marks the document dirty and
// handle() is what flushes, so a test that only settles asserts on an unpublished
// document — the very confusion above.

func dnsOn(c *config.Config) {
	c.DNS.Enabled = true
	c.DNS.Domain = "vsrania.online"
}

func TestControllerPublishesItsURLFromTheDNSZone(t *testing.T) {
	h := newHarness(t, dnsOn)
	h.poll()

	got := h.published().URLs
	if got.Public != "https://vsrania.online" {
		t.Errorf("published public URL = %q, want https://vsrania.online", got.Public)
	}
	if got.Webhook != "https://vsrania.online/webhook" {
		t.Errorf("published webhook URL = %q", got.Webhook)
	}
	// What the agent actually calls. An empty Base() is the failure being prevented.
	if got.Base() == "" {
		t.Error("Base() is empty; an agent cannot find storage")
	}
}

// https:// regardless of ssl_mode, which describes Cloudflare's hop to our origin
// rather than a client's hop to Cloudflare. An http:// URL here would be published
// to the agent and to the dashboard as the canonical address.
func TestControllerURLIsHTTPSEvenWhenSSLIsFlexible(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		dnsOn(c)
		c.DNS.SSLMode = "flexible"
	})
	h.poll()
	if got := h.published().URLs.Public; !strings.HasPrefix(got, "https://") {
		t.Errorf("public URL = %q, want an https:// URL", got)
	}
}

// With DNS off there is no zone to derive an address from, so --controller-url is
// the only source. It has to end up somewhere Base() can see it.
func TestControllerURLFallsBackToTheFlagWhenDNSIsOff(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.DNS.Enabled = false },
		func(d *Deps) { d.ControllerURL = "http://provider.example:31293" })
	h.poll()

	got := h.published().URLs
	if got.Base() != "http://provider.example:31293" {
		t.Errorf("Base() = %q, want the flag's value", got.Base())
	}
	if got.Webhook != "http://provider.example:31293/webhook" {
		t.Errorf("webhook URL = %q", got.Webhook)
	}
}

// Both present is the interesting case: the zone name survives a redeploy and the
// provider's host:port does not, so the stable name has to win. The override is
// still published, as the route that does not depend on Cloudflare being up.
func TestDNSNameWinsOverTheFlagButTheFlagIsKept(t *testing.T) {
	h := newHarness(t, dnsOn,
		func(d *Deps) { d.ControllerURL = "http://provider.example:31293" })
	h.poll()

	got := h.published().URLs
	if got.Public != "https://vsrania.online" {
		t.Errorf("public = %q, want the zone name to win", got.Public)
	}
	if got.Raw != "http://provider.example:31293" {
		t.Errorf("raw = %q, want the override kept as the direct route", got.Raw)
	}
	if got.Base() != "https://vsrania.online" {
		t.Errorf("Base() = %q", got.Base())
	}
}

// No zone and no flag is a real misconfiguration, and it must not be quiet: the
// deploy that follows will fund an escrow, lease, boot, fail to find storage and
// crash. That is what happened live, and the log said nothing about why.
func TestNoControllerURLIsWarnedAbout(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.DNS.Enabled = false })
	h.poll()

	if got := h.published().URLs.Base(); got != "" {
		t.Errorf("Base() = %q, want empty with no zone and no flag", got)
	}
	if !h.logged("no controller URL to publish") {
		h.dumpLogs()
		t.Error("no warning logged for a controller with no reachable URL")
	}
}

// The value the agent reads from git and the value baked into the server's SDL have
// to be the same one, or a redeploy can hand the agent an address the branch
// contradicts. Both come from controllerURL().
func TestServerDeployGetsThePublishedURL(t *testing.T) {
	h := newHarness(t, dnsOn,
		func(d *Deps) { d.ControllerURL = "http://provider.example:31293" })
	h.poll()

	if got, want := h.m.controllerURL(), h.published().URLs.Base(); got != want {
		t.Errorf("SDL would get %q but the branch says %q", got, want)
	}
	if got := h.m.controllerURL(); got != "https://vsrania.online" {
		t.Errorf("controllerURL() = %q", got)
	}
}

// resolveURLs runs on every pass, so it has to be idempotent: marking the document
// dirty when nothing changed would mean a commit to the state branch on every poll,
// forever.
func TestResolvingURLsTwiceDoesNotRepublish(t *testing.T) {
	h := newHarness(t, dnsOn)
	h.poll()
	before := h.published()

	h.m.resolveURLs()
	if h.m.pending != "" {
		t.Errorf("a second resolve marked the document dirty: %q", h.m.pending)
	}
	h.poll()
	if got := h.published().UpdatedAt; got != before.UpdatedAt {
		t.Errorf("republished: updated_at moved from %v to %v", before.UpdatedAt, got)
	}
}

// A change of zone has to reach the branch. This is the redeploy-onto-a-new-domain
// case, and the reason resolveURLs compares every pass rather than writing once at
// startup.
func TestChangingTheZoneRepublishesTheURL(t *testing.T) {
	h := newHarness(t, dnsOn)
	h.poll()

	h.cfg.DNS.Domain = "example.invalid"
	h.poll()

	got := h.published().URLs
	if got.Public != "https://example.invalid" {
		t.Errorf("public = %q, want the new zone", got.Public)
	}
	if got.Webhook != "https://example.invalid/webhook" {
		t.Errorf("webhook = %q, want it moved with the zone", got.Webhook)
	}
}

// The URL has to be on the branch before the agent needs it, which means before any
// deploy: the agent boots into a container that has to reach storage immediately, so
// publishing at online would be too late.
func TestURLIsPublishedBeforeAnyDeploy(t *testing.T) {
	h := newHarness(t, dnsOn)
	h.poll()

	if got := h.published().URLs.Base(); got == "" {
		t.Fatal("no URL on the branch before a deploy")
	}
	if got := h.published().Status; got != state.StatusOffline {
		t.Errorf("status = %q, want the URL published while still offline", got)
	}
}
