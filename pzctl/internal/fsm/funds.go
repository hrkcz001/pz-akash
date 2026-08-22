package fsm

// The escrow top-up loop: what keeps a provider from closing a lease with the world
// still inside it.
//
// An Akash deployment spends from a deposit. Deploy funds it with
// akash.initial_deposit_days at the *ceiling* price — deliberately small, since that
// deposit only has to be large enough to get the lease created — and the lease then
// bills at whatever it was actually won for. Nothing else ever adds to it. Left
// alone the escrow empties, the provider closes the lease, and the controller learns
// about it from reconcileLease after the fact, with the world rolled back to its last
// backup. On the live deployment that deadline was three days out from the deploy.
//
// So this exists, and it is the only part of the controller that spends money on its
// own initiative. Everything about its shape follows from that: it runs on the
// housekeeping tick rather than inside advance — which is a function of the documents
// and the clock and nothing else — it refuses to act on a balance it is not certain
// of, and it sizes every deposit from the price the lease is actually billing at
// rather than from the ceiling config permits.

import "context"

// topUpEscrow keeps the current lease's deposit at akash.deploy_days of runway.
//
// The gap between the level that triggers a top-up and the level it tops up to is
// deliberate. It acts only once the balance has fallen below the horizon, and then
// funds to the horizon times akash.funds.margin, so "enough" and "just topped up"
// are never the same number. Without that gap a lease sitting a cent under the
// horizon would be topped up on every check for the rest of its life, and each one
// is a real transaction.
func (m *Machine) topUpEscrow(ctx context.Context) {
	f := m.cfg.Akash.Funds
	if f.CheckInterval <= 0 {
		// Disabled, which is a supported configuration: an operator may prefer to fund
		// the escrow by hand rather than hand the controller a wallet to spend from.
		return
	}
	if !m.lastFunds.IsZero() && m.now().Sub(m.lastFunds) < f.CheckInterval.D() {
		return
	}

	l := m.doc.Lease
	switch {
	case l == nil || l.DSeq == "":
		return
	case !m.doc.Status.Billing():
		// Either nothing is draining, or the lease is already on its way out. Funding
		// a deployment the machine is closing would put money into an escrow whose
		// refund has been asked for.
		return
	}

	// Stamped before the reading rather than after it, so a check that fails still
	// waits a full interval before the next attempt. Otherwise a provider API that is
	// down turns every tick into another request against something that is already
	// rate-limiting us.
	m.lastFunds = m.now()

	perDay := m.doc.Price.USDPerDay
	if perDay <= 0 {
		// A day of runway costs whatever the lease was won for, so without that price
		// "two days of escrow" is not a number. Falling back to
		// akash.price.max_usd_per_day would fund a cheap lease to several times the
		// horizon that was asked for.
		m.logf("fsm: funds: dseq %s has no recorded price; cannot size a top-up", l.DSeq)
		return
	}

	remaining, known, err := m.akash.Escrow(ctx, l.DSeq)
	if err != nil {
		// Logged, and deliberately not recorded as the document's last error: a
		// reading we failed to take says nothing about the lease, and the next tick
		// will try again. What it does cost is one check's worth of delay, which is
		// why the interval is set well below the horizon it defends.
		m.logf("fsm: funds: reading the escrow on dseq %s: %v", l.DSeq, err)
		return
	}
	if !known {
		// Not zero. See fsm.Akash.Escrow: an unpriceable balance and an empty one are
		// the same number to anyone who guesses, and only one of them is a reason to
		// spend.
		m.logf("fsm: funds: the escrow on dseq %s holds nothing this build can price; not topping up", l.DSeq)
		return
	}

	// One line per check, on purpose. It is the answer to "how long has my server
	// got", and the alternative — logging only when something is wrong — leaves an
	// operator unable to tell a healthy loop from one that never ran.
	floor := perDay * float64(m.cfg.Akash.DeployDays)
	m.logf("fsm: funds: dseq %s has $%.2f left, %.1f day(s) at $%.2f/day",
		l.DSeq, remaining, remaining/perDay, perDay)
	if remaining >= floor {
		return
	}

	margin := f.Margin
	if margin < 1 {
		// config.validate rejects a margin below 1, so reaching this means a Config
		// built by hand in a test. Clamping keeps the arithmetic below from asking for
		// a negative deposit.
		margin = 1
	}
	added, err := m.akash.TopUp(ctx, l.DSeq, floor*margin-remaining)
	if err != nil {
		m.logf("fsm: funds: topping up dseq %s: %v", l.DSeq, err)
		return
	}
	m.logf("fsm: funds: dseq %s was %.1f day(s) from empty; added $%.2f for %.1f day(s) of runway",
		l.DSeq, remaining/perDay, added, (remaining+added)/perDay)
}
