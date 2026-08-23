package akash

// Pricing a deployment without buying it.
//
// The question this exists to answer is "what is the dedicated IP costing us", and
// nothing already in this package can answer it. Console's aggregates are capacity,
// not price. /v1/pricing returns a market average that does not know endpoints exist.
// And the two effects of asking for an `endpoints: kind: ip` pull in opposite
// directions: it adds a resource the provider prices, and it removes every provider
// that has no IP to give — 8 of 60 online providers advertise one, and one of those
// cannot actually serve it. A market of three bidders and a market of sixty do not
// have comparable prices, so the only honest way to size the difference is to ask the
// same market the same question twice, once each way.
//
// So a quote does exactly what a deploy does up to the point where money starts
// moving — render, pick eligible providers, create, collect bids — and then closes
// the deployment instead of taking a lease. No lease means no provider ever bills.
// What it costs is the deposit, locked between the create and the close, and the
// chain fees on the two transactions.

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/denom"
	"github.com/hrkcz001/pz-akash/pzctl/internal/sdl"
	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// Quote is one provider's bid on a spec nobody took.
type Quote struct {
	Provider string
	Country  string
	// AmountPerBlock and Denom are the bid as the chain states it; USDPerDay is that
	// converted, and is the only number worth comparing across denominations.
	AmountPerBlock float64
	Denom          string
	USDPerDay      float64
	// Won marks the bid SelectBid would have taken. Why carries the reason for the
	// others, which is the more informative half of the table: "too expensive" and
	// "no IP capacity" are different answers to the same question.
	Won bool
	Why string
}

// QuoteResult is a whole round of bidding, with what it was bidding on.
type QuoteResult struct {
	RequireIP  bool
	DSeq       string
	Eligible   int
	Providers  int
	CeilingUSD float64
	Quotes     []Quote
}

// Cheapest is the lowest USD/day among the bids that were actually acceptable, and
// false when none were. Deliberately not "the lowest bid": an unacceptable bid is not
// a price we could have paid, and reporting one as the market rate is how a comparison
// ends up recommending a provider that would have refused the lease.
func (r QuoteResult) Cheapest() (Quote, bool) {
	var best Quote
	var found bool
	for _, q := range r.Quotes {
		if q.Why != "" && !q.Won {
			continue
		}
		if !found || q.USDPerDay < best.USDPerDay {
			best, found = q, true
		}
	}
	return best, found
}

// QuoteServer prices the game server's resource request and closes the deployment
// again without leasing it.
//
// requireIP is the whole point of the parameter: pass true for what we deploy today
// and false for the same machine on a shared endpoint, and the difference between the
// two rounds is the price of the address.
//
// The deployment is closed on every path, including the ones that fail. That is not
// politeness — an unclosed quote is a funded escrow with no lease, which is money
// sitting in a deployment that will never do anything.
func (d *Driver) QuoteServer(ctx context.Context, requireIP bool) (res QuoteResult, err error) {
	res.RequireIP = requireIP

	aktUSD, err := d.rate(ctx)
	if err != nil {
		return res, err
	}
	ceiling, err := d.ceiling(aktUSD)
	if err != nil {
		return res, err
	}
	res.CeilingUSD = d.Cfg.Akash.Price.MaxUSDPerDay

	cr, err := CriteriaFor(d.Cfg, d.Cfg.Server.Resources, requireIP, aktUSD)
	if err != nil {
		return res, err
	}

	// A copy, with the one flag flipped. RenderServer reads ip_lease out of the
	// config to decide whether to emit the endpoints block, and the no-IP round has
	// to render the SDL it is pricing rather than the one we deploy. Shallow is
	// enough: nothing below writes through a shared slice or map.
	cfg := *d.Cfg
	cfg.Server.IPLease = requireIP
	doc, err := sdl.RenderServer(sdl.Input{
		Cfg:              &cfg,
		Secrets:          d.Secrets,
		MaxPricePerBlock: ceiling,
	})
	if err != nil {
		return res, fmt.Errorf("rendering the server SDL: %w", err)
	}

	providers, err := d.EligibleProviders(ctx, cr)
	if err != nil {
		return res, err
	}
	res.Eligible = len(providers)
	if len(providers) == 0 {
		// Not an error. "Nobody can host this" is a perfectly good answer to what a
		// configuration costs, and it is the answer for ip_lease on most of the
		// network.
		return res, nil
	}

	dseq, _, err := d.create(ctx, doc, d.deposit())
	res.DSeq = dseq
	if dseq != "" {
		// Closed even if the create reported an error, and before that error is
		// returned: from here on there is a funded escrow.
		defer func() {
			if cerr := d.Close(ctx, state.Lease{DSeq: dseq}); cerr != nil && err == nil {
				err = fmt.Errorf("quote on dseq %s: closing it again: %w", dseq, cerr)
			}
		}()
	}
	if err != nil {
		return res, err
	}
	d.Logf("akash: quote dseq %s created (deposit $%.2f, ip_lease %v); it will be closed again",
		dseq, d.deposit(), requireIP)

	res.Quotes, err = d.collectBids(ctx, dseq, cr, providers, aktUSD)
	return res, err
}

// collectBids polls for the full bid_wait rather than stopping at the first
// acceptable bid.
//
// A deploy wants one bid and wants it soon; a quote wants the market. Returning early
// on the first acceptable bid would systematically under-report how many providers
// answered, and the count is half of what makes the two rounds comparable.
func (d *Driver) collectBids(ctx context.Context, dseq string, cr Criteria, providers []Provider, aktUSD float64) ([]Quote, error) {
	var (
		poll     = time.Duration(d.Cfg.Akash.Timeouts.BidPoll)
		deadline = d.Now().Add(time.Duration(d.Cfg.Akash.Timeouts.BidWait))
		seen     = map[string]Quote{}
		country  = map[string]string{}
	)
	for _, p := range providers {
		country[p.Owner] = p.CountryCode()
	}

	for {
		var list bidList
		if err := d.Client.do(ctx, "GET", "/v1/bids?dseq="+dseq, nil, &list); err != nil && !NotFound(err) {
			return nil, fmt.Errorf("reading bids on dseq %s: %w", dseq, err)
		}

		// Run the real selector over the real bids, so the table says what the deploy
		// path would have done rather than what this file thinks it would have done.
		choice, bad, err := SelectBid(cr, list, providers)
		if err != nil {
			return nil, err
		}
		why := map[string]string{}
		for _, r := range bad {
			why[r.Owner] = r.Why
		}

		for _, b := range list {
			usd, cerr := denom.USDPerDay(b.Price.Amount.F(), b.Price.Denom, d.Cfg.Akash.BlocksPerDay, aktUSD)
			if cerr != nil {
				// An unpriceable denomination is recorded as the bid it is, with the
				// conversion failure as its reason. Dropping it would hide a bidder.
				usd = 0
			}
			q := Quote{
				Provider:       b.ID.Provider,
				Country:        country[b.ID.Provider],
				AmountPerBlock: b.Price.Amount.F(),
				Denom:          b.Price.Denom,
				USDPerDay:      usd,
				Why:            why[b.ID.Provider],
			}
			if cerr != nil && q.Why == "" {
				q.Why = cerr.Error()
			}
			if choice != nil && choice.Bid.ID.Provider == b.ID.Provider {
				q.Won, q.Why = true, ""
			}
			seen[b.ID.Provider] = q
		}

		if !d.Now().Before(deadline) {
			break
		}
		if err := d.sleep(ctx, poll); err != nil {
			return nil, err
		}
	}

	out := make([]Quote, 0, len(seen))
	for _, q := range seen {
		out = append(out, q)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].USDPerDay < out[j].USDPerDay })
	return out, nil
}
