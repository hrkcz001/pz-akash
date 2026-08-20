// Package dns keeps the Cloudflare zone pointing at whatever we deployed last.
//
// It answers two separate questions, and the difference between them is the whole
// design:
//
//   - Where is the dashboard? The apex (and www) are proxied CNAMEs at
//     Cloudflare, which terminates TLS and forwards to the controller's provider
//     hostname on whatever port Akash assigned. That is v1's behaviour, and
//     v1's update_cloudflare.py is the specification this half is written
//     against.
//   - Where is the game server? A DNS-only A record, `dns.game_record`, pointed
//     at the lease's dedicated IP. New in v2: v1 published a fresh IP to the
//     dashboard after every redeploy and players had to go and read it.
//
// The game record must never be proxied. Cloudflare's proxy carries HTTP, PZ does
// not, and a proxied name resolves to Cloudflare's own addresses — so proxying it
// would send every player to Cloudflare instead of to the server. That is not a
// setting this package exposes; `dns.proxied` governs the controller records only.
//
// Nothing in here is allowed to fail a deploy. A record that did not get written
// costs an address the operator reads off the dashboard, which is exactly where v1
// left it; a lease that failed because Cloudflare returned 502 costs a redeploy and
// a world rollback.
package dns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
)

// Cloudflare is a zone-scoped Cloudflare v4 client. It is safe for concurrent use.
type Cloudflare struct {
	base   string
	token  string
	zoneID string
	zone   config.DNS

	hc    *http.Client
	logf  func(string, ...any)
	sleep func(context.Context, time.Duration) error

	dryRun bool

	timeout   time.Duration
	retries   int
	retryWait time.Duration
}

// Options configures a Cloudflare client.
type Options struct {
	// Zone is the dns: block, verbatim. Passing the whole thing rather than a
	// dozen fields keeps the derived names (GameHost, ControllerHosts) in config
	// where they are already tested, instead of being re-derived here.
	Zone config.DNS

	// Token is PZ_CLOUDFLARE_API_TOKEN. It needs Zone.DNS:Edit, and
	// Zone.Zone Settings:Edit plus Zone.Config:Edit for the settings and rulesets.
	Token string

	HTTP *http.Client
	Logf func(string, ...any)

	// DryRun performs every read and no write: a sync reports what it would have
	// changed, having actually compared it against the live zone. This is the only
	// safe way to point v2 at a zone v1 has been managing, where the question is
	// not "does the code work" but "what is already in there".
	DryRun bool

	// sleep is overridden in tests so a backoff costs no wall-clock time.
	sleep func(context.Context, time.Duration) error
}

// New builds a client. It returns nil, nil when DNS is disabled: every caller has
// somewhere sensible to put "no DNS" and none of them should have to ask twice.
func New(o Options) (*Cloudflare, error) {
	if !o.Zone.Enabled {
		return nil, nil
	}
	if o.Zone.Provider != "cloudflare" {
		return nil, fmt.Errorf("dns: provider %q is not supported", o.Zone.Provider)
	}
	base := strings.TrimRight(strings.TrimSpace(o.Zone.APIBase), "/")
	if base == "" {
		return nil, errors.New("dns: api_base is empty")
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return nil, fmt.Errorf("dns: api_base %q is not an http(s) URL", base)
	}
	if strings.TrimSpace(o.Token) == "" {
		return nil, errors.New("dns: the Cloudflare API token is empty")
	}
	if strings.TrimSpace(o.Zone.ZoneID) == "" {
		return nil, errors.New("dns: zone_id is empty")
	}

	c := &Cloudflare{
		base:      base,
		token:     o.Token,
		zoneID:    strings.TrimSpace(o.Zone.ZoneID),
		zone:      o.Zone,
		hc:        o.HTTP,
		logf:      o.Logf,
		sleep:     o.sleep,
		dryRun:    o.DryRun,
		timeout:   o.Zone.Timeout.D(),
		retries:   o.Zone.Retries,
		retryWait: o.Zone.RetryWait.D(),
	}
	if c.hc == nil {
		c.hc = &http.Client{}
	}
	if c.logf == nil {
		c.logf = func(string, ...any) {}
	}
	if c.sleep == nil {
		c.sleep = sleepCtx
	}
	if c.retryWait <= 0 {
		c.retryWait = 2 * time.Second
	}
	return c, nil
}

// Domain is the zone's apex name, and PublicURL is where the dashboard answers
// once the controller records are in place.
func (c *Cloudflare) Domain() string { return c.zone.Domain }

// PublicURL is https:// even when ssl_mode is flexible, because that describes
// Cloudflare's connection to our origin, not the browser's to Cloudflare.
func (c *Cloudflare) PublicURL() string {
	if c == nil || c.zone.Domain == "" {
		return ""
	}
	return "https://" + c.zone.Domain
}

// --- changes ---

// Action is what a sync did to one record.
type Action string

const (
	Created   Action = "created"
	Updated   Action = "updated"
	Unchanged Action = "unchanged"
	Deleted   Action = "deleted"
)

// verb is how to say the Action happened, or — for a plan — that it would. Only the
// three that write have a future form: "would unchanged" is not English, and a record
// that already matches would be left alone either way, so the plan and the outcome
// are the same word.
func (a Action) verb(planned bool) string {
	if !planned {
		return string(a)
	}
	switch a {
	case Created:
		return "would create"
	case Updated:
		return "would update"
	case Deleted:
		return "would delete"
	default:
		return string(a)
	}
}

// Change is one record a sync touched. Unchanged is reported rather than omitted:
// these syncs run on every deploy and an operator reading the log needs to see that
// the record was checked, not infer it from silence.
type Change struct {
	Action  Action
	Name    string
	Type    string
	Content string
	Proxied bool
	TTL     int

	// Planned means the write was withheld because of Options.DryRun, so the Action
	// is what would have happened rather than what did. It is carried on the change
	// rather than handled by whoever prints one because there is more than one such
	// printer, and a report of a dry run that reads exactly like a report of a real
	// run is how an operator comes to believe a record exists.
	Planned bool
}

func (ch Change) String() string {
	s := fmt.Sprintf("%s %s %s -> %s", ch.Action.verb(ch.Planned),
		ch.Type, ch.Name, ch.Content)
	if ch.Action != Deleted {
		if ch.Proxied {
			s += " (proxied)"
		} else {
			s += fmt.Sprintf(" (dns-only, ttl %d)", ch.TTL)
		}
	}
	return s
}

// --- the two syncs ---

// SyncController points the apex — and www, when dns.include_www — at the
// controller's own address, and brings the zone settings v1 managed into line.
//
// target is what the controller answers on: a URL, or a host:port. Its port is the
// origin port, and a port that is not 80 or 443 needs an origin rule, because
// Cloudflare will otherwise forward :443 traffic to :443 on a provider that is
// listening somewhere else entirely.
//
// Every step is attempted even when an earlier one failed, and the failures are
// returned together. Half-applying this is the state v1 could reach and could not
// report: an apex record pointing at the new controller with an origin rule still
// naming the old port.
func (c *Cloudflare) SyncController(ctx context.Context, target string) ([]Change, error) {
	if c == nil {
		return nil, nil
	}
	host, port, err := splitTarget(target)
	if err != nil {
		return nil, err
	}
	hosts := c.zone.ControllerHosts()
	if len(hosts) == 0 {
		return nil, errors.New("dns: no controller hostname — dns.domain is empty")
	}

	var (
		changes []Change
		errs    []error
	)
	if c.zone.ClearRedirectRules {
		if err := c.clearAllRules(ctx, phaseDynamicRedirect); err != nil {
			errs = append(errs, err)
		}
	}
	// Only meaningful behind the proxy: with proxied off, Cloudflare is not in the
	// path and how it would talk to our origin describes nothing.
	if c.zone.Proxied {
		if c.zone.SSLMode != "" {
			if err := c.setSetting(ctx, "ssl", c.zone.SSLMode); err != nil {
				errs = append(errs, err)
			}
		}
		if c.zone.RelaxSecurity {
			if err := c.setSetting(ctx, "browser_check", "off"); err != nil {
				errs = append(errs, err)
			}
			if err := c.setSetting(ctx, "security_level", "essentially_off"); err != nil {
				errs = append(errs, err)
			}
		}
	}

	for _, name := range hosts {
		ch, err := c.upsert(ctx, record{
			Name:    name,
			Type:    recordType(host),
			Content: host,
			Proxied: c.zone.Proxied,
			// Proxied records must carry ttl 1; Cloudflare rejects anything else,
			// since the edge answers with its own.
			TTL: 1,
		})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		changes = append(changes, ch)
	}

	switch {
	case !c.zone.Proxied:
		// Nothing to route: clients reach the origin directly and have to be told
		// the port, which is the address the dashboard prints.
	case port == 80 || port == 443:
		if err := c.dropOriginRule(ctx); err != nil {
			errs = append(errs, err)
		}
	default:
		if err := c.setOriginPort(ctx, hosts, port); err != nil {
			errs = append(errs, err)
		}
	}
	return changes, errors.Join(errs...)
}

// SyncGame points dns.game_record at the game server's address.
//
// DNS-only, always: see the package comment. The record is written even when the
// address has not changed — the check is one GET, and the alternative is trusting
// that nothing else has touched the zone since the last deploy.
func (c *Cloudflare) SyncGame(ctx context.Context, addr string) ([]Change, error) {
	if c == nil {
		return nil, nil
	}
	name := c.zone.GameHost()
	if name == "" {
		return nil, nil
	}
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil, errors.New("dns: refusing to point " + name + " at an empty address")
	}
	// A host:port here would be written into the record verbatim and resolve to
	// nothing. Catching it costs one line and saves a record nobody can use.
	if h, _, err := net.SplitHostPort(addr); err == nil {
		addr = h
	}
	ttl := c.zone.GameTTL
	if ttl <= 0 {
		ttl = 1
	}
	ch, err := c.upsert(ctx, record{
		Name:    name,
		Type:    recordType(addr),
		Content: addr,
		Proxied: false,
		TTL:     ttl,
	})
	if err != nil {
		return nil, err
	}
	return []Change{ch}, nil
}

// ClearGame removes the game record, so that a name with no server behind it
// resolves to nothing rather than to whoever the provider gave that IP to next.
// Deleting a record that is already gone is a success.
func (c *Cloudflare) ClearGame(ctx context.Context) ([]Change, error) {
	if c == nil {
		return nil, nil
	}
	name := c.zone.GameHost()
	if name == "" {
		return nil, nil
	}
	return c.deleteByName(ctx, name)
}

// splitTarget accepts a URL, a bare host, or a host:port, and returns the host and
// the port Cloudflare should forward to. v1 took the same three shapes because the
// value came from a lease status that might be any of them.
func splitTarget(target string) (host string, port int, err error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", 0, errors.New("dns: the controller target is empty")
	}
	raw := target
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", 0, fmt.Errorf("dns: cannot parse the controller target %q: %w", target, err)
	}
	host = u.Hostname()
	if host == "" {
		return "", 0, fmt.Errorf("dns: the controller target %q has no host", target)
	}
	switch p := u.Port(); {
	case p != "":
		if port, err = strconv.Atoi(p); err != nil || port <= 0 || port > 65535 {
			return "", 0, fmt.Errorf("dns: the controller target %q has an invalid port", target)
		}
	case u.Scheme == "https":
		port = 443
	default:
		port = 80
	}
	return host, port, nil
}

// recordType picks A, AAAA or CNAME from the content. Getting this wrong is a
// Cloudflare 400 rather than a broken record, but the 400 arrives after the lease
// is already billing.
func recordType(content string) string {
	ip := net.ParseIP(content)
	switch {
	case ip == nil:
		return "CNAME"
	case ip.To4() != nil:
		return "A"
	default:
		return "AAAA"
	}
}
