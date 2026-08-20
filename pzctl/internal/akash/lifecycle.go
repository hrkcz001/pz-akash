package akash

// Closing, checking, adopting, and paying: everything that happens to a lease
// after it exists.
//
// One rule runs through all of it — err toward believing money is still being
// spent. A wrong "it is already closed" leaves a lease billing until the escrow
// drains, which is how v1 lost deposits with nobody watching. A wrong "it is still
// alive" costs one redundant DELETE against a deployment that is already gone, and
// that call answers 404, which this package treats as success.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/denom"
	"github.com/hrkcz001/pz-akash/pzctl/internal/sdl"
	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// Close closes a deployment and stops the billing.
//
// It is idempotent because the controller has to be able to call it again after a
// restart: "closing" is a status it can wake up in. A deployment that is already
// gone answers 404, and a 404 here is the outcome we wanted.
func (d *Driver) Close(ctx context.Context, l state.Lease) error {
	dseq := strings.TrimSpace(l.DSeq)
	if dseq == "" {
		// Nothing to close is not a failure; the FSM calls this on a document whose
		// lease may already have been cleared.
		return nil
	}
	err := d.Client.do(ctx, "DELETE", "/v1/deployments/"+dseq, nil, nil)
	switch {
	case err == nil:
		d.Logf("akash: closed dseq %s", dseq)
	case NotFound(err):
		d.Logf("akash: dseq %s was already gone", dseq)
	default:
		return fmt.Errorf("closing dseq %s: %w", dseq, err)
	}
	// The refund lands a few seconds later. Waiting here means the caller's next
	// deploy sees the reclaimed balance rather than bouncing off a wallet that is
	// briefly short — v1's DEPOSIT_SETTLE_SEC, with a name that says what it is.
	if settle := time.Duration(d.Cfg.Akash.Timeouts.DepositSettle); settle > 0 {
		return d.sleep(ctx, settle)
	}
	return nil
}

// Alive reports whether there is still something to close.
//
// "Alive" is deliberately about the deployment rather than the workload: an open
// deployment holds escrow whether or not anything is running in it. The one case
// that returns false while the deployment is open is a lease the provider closed
// under us — the workload is gone, the FSM needs to know, and the close that
// follows collects what is left of the deposit.
func (d *Driver) Alive(ctx context.Context, l state.Lease) (bool, error) {
	if strings.TrimSpace(l.DSeq) == "" {
		return false, nil
	}
	var out deploymentDetail
	err := d.Client.do(ctx, "GET", "/v1/deployments/"+l.DSeq, nil, &out)
	if NotFound(err) {
		return false, nil
	}
	if err != nil {
		// Unknown, not dead. The caller logs and assumes alive, because assuming
		// dead here would start a second server alongside a live one.
		return false, fmt.Errorf("reading dseq %s: %w", l.DSeq, err)
	}
	switch st := strings.ToLower(strings.TrimSpace(out.Data.Deployment.State)); st {
	case deployStateOpen, leaseStateActive:
		// Still holds escrow.
	case "":
		return false, fmt.Errorf("dseq %s reports no state", l.DSeq)
	default:
		return false, nil
	}
	if ld, ok := matchLease(out.Data.Leases, l); ok {
		return strings.EqualFold(ld.State, leaseStateActive), nil
	}
	// An open deployment with no lease of ours: either we died between creating it
	// and leasing it, or the lease was cleared. Either way the escrow is funded and
	// somebody has to close it.
	return true, nil
}

// matchLease finds the lease in ls that l identifies. Fields we do not yet know —
// a dseq recorded before a bid was chosen has no gseq — match anything.
func matchLease(ls []leaseDetail, l state.Lease) (leaseDetail, bool) {
	for _, ld := range ls {
		if l.Provider != "" && ld.ID.Provider != l.Provider {
			continue
		}
		if l.GSeq != 0 && ld.ID.GSeq != l.GSeq {
			continue
		}
		if l.OSeq != 0 && ld.ID.OSeq != l.OSeq {
			continue
		}
		return ld, true
	}
	return leaseDetail{}, false
}

// Adopt lists deployments that look like ours.
//
// It is the last line of defence for invariant I1: with the state document
// unreadable, this is the only way to find a lease that is still billing. Two
// kinds qualify, and neither can be the controller's own deployment:
//
//   - A deployment running the server service. The service name is the
//     identification, which is why the controller — running a service by another
//     name — is never in the result.
//   - An open deployment with no leases at all, when akash.adopt_unleased is set.
//     That is the shape our own crash between create and lease leaves behind:
//     escrow funded, nothing running, nothing billing it down to zero except the
//     settlement. The controller's deployment always has a lease, so it cannot
//     land here. What can is somebody else's brand-new deployment on the same
//     wallet, mid-creation — hence the switch.
func (d *Driver) Adopt(ctx context.Context) ([]state.Lease, error) {
	var list deploymentList
	if err := d.Client.do(ctx, "GET", "/v1/deployments?limit=1000", nil, &list); err != nil {
		return nil, fmt.Errorf("listing deployments: %w", err)
	}
	var (
		out    []state.Lease
		looked int
	)
	if list.Data.Pagination.HasMore {
		// Worth saying out loud: a truncated list means a lease we did not look at,
		// and the whole point of Adopt is that nothing is missed.
		d.Logf("akash: WARNING the deployment list is paginated (%d total) and only the first page was read",
			list.Data.Pagination.Total)
	}
	for _, dep := range list.Data.Deployments {
		dseq, err := decodeSeq(dep.Deployment.ID.DSeq)
		if err != nil || dseq == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(dep.Deployment.State)) {
		case deployStateOpen, leaseStateActive:
		default:
			continue
		}
		looked++
		// The detail call is not an optimisation to remove. The list's own leases
		// identify the wrong deployment — see deploymentList — and the service name,
		// the only thing that says a deployment is ours, is not in the list at all.
		l, ok, err := d.adoptable(ctx, dseq)
		if err != nil {
			d.Logf("akash: could not inspect dseq %s while adopting: %v", dseq, err)
			continue
		}
		if ok {
			out = append(out, l)
		}
	}
	d.Logf("akash: adopt inspected %d open deployment(s), claimed %d", looked, len(out))
	return out, nil
}

// adoptable decides whether one deployment is ours.
func (d *Driver) adoptable(ctx context.Context, dseq string) (state.Lease, bool, error) {
	var out deploymentDetail
	if err := d.Client.do(ctx, "GET", "/v1/deployments/"+dseq, nil, &out); err != nil {
		if NotFound(err) {
			return state.Lease{}, false, nil
		}
		return state.Lease{}, false, err
	}
	for _, ld := range out.Data.Leases {
		if !strings.EqualFold(ld.State, leaseStateActive) {
			continue
		}
		if _, ok := ld.Status.Services[sdl.ServerService]; !ok {
			continue
		}
		return state.Lease{
			DSeq:     dseq,
			GSeq:     ld.ID.GSeq,
			OSeq:     ld.ID.OSeq,
			Provider: ld.ID.Provider,
		}, true, nil
	}
	if len(out.Data.Leases) == 0 && d.Cfg.Akash.AdoptUnleased {
		d.Logf("akash: dseq %s is open with no lease — adopting it so its deposit is not stranded", dseq)
		return state.Lease{DSeq: dseq}, true, nil
	}
	return state.Lease{}, false, nil
}

// --- escrow ---

// Escrow is what a deployment has left to spend and what it has already paid.
type Escrow struct {
	// RemainingUSD is the unspent deposit. Priced, not raw, because a number
	// without its denomination is how the whole class of bugs in this system
	// started.
	RemainingUSD float64
	SpentUSD     float64
	Denom        string
	// Known is false when the escrow reports only denominations this build cannot
	// price. That is emphatically not the same as zero: topping up against a wrong
	// zero spends real money on a deployment that needed nothing.
	Known bool
}

// Escrow reads a deployment's escrow account.
func (d *Driver) Escrow(ctx context.Context, dseq string) (Escrow, error) {
	var out deploymentDetail
	if err := d.Client.do(ctx, "GET", "/v1/deployments/"+dseq, nil, &out); err != nil {
		return Escrow{}, fmt.Errorf("reading the escrow on dseq %s: %w", dseq, err)
	}
	want := d.Cfg.Akash.Price.Denom
	rate, err := d.rate(ctx)
	if err != nil {
		return Escrow{}, err
	}
	perUnit, err := denom.USDPerUnit(want, rate)
	if err != nil {
		return Escrow{}, err
	}
	e := Escrow{Denom: want}
	funds, haveFunds := pick(out.Data.EscrowAccount.State.Funds, want)
	spent, _ := pick(out.Data.EscrowAccount.State.Transferred, want)
	if !haveFunds {
		return e, nil
	}
	e.Known = true
	e.RemainingUSD = funds * perUnit
	e.SpentUSD = spent * perUnit
	return e, nil
}

// TopUp adds usd to a deployment's escrow.
//
// The floor is akash.funds.min_topup_usd: the API rejects deposits below its own
// minimum, and a loop that retries a rejected two-cent top-up every tick is a loop
// that never funds anything. Rounding up to the floor spends slightly more than
// asked, which is the safe direction — the alternative is a lease that expires
// mid-session.
func (d *Driver) TopUp(ctx context.Context, dseq string, usd float64) (float64, error) {
	if usd <= 0 {
		return 0, nil
	}
	if floor := d.Cfg.Akash.Funds.MinTopupUSD; usd < floor {
		d.Logf("akash: raising a $%.2f top-up to the $%.2f minimum the API accepts", usd, floor)
		usd = floor
	}
	body := map[string]any{"data": map[string]any{
		"dseq":    dseq,
		"deposit": usd,
	}}
	if err := d.Client.do(ctx, "POST", "/v1/deposit-deployment", body, nil); err != nil {
		return 0, fmt.Errorf("topping up dseq %s by $%.2f: %w", dseq, usd, err)
	}
	d.Logf("akash: added $%.2f to the escrow on dseq %s", usd, dseq)
	return usd, nil
}
