package dns

// Zone settings and rulesets: everything that is configuration of the zone rather
// than a record in it.
//
// Two of these have teeth. `ssl` decides whether Cloudflare will talk HTTP to an
// origin that has no certificate, which every Akash provider hostname is; without
// `flexible` the dashboard answers 526 and looks like an outage. The origin rule
// decides which port :443 traffic is forwarded to, and Akash assigns that port at
// lease time — so the rule is as much a per-deploy fact as the address is.
//
// Rules we did not write are preserved. v1 replaced whole rulesets, which is fine
// on a zone that has only ever been driven by v1 and destroys an afternoon's work
// on one that has not.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	phaseOrigin          = "http_request_origin"
	phaseDynamicRedirect = "http_request_dynamic_redirect"
)

// originRuleMarker identifies our origin rule across port changes. v1 put the port
// in the description, so a redeploy onto a different port left the old rule in
// place and added a second one — matching on a stable prefix is what makes the rule
// replaceable instead of cumulative.
const originRuleMarker = "pzctl: controller origin port"

// v1OriginRuleMarker is the description update_cloudflare.py used. Recognising it
// means the first v2 sync replaces v1's rule rather than stacking a second rule in
// front of it, which would leave whichever matched first deciding the port.
const v1OriginRuleMarker = "PZ Controller Origin Port Route"

// --- settings ---

type settingValue struct {
	ID    string          `json:"id"`
	Value json.RawMessage `json:"value"`
}

// setSetting brings one zone setting to value, reading it first so a zone that is
// already correct is not written to. Not for economy: these run on every controller
// sync, and a settings history full of no-op writes is where a real change hides.
func (c *Cloudflare) setSetting(ctx context.Context, name, value string) error {
	path := "/zones/" + c.zoneID + "/settings/" + name
	var cur settingValue
	if err := c.do(ctx, "GET", path, nil, &cur); err != nil {
		// Not fatal on its own: the PATCH below is the operation that matters, and a
		// token permitted to write settings but not read them is a real shape.
		c.logf("cloudflare: could not read setting %s (%v) — patching anyway", name, err)
	} else if got := strings.Trim(string(cur.Value), `"`); got == value {
		c.logf("cloudflare: %s is already %s", name, value)
		return nil
	}
	if err := c.do(ctx, "PATCH", path, map[string]any{"value": value}, nil); err != nil {
		return fmt.Errorf("setting %s to %s: %w", name, value, err)
	}
	c.logf("cloudflare: set %s to %s", name, value)
	return nil
}

// --- rulesets ---

// rules reads the rules in a phase's entrypoint ruleset.
//
// They are kept as maps so that rules belonging to someone else survive a
// round-trip through us with fields this package has never heard of. The two
// server-managed fields are dropped, because sending them back is an error.
func (c *Cloudflare) rules(ctx context.Context, phase string) ([]map[string]any, error) {
	var out struct {
		Rules []map[string]any `json:"rules"`
	}
	path := "/zones/" + c.zoneID + "/rulesets/phases/" + phase + "/entrypoint"
	if err := c.do(ctx, "GET", path, nil, &out); err != nil {
		// No entrypoint ruleset yet. That is the normal state of a zone nobody has
		// written a rule on, and it means there is nothing to clean up.
		if Status(err) == 404 {
			return nil, nil
		}
		return nil, err
	}
	for _, r := range out.Rules {
		delete(r, "version")
		delete(r, "last_updated")
	}
	return out.Rules, nil
}

// putRules replaces a phase's entrypoint rules.
func (c *Cloudflare) putRules(ctx context.Context, phase string, rules []map[string]any) error {
	if rules == nil {
		// An explicit empty list, never null: null asks Cloudflare to leave the
		// rules alone, which is the opposite of what every caller here means.
		rules = []map[string]any{}
	}
	path := "/zones/" + c.zoneID + "/rulesets/phases/" + phase + "/entrypoint"
	return c.do(ctx, "PUT", path, map[string]any{"rules": rules}, nil)
}

// ours reports whether a rule is one this system wrote, in either version.
func ours(rule map[string]any) bool {
	d, _ := rule["description"].(string)
	return strings.HasPrefix(d, originRuleMarker) || strings.HasPrefix(d, v1OriginRuleMarker)
}

// setOriginPort makes Cloudflare forward the controller's hostnames to port on the
// origin. Traffic arrives at Cloudflare on :443; without this it would leave for
// :443 on a provider that is listening on an arbitrary high port.
func (c *Cloudflare) setOriginPort(ctx context.Context, hosts []string, port int) error {
	existing, err := c.rules(ctx, phaseOrigin)
	if err != nil {
		return err
	}

	terms := make([]string, 0, len(hosts))
	for _, h := range hosts {
		terms = append(terms, fmt.Sprintf("http.host eq %q", h))
	}
	want := map[string]any{
		"description": fmt.Sprintf("%s (%s -> :%d)", originRuleMarker, c.zone.Domain, port),
		"expression":  "(" + strings.Join(terms, " or ") + ")",
		"action":      "route",
		"action_parameters": map[string]any{
			"origin": map[string]any{"port": port},
		},
		"enabled": true,
	}

	// Ours first: origin rules are evaluated in order, and a foreign rule matching
	// one of our hostnames ahead of ours would decide the port instead.
	merged := []map[string]any{want}
	mine := 0
	for _, r := range existing {
		if ours(r) {
			mine++
			continue
		}
		merged = append(merged, r)
	}
	if foreign := len(existing) - mine; foreign > 0 {
		c.logf("cloudflare: keeping %d origin rule(s) we did not write", foreign)
	}
	// One rule of ours, already saying this, and nothing of ours duplicated behind
	// it: there is nothing to write.
	if mine == 1 {
		same, err := c.sameOriginRule(existing, want)
		if err != nil {
			return err
		}
		if same {
			c.logf("cloudflare: origin port for %s is already :%d", c.zone.Domain, port)
			return nil
		}
	}
	if err := c.putRules(ctx, phaseOrigin, merged); err != nil {
		return fmt.Errorf("routing %s to origin port %d: %w", c.zone.Domain, port, err)
	}
	c.logf("cloudflare: routing https://%s to origin port %d", c.zone.Domain, port)
	return nil
}

// sameOriginRule reports whether the live rules already consist of exactly our rule
// in front, with the wanted expression and port. Compared as JSON because the
// values arriving from the API are float64 where ours are int, and comparing those
// field by field is how a no-op write starts looking like a change.
func (c *Cloudflare) sameOriginRule(existing []map[string]any, want map[string]any) (bool, error) {
	if len(existing) == 0 || !ours(existing[0]) {
		return false, nil
	}
	for _, k := range []string{"description", "expression", "action", "action_parameters", "enabled"} {
		a, err := json.Marshal(existing[0][k])
		if err != nil {
			return false, err
		}
		b, err := json.Marshal(want[k])
		if err != nil {
			return false, err
		}
		if string(a) != string(b) {
			return false, nil
		}
	}
	return true, nil
}

// dropOriginRule removes our origin rule and leaves every other rule alone. Called
// when the controller answers on 80 or 443, where forwarding needs no help — and a
// stale rule from a previous deploy would send traffic to a port nothing is on.
func (c *Cloudflare) dropOriginRule(ctx context.Context) error {
	existing, err := c.rules(ctx, phaseOrigin)
	if err != nil {
		return err
	}
	kept := make([]map[string]any, 0, len(existing))
	for _, r := range existing {
		if !ours(r) {
			kept = append(kept, r)
		}
	}
	if len(kept) == len(existing) {
		return nil
	}
	if err := c.putRules(ctx, phaseOrigin, kept); err != nil {
		return fmt.Errorf("removing our origin port rule: %w", err)
	}
	c.logf("cloudflare: removed our origin port rule; the controller is on a standard port")
	return nil
}

// clearAllRules empties a phase's ruleset, including rules we did not write. Only
// reachable through dns.clear_redirect_rules, which says so.
func (c *Cloudflare) clearAllRules(ctx context.Context, phase string) error {
	existing, err := c.rules(ctx, phase)
	if err != nil {
		return err
	}
	if len(existing) == 0 {
		// Nothing to clear, and no empty ruleset created to say we were here.
		return nil
	}
	for _, r := range existing {
		if !ours(r) {
			d, _ := r["description"].(string)
			c.logf("cloudflare: WARNING deleting %s rule not written by us: %q", phase, d)
		}
	}
	if err := c.putRules(ctx, phase, nil); err != nil {
		return fmt.Errorf("clearing the %s rules: %w", phase, err)
	}
	c.logf("cloudflare: cleared %d rule(s) from %s", len(existing), phase)
	return nil
}

// --- zones ---

// Zone is a zone this token can see.
type Zone struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// Zones lists the zones the token can reach. It is the answer to "what do I put in
// dns.zone_id": v1 guessed by taking the first zone the token could see, which is
// correct until the account holds two domains and then silently reconfigures the
// wrong one.
func (c *Cloudflare) Zones(ctx context.Context) ([]Zone, error) {
	var out []Zone
	if err := c.do(ctx, "GET", "/zones?per_page=100", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
