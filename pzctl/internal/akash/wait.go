package akash

// The waiting. Three loops, each with its own deadline from config, and each with
// the same discipline: a timeout says what it was waiting for and what it saw
// instead, because "timed out" alone leaves an operator with a bill and no
// explanation.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// waitForBid polls the deployment's bids until one is worth taking.
//
// Bids arrive over seconds, not at once, so the first poll returning nothing is
// normal and the loop is not an error path. What is an error is running out of
// time, and then the message carries the rejection reasons: "every bid was 12
// USD/day against a 3 USD/day limit" and "the only bidder is skip-listed" are
// different problems with the same symptom.
//
// It does not stop at the first acceptable bid, and that is the whole shape of the
// function. SelectBid returns the cheapest bid it has been shown, so returning as
// soon as one is acceptable means taking the cheapest of however few happened to
// have arrived five seconds in — which is one. The live server was leased that way:
// it went to a provider at $0.96/day while another eligible provider bid $0.81 for
// the same spec, a difference of $55 a year decided by whose bid engine answered
// first. So the first acceptable bid opens a window of akash.timeouts.bid_settle
// during which the loop keeps looking, and then takes the best of the field.
//
// The window runs from that first bid rather than from loop entry, because entry is
// not when the market starts answering. A round where the first bid lands at 100s
// would otherwise get 20 seconds of shopping out of a 120-second budget — and slow
// bidders are the exact thing being waited for.
//
// Settling can only improve the outcome, never fail a deploy that would otherwise
// have worked: a bid held through the settle window is still there at the end of it
// — bids do not expire in minutes — and if bid_wait runs out first, whatever was
// acceptable is taken rather than discarded.
func (d *Driver) waitForBid(ctx context.Context, dseq string, cr Criteria, providers []Provider) (*Choice, error) {
	var (
		poll     = time.Duration(d.Cfg.Akash.Timeouts.BidPoll)
		settle   = time.Duration(d.Cfg.Akash.Timeouts.BidSettle)
		deadline = d.Now().Add(time.Duration(d.Cfg.Akash.Timeouts.BidWait))
		// Zero until something acceptable has been seen; see the doc comment.
		settled  time.Time
		best     *Choice
		lastBids int
		lastWhy  string
	)
	for attempt := 1; ; attempt++ {
		var list bidList
		err := d.Client.do(ctx, "GET", "/v1/bids?dseq="+dseq, nil, &list)
		if err != nil && !NotFound(err) {
			return nil, fmt.Errorf("reading bids on dseq %s: %w", dseq, err)
		}
		lastBids = len(list)

		choice, bad, err := SelectBid(cr, list, providers)
		if err != nil {
			// Bad criteria, not a bad market: retrying cannot help.
			return nil, err
		}
		if choice != nil {
			// Overwritten rather than compared, because the bid list only grows and
			// SelectBid is given all of it every time: the newest answer is by
			// construction the best of everything seen so far.
			if best != nil && choice.Bid.ID.Provider != best.Bid.ID.Provider {
				d.Logf("akash: dseq %s: %s bid %.4f USD/day, undercutting %s at %.4f",
					dseq, choice.Provider.Owner, choice.USDPerDay,
					best.Provider.Owner, best.USDPerDay)
			}
			best = choice
			if settled.IsZero() {
				settled = d.Now().Add(settle)
			}
		}
		if len(bad) > 0 {
			lastWhy = Reasons(bad)
		}

		now := d.Now()
		if best != nil && !settled.IsZero() && !now.Before(settled) {
			return best, nil
		}
		if !now.Before(deadline) {
			if best != nil {
				// The settle window did not finish, but a usable bid is in hand. Taking
				// it beats failing a deploy over a preference for a cheaper one that
				// may not exist.
				d.Logf("akash: dseq %s: taking %s at %.4f USD/day without a full settle window",
					dseq, best.Provider.Owner, best.USDPerDay)
				return best, nil
			}
			return nil, fmt.Errorf("no acceptable bid on dseq %s after %s (%d bid(s) seen; %s)",
				dseq, time.Duration(d.Cfg.Akash.Timeouts.BidWait), lastBids, orNoBids(lastWhy, lastBids))
		}
		if attempt == 1 || attempt%4 == 0 {
			if best != nil {
				d.Logf("akash: dseq %s: %d bid(s), best so far %s at %.4f USD/day; still settling for %s",
					dseq, lastBids, best.Provider.Owner, best.USDPerDay, settled.Sub(now).Round(time.Second))
			} else {
				d.Logf("akash: dseq %s: %d bid(s), none acceptable yet (%s)", dseq, lastBids, orNoBids(lastWhy, lastBids))
			}
		}
		if err := d.sleep(ctx, poll); err != nil {
			return nil, err
		}
	}
}

func orNoBids(why string, bids int) string {
	if why != "" {
		return why
	}
	if bids == 0 {
		return "no provider bid at all"
	}
	return "no reason recorded"
}

// waitForEndpoint polls until the workload is running and reachable.
//
// Two sources, in that order of authority: the Console API, and — every
// akash.provider_status.every polls — the provider itself. The provider knows
// first, and v1 added the direct query after deploys timed out waiting for an
// address the provider had already assigned. That timeout is expensive: the lease
// is live and billing, so giving up means paying for a server nobody can join.
func (d *Driver) waitForEndpoint(ctx context.Context, l state.Lease, p Provider, service string, kind addrKind) (state.Endpoint, string, error) {
	var (
		poll     = time.Duration(d.Cfg.Akash.Timeouts.LeasePoll)
		wait     = time.Duration(d.Cfg.Akash.Timeouts.LeaseReady)
		deadline = d.Now().Add(wait)
		ps       = d.Cfg.Akash.ProviderStatus
		last     string
	)
	for attempt := 1; ; attempt++ {
		st, err := d.leaseStatus(ctx, l, service)
		switch {
		case err != nil:
			last = err.Error()
		case !st.ready:
			last = fmt.Sprintf("service %s has no ready replica", service)
		default:
			ep, url, err := d.endpointFrom(st.status, service, kind)
			if err == nil {
				return ep, url, nil
			}
			last = err.Error()
		}

		// The second opinion, on its own cadence so a healthy deploy does not mint
		// a JWT every few seconds.
		if ps.Enabled && ps.Every > 0 && attempt%ps.Every == 0 {
			if status, err := d.providerStatus(ctx, l, p); err != nil {
				d.Logf("akash: provider %s status query failed: %v", p.Owner, err)
			} else if svc, ok := status.Services[service]; ok && svc.Ready() {
				ep, url, err := d.endpointFrom(leaseStatus{
					Services:       status.Services,
					ForwardedPorts: status.ForwardedPorts,
					IPs:            status.IPs,
				}, service, kind)
				if err == nil {
					d.Logf("akash: %s answered before the Console API did", p.Owner)
					return ep, url, nil
				}
				last = err.Error()
			}
		}

		if !d.Now().Before(deadline) {
			return state.Endpoint{}, "", fmt.Errorf("dseq %s did not become routable within %s: %s", l.DSeq, wait, last)
		}
		if attempt == 1 || attempt%4 == 0 {
			d.Logf("akash: dseq %s not ready yet: %s", l.DSeq, last)
		}
		if err := d.sleep(ctx, poll); err != nil {
			return state.Endpoint{}, "", err
		}
	}
}

// leaseState is one lease's worth of the deployment detail.
type leaseState struct {
	found  bool
	active bool
	ready  bool
	status leaseStatus
	price  float64
	denom  string
}

// leaseStatus reads the deployment and picks out our lease.
func (d *Driver) leaseStatus(ctx context.Context, l state.Lease, service string) (leaseState, error) {
	var out deploymentDetail
	if err := d.Client.do(ctx, "GET", "/v1/deployments/"+l.DSeq, nil, &out); err != nil {
		return leaseState{}, err
	}
	for _, ld := range out.Data.Leases {
		// gseq and oseq are zero until we have chosen a bid, so a lease is matched
		// on whatever identity we actually hold.
		if l.Provider != "" && ld.ID.Provider != l.Provider {
			continue
		}
		if l.GSeq != 0 && ld.ID.GSeq != l.GSeq {
			continue
		}
		if l.OSeq != 0 && ld.ID.OSeq != l.OSeq {
			continue
		}
		st := leaseState{
			found:  true,
			active: strings.EqualFold(ld.State, leaseStateActive),
			status: ld.Status,
			price:  ld.Price.Amount.F(),
			denom:  ld.Price.Denom,
		}
		if svc, ok := ld.Status.Services[service]; ok {
			st.ready = svc.Ready()
		}
		return st, nil
	}
	return leaseState{}, fmt.Errorf("dseq %s reports no lease matching provider %q", l.DSeq, l.Provider)
}

// addrKind is what a caller needs out of a lease status.
//
// This replaced a bool named requireIP, which worked only while "no dedicated IP"
// and "this is the controller" were the same statement. Once the game server could
// also run on a shared endpoint there were two shared-endpoint callers wanting
// completely different answers out of the same forwarded_ports map — a game address
// and an HTTP URL — and a bool cannot say which.
type addrKind int

const (
	// addrDedicatedIP is an address we leased: it goes in the zone as an A record.
	addrDedicatedIP addrKind = iota
	// addrSharedGame is the provider's hostname plus whichever UDP port it chose.
	addrSharedGame
	// addrSharedURL is the controller's own HTTP URL.
	addrSharedURL
)

// endpointFrom turns a provider-reported status into an address.
//
// A dedicated IP and a shared endpoint are genuinely different things and the
// caller says which it needs, because accepting the wrong one is a server players
// cannot reach: v1 wrote the provider's hostname into a DNS A record more than
// once, and an A record containing a hostname is simply broken. What makes that
// safe now is not this function but dns.recordType, which picks CNAME for anything
// that does not parse as an IP — so a Host reaching the zone is written correctly
// rather than silently producing a dead record.
func (d *Driver) endpointFrom(st leaseStatus, service string, kind addrKind) (state.Endpoint, string, error) {
	if kind == addrDedicatedIP {
		ips := st.IPs[service]
		if len(ips) == 0 {
			return state.Endpoint{}, "", fmt.Errorf("no dedicated IP assigned to %s yet", service)
		}
		game := d.Cfg.Server.Ports.Game
		ep := state.Endpoint{
			GamePort: portFor(ips, game, game),
			UDPPort:  portFor(ips, d.Cfg.Server.Ports.UDP, d.Cfg.Server.Ports.UDP),
		}
		if d.Cfg.Server.RCON.Enabled {
			ep.RCONPort = portFor(ips, d.Cfg.Server.RCON.Port, d.Cfg.Server.RCON.Port)
		}
		// Prefer the address the game port is actually mapped on; with a dedicated
		// IP every entry carries the same one, but "every entry" has been an empty
		// set before now.
		for _, e := range ips {
			if e.Port == game && e.IP != "" {
				ep.IP = e.IP
				break
			}
		}
		if ep.IP == "" {
			ep.IP = ips[0].IP
		}
		if !ep.Ready() {
			return state.Endpoint{}, "", fmt.Errorf("%s reports an incomplete address (ip %q, game port %d)",
				service, ep.IP, ep.GamePort)
		}
		return ep, "", nil
	}

	if kind == addrSharedGame {
		return d.sharedGameEndpoint(st, service)
	}

	// Shared endpoint, HTTP: the provider's hostname and a port it chose. The
	// Endpoint stays empty on purpose — it means "where players connect", and
	// nobody plays on the controller.
	//
	// An HTTP ingress URI comes first when there is one: a service exposed as port
	// 80 gets a hostname and no forwarded port at all, so reading only
	// forwarded_ports would wait forever for something that will never appear.
	if svc, ok := st.Services[service]; ok {
		for _, u := range svc.URIs {
			if u = strings.TrimSpace(u); u != "" {
				if strings.Contains(u, "://") {
					return state.Endpoint{}, u, nil
				}
				return state.Endpoint{}, "http://" + u, nil
			}
		}
	}
	fwd := st.ForwardedPorts[service]
	if len(fwd) == 0 {
		return state.Endpoint{}, "", fmt.Errorf("no forwarded port assigned to %s yet", service)
	}
	want := d.Cfg.Controller.HTTPPort
	for _, f := range fwd {
		if f.Port == want && f.Host != "" && f.ExternalPort > 0 {
			return state.Endpoint{}, fmt.Sprintf("http://%s:%d", f.Host, f.ExternalPort), nil
		}
	}
	return state.Endpoint{}, "", fmt.Errorf("%s has no forwarded port for %d yet", service, want)
}

// sharedGameEndpoint reads a playable address off a shared endpoint: the
// provider's own hostname, and whichever external port it assigned.
//
// The assigned port is neither predictable nor negotiable. A shared endpoint
// ignores the SDL's `as:` and allocates from its own pool — our controller asked
// for 8000 and was handed 31188 — so being given a port nobody requested is the
// normal case rather than a fault. That is exactly why Endpoint carries its ports
// explicitly instead of assuming the configured ones.
//
// UDPPort is left at zero when the provider forwards only one port, because on
// this path it is a report of what exists rather than a default to fall back on.
func (d *Driver) sharedGameEndpoint(st leaseStatus, service string) (state.Endpoint, string, error) {
	fwd := st.ForwardedPorts[service]
	if len(fwd) == 0 {
		return state.Endpoint{}, "", fmt.Errorf("no forwarded port assigned to %s yet", service)
	}
	game := d.Cfg.Server.Ports.Game
	udp := d.Cfg.Server.Ports.UDP
	var ep state.Endpoint
	for _, f := range fwd {
		if f.Host == "" || f.ExternalPort <= 0 {
			continue
		}
		switch {
		case f.Port == game && udpish(f.Proto):
			ep.Host, ep.GamePort = f.Host, f.ExternalPort
		case udp > 0 && f.Port == udp && udpish(f.Proto):
			ep.UDPPort = f.ExternalPort
		case d.Cfg.Server.RCON.Enabled && f.Port == d.Cfg.Server.RCON.Port:
			ep.RCONPort = f.ExternalPort
		}
	}
	if !ep.Ready() {
		return state.Endpoint{}, "", fmt.Errorf("%s has no forwarded udp port for %d yet", service, game)
	}
	// No URL: this is a game address, and nothing here speaks HTTP.
	return ep, "", nil
}

// udpish accepts an unset proto as well as "udp". The field is informational in
// some provider implementations and a blank one has been seen in the wild; a
// service whose SDL exposes nothing but UDP cannot have been handed a TCP forward,
// so refusing to read the port over an empty string would time out a lease that is
// in fact working — and that timeout is paid for, because the lease is already
// billing by then.
func udpish(proto string) bool {
	p := strings.TrimSpace(proto)
	return p == "" || strings.EqualFold(p, "udp")
}

// portFor returns the external port a container port was mapped to, or def.
func portFor(ips []leaseIP, container, def int) int {
	for _, e := range ips {
		if e.Port == container && e.ExternalPort > 0 {
			return e.ExternalPort
		}
	}
	return def
}

// --- asking the provider directly ---

// providerStatus queries the provider's own lease status endpoint, which is what
// v1 did with `curl -sk` and a scoped JWT.
func (d *Driver) providerStatus(ctx context.Context, l state.Lease, p Provider) (providerStatusResponse, error) {
	var out providerStatusResponse
	host := strings.TrimRight(strings.TrimSpace(p.HostURI), "/")
	if host == "" {
		return out, fmt.Errorf("provider %s publishes no hostUri", p.Owner)
	}
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	token, err := d.statusJWT(ctx)
	if err != nil {
		return out, err
	}

	url := fmt.Sprintf("%s/lease/%s/%d/%d/status", host, l.DSeq, l.GSeq, l.OSeq)
	timeout := time.Duration(d.Cfg.Akash.ProviderStatus.Timeout)
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil && d.Cfg.Akash.ProviderStatus.InsecureSkipVerify {
		// Provider lease endpoints commonly serve a certificate that does not
		// match hostUri. Verification is attempted first and this is the retry;
		// the exchange is a read-only query carrying a token scoped to "status"
		// and valid for minutes, so what an interceptor could learn is which
		// ports our own lease listens on.
		resp, err = insecureClient(timeout).Do(req)
	}
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()

	body := io.LimitReader(resp.Body, 1<<20)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return out, fmt.Errorf("GET %s: HTTP %d: %s", url, resp.StatusCode, readSnippet(body))
	}
	if err := json.NewDecoder(body).Decode(&out); err != nil {
		return out, fmt.Errorf("decoding the provider status: %w", err)
	}
	return out, nil
}

// statusJWT mints a token scoped to reading lease status and nothing else. The
// scope matters: this token travels to a third-party host, possibly over a
// connection we did not verify.
func (d *Driver) statusJWT(ctx context.Context) (string, error) {
	ttl := int(time.Duration(d.Cfg.Akash.ProviderStatus.JWTTTL).Seconds())
	if ttl <= 0 {
		ttl = 600
	}
	body := map[string]any{"data": map[string]any{
		"ttl": ttl,
		"leases": map[string]any{
			"access": "scoped",
			"scope":  []string{"status"},
		},
	}}
	var out jwtResponse
	if err := d.Client.do(ctx, "POST", "/v1/create-jwt-token", body, &out); err != nil {
		return "", fmt.Errorf("minting a status token: %w", err)
	}
	if out.Data.Token == "" {
		return "", fmt.Errorf("the API returned an empty status token")
	}
	return out.Data.Token, nil
}

func insecureClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // see providerStatus
		},
	}
}
