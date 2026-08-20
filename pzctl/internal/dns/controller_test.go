package dns

// The controller half, written against v1's update_cloudflare.py: a proxied record
// at the apex and at www, SSL set so Cloudflare will talk to an origin with no
// certificate, the browser challenges turned off so a webhook is never asked to
// solve one, and an origin rule when Akash put the controller on a high port.
//
// What is deliberately not v1's behaviour is tested here too: v1 replaced whole
// rulesets and wrote to the zone on every boot whether anything had changed or not.

import (
	"context"
	"strings"
	"testing"
)

func TestSyncControllerProxiesTheApexAndWWW(t *testing.T) {
	f := newFakeCF(t)
	c := f.client(t, testZone())

	changes, err := c.SyncController(context.Background(), "http://provider.example:32100")
	if err != nil {
		t.Fatalf("SyncController: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("changes = %+v, want the apex and www", changes)
	}

	for _, name := range []string{"vsrania.online", "www.vsrania.online"} {
		got := f.one(name)
		if got.Type != "CNAME" || got.Content != "provider.example" {
			t.Errorf("%s = %+v, want a CNAME to the provider", name, got)
		}
		if !got.Proxied {
			t.Errorf("%s is not proxied, so there is no TLS and no custom domain", name)
		}
		if got.TTL != 1 {
			t.Errorf("%s ttl = %d, want 1: Cloudflare rejects anything else on a proxied record", name, got.TTL)
		}
	}

	if got := f.setting("ssl"); got != "flexible" {
		t.Errorf("ssl = %q, want flexible — an Akash origin has no certificate, so anything stricter answers 526", got)
	}
	if got := f.setting("browser_check"); got != "off" {
		t.Errorf("browser_check = %q, want off — GitHub's webhook cannot solve a challenge", got)
	}
	if got := f.setting("security_level"); got != "essentially_off" {
		t.Errorf("security_level = %q, want essentially_off", got)
	}

	rules := f.rules(phaseOrigin)
	if len(rules) != 1 {
		t.Fatalf("origin rules = %+v, want exactly ours", rules)
	}
	if got := rules[0]["expression"]; !strings.Contains(got.(string), `http.host eq "vsrania.online"`) ||
		!strings.Contains(got.(string), `http.host eq "www.vsrania.online"`) {
		t.Errorf("expression = %v, want both controller hostnames", got)
	}
	// The port is the whole point of the rule: Cloudflare receives :443 and has to
	// be told the origin is somewhere else entirely.
	if got := f.bodyOf("PUT /zones/" + testZoneID + "/rulesets"); !strings.Contains(got, `"port":32100`) {
		t.Errorf("the origin rule does not name port 32100: %s", got)
	}
}

func TestSyncControllerTakesAnAddressWithoutAScheme(t *testing.T) {
	f := newFakeCF(t)
	c := f.client(t, testZone())
	// v1 accepted a bare host:port because the value came from a lease status that
	// might be any of three shapes.
	if _, err := c.SyncController(context.Background(), "203.0.113.20:32100"); err != nil {
		t.Fatalf("SyncController: %v", err)
	}
	if got := f.one("vsrania.online"); got.Type != "A" || got.Content != "203.0.113.20" {
		t.Errorf("apex = %+v, want an A to the address", got)
	}
}

func TestSyncControllerOnAStandardPortWritesNoOriginRule(t *testing.T) {
	f := newFakeCF(t)
	c := f.client(t, testZone())

	if _, err := c.SyncController(context.Background(), "https://provider.example"); err != nil {
		t.Fatalf("SyncController: %v", err)
	}
	if rules := f.rules(phaseOrigin); len(rules) != 0 {
		t.Errorf("origin rules = %+v, want none for a standard port", rules)
	}
	// And no empty ruleset created just to say we were here.
	for _, call := range f.log() {
		if strings.HasPrefix(call, "PUT ") && strings.Contains(call, "/rulesets/") {
			t.Errorf("a ruleset was written for a zone that had none: %v", f.log())
			break
		}
	}
}

// TestSyncControllerRemovesAStaleOriginRule: the port changes with every controller
// redeploy, and a rule left naming the old one forwards every request to a port
// nothing is listening on — a dashboard that times out while the lease is healthy.
func TestSyncControllerRemovesAStaleOriginRule(t *testing.T) {
	f := newFakeCF(t)
	f.seedRules(phaseOrigin, map[string]any{
		"description": originRuleMarker + " (vsrania.online -> :31000)",
		"expression":  `(http.host eq "vsrania.online")`,
		"action":      "route",
		"enabled":     true,
	})
	c := f.client(t, testZone())

	if _, err := c.SyncController(context.Background(), "https://provider.example"); err != nil {
		t.Fatalf("SyncController: %v", err)
	}
	if rules := f.rules(phaseOrigin); len(rules) != 0 {
		t.Errorf("origin rules = %+v, want ours gone", rules)
	}
}

// TestSyncControllerReplacesTheV1Rule: v1 put the port in the description, so
// matching on the whole string would leave one rule per port ever deployed, with
// whichever matched first deciding where traffic went.
func TestSyncControllerReplacesTheV1Rule(t *testing.T) {
	f := newFakeCF(t)
	f.seedRules(phaseOrigin, map[string]any{
		"description":       v1OriginRuleMarker + " (vsrania.online -> :31000)",
		"expression":        `(http.host eq "vsrania.online" or http.host eq "www.vsrania.online")`,
		"action":            "route",
		"action_parameters": map[string]any{"origin": map[string]any{"port": 31000}},
		"enabled":           true,
	})
	c := f.client(t, testZone())

	if _, err := c.SyncController(context.Background(), "http://provider.example:32100"); err != nil {
		t.Fatalf("SyncController: %v", err)
	}
	rules := f.rules(phaseOrigin)
	if len(rules) != 1 {
		t.Fatalf("origin rules = %+v, want v1's replaced rather than stacked", rules)
	}
	if d, _ := rules[0]["description"].(string); !strings.HasPrefix(d, originRuleMarker) {
		t.Errorf("description = %q, want ours", d)
	}
}

// TestSyncControllerKeepsForeignOriginRules: v1 emptied the whole phase. A zone
// this system does not exclusively own would lose rules nobody could recover.
func TestSyncControllerKeepsForeignOriginRules(t *testing.T) {
	f := newFakeCF(t)
	foreign := map[string]any{
		"description": "somebody else's rule",
		"expression":  `(http.host eq "shop.vsrania.online")`,
		"action":      "route",
		// The two fields the API sends back and refuses to accept. The fake rejects
		// them on a PUT, so this asserts they are stripped on the way through.
		"version":      "3",
		"last_updated": "2026-01-01T00:00:00Z",
		"enabled":      true,
	}
	f.seedRules(phaseOrigin, foreign)
	c := f.client(t, testZone())

	if _, err := c.SyncController(context.Background(), "http://provider.example:32100"); err != nil {
		t.Fatalf("SyncController: %v", err)
	}
	rules := f.rules(phaseOrigin)
	if len(rules) != 2 {
		t.Fatalf("origin rules = %+v, want ours and theirs", rules)
	}
	// Ours first: origin rules are evaluated in order, and a foreign rule that
	// matched one of our hostnames ahead of ours would decide the port.
	if d, _ := rules[0]["description"].(string); !strings.HasPrefix(d, originRuleMarker) {
		t.Errorf("rule order = %v, want ours first", []any{rules[0]["description"], rules[1]["description"]})
	}
	if d, _ := rules[1]["description"].(string); d != "somebody else's rule" {
		t.Errorf("the foreign rule was not preserved: %+v", rules)
	}
}

// TestSyncControllerDropsOnlyOurRuleOnAStandardPort is the same property on the
// other branch: standard port, so our rule goes and theirs stays.
func TestSyncControllerDropsOnlyOurRuleOnAStandardPort(t *testing.T) {
	f := newFakeCF(t)
	f.seedRules(phaseOrigin,
		map[string]any{"description": originRuleMarker + " (x -> :31000)", "action": "route", "enabled": true},
		map[string]any{"description": "somebody else's rule", "action": "route", "enabled": true},
	)
	c := f.client(t, testZone())

	if _, err := c.SyncController(context.Background(), "https://provider.example"); err != nil {
		t.Fatalf("SyncController: %v", err)
	}
	rules := f.rules(phaseOrigin)
	if len(rules) != 1 {
		t.Fatalf("origin rules = %+v, want only the foreign one", rules)
	}
	if d, _ := rules[0]["description"].(string); d != "somebody else's rule" {
		t.Errorf("the wrong rule survived: %+v", rules)
	}
}

// TestSyncControllerLeavesMailRecordsAlone is the reason the record code filters by
// type. The apex of a real domain carries MX and TXT records, and a sync that
// cleared the name before writing would take the operator's email with it.
func TestSyncControllerLeavesMailRecordsAlone(t *testing.T) {
	f := newFakeCF(t)
	f.seed(record{Name: "vsrania.online", Type: "MX", Content: "mail.protonmail.ch", TTL: 3600})
	f.seed(record{Name: "vsrania.online", Type: "TXT", Content: "v=spf1 include:_spf.protonmail.ch ~all", TTL: 3600})
	c := f.client(t, testZone())

	if _, err := c.SyncController(context.Background(), "http://provider.example:32100"); err != nil {
		t.Fatalf("SyncController: %v", err)
	}
	var kinds []string
	for _, r := range f.byName("vsrania.online") {
		kinds = append(kinds, r.Type)
	}
	if len(kinds) != 3 {
		t.Fatalf("the apex holds %v, want MX, TXT and our CNAME", kinds)
	}
}

func TestSyncControllerWritesNothingWhenNothingChanged(t *testing.T) {
	f := newFakeCF(t)
	c := f.client(t, testZone())
	ctx := context.Background()

	if _, err := c.SyncController(ctx, "http://provider.example:32100"); err != nil {
		t.Fatalf("first SyncController: %v", err)
	}
	f.reset()

	changes, err := c.SyncController(ctx, "http://provider.example:32100")
	if err != nil {
		t.Fatalf("second SyncController: %v", err)
	}
	for _, ch := range changes {
		if ch.Action != Unchanged {
			t.Errorf("%s was rewritten by an identical sync", ch)
		}
	}
	if w := f.writes(); len(w) != 0 {
		t.Errorf("an identical sync wrote to the zone: %v", w)
	}
}

func TestSyncControllerWithoutWWW(t *testing.T) {
	f := newFakeCF(t)
	z := testZone()
	z.IncludeWWW = false
	c := f.client(t, z)

	changes, err := c.SyncController(context.Background(), "http://provider.example:32100")
	if err != nil {
		t.Fatalf("SyncController: %v", err)
	}
	if len(changes) != 1 || changes[0].Name != "vsrania.online" {
		t.Fatalf("changes = %+v, want the apex alone", changes)
	}
	if got := f.byName("www.vsrania.online"); len(got) != 0 {
		t.Errorf("www was written with include_www off: %+v", got)
	}
	if got := f.bodyOf("PUT /zones/" + testZoneID + "/rulesets"); strings.Contains(got, "www.") {
		t.Errorf("the origin rule matches www with include_www off: %s", got)
	}
}

// TestSyncControllerUnproxiedTouchesNoZoneSettings: ssl_mode describes how
// Cloudflare talks to our origin, and there is no such conversation when the record
// is DNS-only. Patching it anyway would change the zone for records we do not own.
func TestSyncControllerUnproxiedTouchesNoZoneSettings(t *testing.T) {
	f := newFakeCF(t)
	z := testZone()
	z.Proxied = false
	c := f.client(t, z)

	if _, err := c.SyncController(context.Background(), "http://provider.example:32100"); err != nil {
		t.Fatalf("SyncController: %v", err)
	}
	if got := f.setting("ssl"); got != "off" {
		t.Errorf("ssl = %q, want the zone's own value untouched", got)
	}
	if got := f.setting("browser_check"); got != "on" {
		t.Errorf("browser_check = %q, want the zone's own value untouched", got)
	}
	if got := f.one("vsrania.online"); got.Proxied {
		t.Error("the apex is proxied with dns.proxied false")
	}
	// No origin rule either: with Cloudflare out of the path there is nothing to
	// route, and the port has to reach clients some other way.
	if rules := f.rules(phaseOrigin); len(rules) != 0 {
		t.Errorf("origin rules = %+v, want none when unproxied", rules)
	}
}

func TestSyncControllerLeavesSecuritySettingsAloneWhenAsked(t *testing.T) {
	f := newFakeCF(t)
	z := testZone()
	z.RelaxSecurity = false
	c := f.client(t, z)

	if _, err := c.SyncController(context.Background(), "http://provider.example:32100"); err != nil {
		t.Fatalf("SyncController: %v", err)
	}
	if got := f.setting("browser_check"); got != "on" {
		t.Errorf("browser_check = %q, want untouched with relax_security false", got)
	}
	if got := f.setting("ssl"); got != "flexible" {
		t.Errorf("ssl = %q — relax_security must not govern the SSL mode", got)
	}
}

func TestSyncControllerClearsRedirectRulesOnlyWhenAsked(t *testing.T) {
	stale := map[string]any{
		"description": "v1's 302 to the provider",
		"expression":  `(http.host eq "vsrania.online")`,
		"action":      "redirect",
		"enabled":     true,
	}

	f := newFakeCF(t)
	f.seedRules(phaseDynamicRedirect, stale)
	c := f.client(t, testZone())
	if _, err := c.SyncController(context.Background(), "http://provider.example:32100"); err != nil {
		t.Fatalf("SyncController: %v", err)
	}
	if rules := f.rules(phaseDynamicRedirect); len(rules) != 1 {
		t.Errorf("redirect rules = %+v, want them left alone by default", rules)
	}

	f2 := newFakeCF(t)
	f2.seedRules(phaseDynamicRedirect, stale)
	z := testZone()
	z.ClearRedirectRules = true
	c2 := f2.client(t, z)
	if _, err := c2.SyncController(context.Background(), "http://provider.example:32100"); err != nil {
		t.Fatalf("SyncController: %v", err)
	}
	if rules := f2.rules(phaseDynamicRedirect); len(rules) != 0 {
		t.Errorf("redirect rules = %+v, want them cleared", rules)
	}
}

func TestSyncControllerRejectsAnUnusableTarget(t *testing.T) {
	f := newFakeCF(t)
	c := f.client(t, testZone())
	for _, target := range []string{"", "   ", "http://:32100", "http://host:0", "http://host:99999"} {
		if _, err := c.SyncController(context.Background(), target); err == nil {
			t.Errorf("target %q was accepted", target)
		}
	}
	if calls := f.log(); len(calls) != 0 {
		t.Errorf("the zone was called for a target we could not parse: %v", calls)
	}
}

// TestSyncControllerReportsEveryFailure: a half-applied sync is the state v1 could
// reach and could not report — a record pointing at the new controller with an
// origin rule still naming the old port. Every step runs, and every failure comes
// back.
func TestSyncControllerReportsEveryFailure(t *testing.T) {
	f := newFakeCF(t)
	f.onFail(func(method, path string) (int, string) {
		switch {
		case method == "PATCH" && strings.Contains(path, "/settings/ssl"):
			return 403, refuse(10000, "Authentication error")
		case method == "PUT" && strings.Contains(path, "/rulesets/"):
			return 400, refuse(20041, "rulesets are not available on this plan")
		}
		return 0, ""
	})
	c := f.client(t, testZone())

	changes, err := c.SyncController(context.Background(), "http://provider.example:32100")
	if err == nil {
		t.Fatal("a sync with two failed steps reported success")
	}
	for _, want := range []string{"ssl", "origin port"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q:\n%v", want, err)
		}
	}
	// The records still got written, and are still reported. Losing the successes
	// would leave the caller unable to log what did happen.
	if len(changes) != 2 {
		t.Errorf("changes = %+v, want the two records that did get written", changes)
	}
	if got := f.byName("vsrania.online"); len(got) != 1 {
		t.Errorf("the apex was not written even though only the settings failed: %+v", got)
	}
}

func TestPublicURL(t *testing.T) {
	f := newFakeCF(t)
	c := f.client(t, testZone())
	// https even with ssl_mode flexible: that setting describes Cloudflare's
	// connection to the origin, not the browser's to Cloudflare.
	if got := c.PublicURL(); got != "https://vsrania.online" {
		t.Errorf("PublicURL() = %q", got)
	}
	if got := c.Domain(); got != "vsrania.online" {
		t.Errorf("Domain() = %q", got)
	}
}

func TestZones(t *testing.T) {
	f := newFakeCF(t)
	c := f.client(t, testZone())
	got, err := c.Zones(context.Background())
	if err != nil {
		t.Fatalf("Zones: %v", err)
	}
	if len(got) != 1 || got[0].ID != testZoneID || got[0].Name != "vsrania.online" {
		t.Errorf("Zones() = %+v", got)
	}
}
