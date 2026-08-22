package fsm

import (
	"context"
	"math"
	"testing"
	"time"
)

// The funds loop, which is the only part of the controller that spends money without
// being asked to. Every test here is about a way of spending it wrongly: too often,
// on a balance nobody read correctly, or sized from a price that was never quoted.
// The one test about spending it rightly is first.

// escrowOf reads the simulated provider's balance for the lease the document holds.
func escrowOf(t *testing.T, h *harness) float64 {
	t.Helper()
	if h.m.doc.Lease == nil {
		t.Fatal("the document holds no lease")
	}
	remaining, known, err := h.dry.Escrow(t.Context(), h.m.doc.Lease.DSeq)
	if err != nil {
		t.Fatal(err)
	}
	if !known {
		t.Fatal("the simulated escrow reports an unpriceable balance")
	}
	return remaining
}

func TestFundsLoopTopsUpAnEscrowBelowTheHorizon(t *testing.T) {
	h := newHarness(t, nil)
	h.bringOnline()

	// Half a day of billing against a deposit that only ever covered
	// initial_deposit_days at the ceiling price. This is the live situation: the world
	// was deployed with one day of deposit and burns for less than that per day, so it
	// sits below the two-day horizon from the moment it comes up.
	//
	// The clock moves before the balance is read, not after, so that the drain is
	// already in both numbers and the only difference between them can be a deposit.
	h.clk.add(12 * time.Hour)
	before := escrowOf(t, h)
	h.tick()

	after := escrowOf(t, h)
	if after <= before {
		h.dumpLogs()
		t.Fatalf("escrow went from $%.2f to $%.2f; the loop did not top up a lease below the horizon", before, after)
	}

	// Funded to deploy_days * margin at the price the lease is actually billing at.
	// Sizing it from akash.price.max_usd_per_day instead would fund this lease to four
	// days while claiming two, which is the failure that looks like success.
	want := h.dry.pricePerDay() * float64(h.cfg.Akash.DeployDays) * h.cfg.Akash.Funds.Margin
	if math.Abs(after-want) > 0.01 {
		h.dumpLogs()
		t.Errorf("escrow = $%.2f after the top-up, want $%.2f (%d day(s) at $%.2f/day, margin %g)",
			after, want, h.cfg.Akash.DeployDays, h.dry.pricePerDay(), h.cfg.Akash.Funds.Margin)
	}
}

// A funded lease must not be topped up again on the next tick.
//
// This is the band between the level that triggers a top-up and the level it funds to.
// Without it a lease one cent under the horizon would be topped up on every check
// forever — each one a real transaction, and the whole point of check_interval is that
// transactions are not free.
func TestFundsLoopDoesNotTopUpAgainWhileFunded(t *testing.T) {
	h := newHarness(t, nil)
	h.bringOnline()

	h.clk.add(12 * time.Hour)
	h.tick()
	// Counted on the loop's own line rather than on "added $", which the simulated
	// provider also logs — the assertion is about how many times the loop decided to
	// spend, not how many lines mention money.
	if n := h.logCount("of runway"); n != 1 {
		h.dumpLogs()
		t.Fatalf("%d top-up(s) on the first tick, want 1", n)
	}

	// Immediately again: refused by check_interval, before any provider call.
	h.tick()
	// And again past the interval, where the rate limit no longer applies and the
	// balance itself has to be what stops it.
	h.clk.add(2 * h.cfg.Akash.Funds.CheckInterval.D())
	h.tick()

	if n := h.logCount("of runway"); n != 1 {
		h.dumpLogs()
		t.Errorf("%d top-up(s) after three ticks, want 1", n)
	}
}

// blindEscrow reports a balance it cannot price, and counts any attempt to spend
// against it. That is what an escrow denominated in something the client cannot
// convert looks like, and it is indistinguishable from an empty one to anyone who
// treats "unknown" as zero.
type blindEscrow struct {
	Akash
	tops int
}

func (b *blindEscrow) Escrow(context.Context, string) (float64, bool, error) {
	return 0, false, nil
}

func (b *blindEscrow) TopUp(ctx context.Context, dseq string, usd float64) (float64, error) {
	b.tops++
	return b.Akash.TopUp(ctx, dseq, usd)
}

func TestFundsLoopWillNotSpendAgainstABalanceItCannotRead(t *testing.T) {
	h := newHarness(t, nil)
	h.bringOnline()

	blind := &blindEscrow{Akash: h.m.akash}
	h.m.akash = blind

	h.clk.add(12 * time.Hour)
	h.tick()

	if blind.tops != 0 {
		h.dumpLogs()
		t.Errorf("%d top-up(s) against an unpriceable balance, want 0 — a reading of zero we did not take is not a reason to spend", blind.tops)
	}
	if !h.logged("holds nothing this build can price") {
		h.dumpLogs()
		t.Error("the loop did not say why it declined to top up")
	}
}

// A lease with no recorded price cannot be given a horizon in days.
//
// The tempting fallback is akash.price.max_usd_per_day, which is a ceiling and not a
// price: on a lease won at a sixth of it, "two days" would buy twelve.
func TestFundsLoopWillNotSizeATopUpWithoutALeasePrice(t *testing.T) {
	h := newHarness(t, nil)
	h.bringOnline()

	// The shape of a document written by a build that did not record the price, or of
	// a deploy whose quote never arrived.
	h.m.doc.Price.USDPerDay = 0

	// Read either side of the tick with the clock held still, so the simulated drain
	// cannot be mistaken for the loop having done nothing.
	h.clk.add(12 * time.Hour)
	before := escrowOf(t, h)
	h.tick()

	if after := escrowOf(t, h); after != before {
		h.dumpLogs()
		t.Errorf("escrow went from $%.2f to $%.2f without a price to size the top-up from", before, after)
	}
	if !h.logged("no recorded price") {
		h.dumpLogs()
		t.Error("the loop did not say why it could not size a top-up")
	}
}
