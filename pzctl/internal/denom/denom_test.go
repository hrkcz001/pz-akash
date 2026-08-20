package denom

import "testing"

// The whole reason this package exists: the same number means different money in
// each denomination, and v1 applied the AKT rate to both.
func TestUACTIsDollarPeggedAndUAKTIsNot(t *testing.T) {
	// The live lease: 34 per block over 14400 blocks/day.
	const amount, bpd = 34.0, 14400

	// uact is 1e6 to the dollar, so this is $0.4896/day — about $15/month, which
	// is what the deployment actually costs. No rate involved, so passing a wild
	// one must change nothing.
	for _, rate := range []float64{0, 1, 3, 1000} {
		got, err := USDPerDay(amount, UACT, bpd, rate)
		if err != nil {
			t.Fatalf("uact at rate %g: %v", rate, err)
		}
		if want := 0.4896; !nearly(got, want) {
			t.Errorf("USDPerDay(34, uact) at rate %g = %v, want %v", rate, got, want)
		}
	}

	// uakt is micro-AKT, so the same 34 costs the AKT price per unit.
	got, err := USDPerDay(amount, UAKT, bpd, 3.0)
	if err != nil {
		t.Fatal(err)
	}
	if want := 1.4688; !nearly(got, want) {
		t.Errorf("USDPerDay(34, uakt, AKT=$3) = %v, want %v", got, want)
	}
}

// This is the failure v1 would hit as AKT appreciates, stated as a test so the
// fix cannot be undone quietly. v1 computed the ceiling as usd*1e6/(rate*bpd),
// which for a dollar-pegged denomination divides by the AKT price for no reason.
func TestUACTCeilingDoesNotShrinkWithTheAKTPrice(t *testing.T) {
	const bpd = 14400
	want, err := CeilingPerBlock(3.0, UACT, bpd, 0)
	if err != nil {
		t.Fatal(err)
	}
	// $3/day / ($1e-6 * 14400) = 208.33 -> 208.
	if want != 208 {
		t.Fatalf("CeilingPerBlock(3, uact, 14400) = %d, want 208", want)
	}
	for _, rate := range []float64{1, 1.2, 3, 5, 25} {
		got, err := CeilingPerBlock(3.0, UACT, bpd, rate)
		if err != nil {
			t.Fatalf("rate %g: %v", rate, err)
		}
		if got != want {
			t.Errorf("AKT at $%g moved the uact ceiling to %d, want %d — the v1 bug", rate, got, want)
		}
	}
	// The live lease at 34 must sit under the ceiling at every one of those
	// rates; under v1's arithmetic at AKT $25 the ceiling was 8 and no bid could
	// ever win.
	if 34 > want {
		t.Errorf("ceiling %d excludes the price the deployment actually pays (34)", want)
	}
}

// A ceiling must never exceed the stated dollar limit, so the conversion
// truncates. Round-tripping it back to dollars proves the direction.
func TestCeilingNeverExceedsTheStatedLimit(t *testing.T) {
	for _, tc := range []struct {
		d    string
		rate float64
	}{
		{UACT, 0},
		{UAKT, 1.2},
		{UAKT, 7.77},
	} {
		for _, limit := range []float64{0.1, 0.5, 3, 12.34} {
			ceil, err := CeilingPerBlock(limit, tc.d, 14400, tc.rate)
			if err != nil {
				// A limit too low to afford even one unit per block is refused
				// outright, which is the same guarantee stated a different way.
				continue
			}
			back, err := USDPerDay(float64(ceil), tc.d, 14400, tc.rate)
			if err != nil {
				t.Fatal(err)
			}
			if back > limit {
				t.Errorf("%s: ceiling %d is $%.4f/day, above the $%.2f limit", tc.d, ceil, back, limit)
			}
		}
	}
}

// An unknown denomination is refused rather than assumed dollar-pegged: guessing
// wrong in that direction accepts a bid costing a million times the intent.
func TestUnknownDenominationIsRefused(t *testing.T) {
	for _, d := range []string{"", "uosmo", "akt", "usd", "uaktx"} {
		if Known(d) {
			t.Errorf("Known(%q) = true", d)
		}
		if _, err := USDPerDay(34, d, 14400, 3); err == nil {
			t.Errorf("USDPerDay with denom %q returned no error", d)
		}
		if _, err := CeilingPerBlock(3, d, 14400, 3); err == nil {
			t.Errorf("CeilingPerBlock with denom %q returned no error", d)
		}
	}
	// Case and stray whitespace come from config files and API responses alike.
	for _, d := range []string{"uact", "UACT", " uAct ", "uakt", "UAKT"} {
		if !Known(d) {
			t.Errorf("Known(%q) = false", d)
		}
	}
}

// uakt without a rate must fail rather than silently price at zero, which would
// make every bid look free and win the most expensive one available.
func TestUAKTRequiresARate(t *testing.T) {
	if NeedsOracle(UACT) {
		t.Error("uact must not need the oracle; that is what keeps CoinGecko off the deploy path")
	}
	if !NeedsOracle(UAKT) {
		t.Error("uakt must need the oracle")
	}
	for _, rate := range []float64{0, -1} {
		if _, err := USDPerDay(34, UAKT, 14400, rate); err == nil {
			t.Errorf("USDPerDay(uakt) at rate %g returned no error", rate)
		}
		if _, err := CeilingPerBlock(3, UAKT, 14400, rate); err == nil {
			t.Errorf("CeilingPerBlock(uakt) at rate %g returned no error", rate)
		}
	}
}

func TestRejectsNonsenseArguments(t *testing.T) {
	if _, err := USDPerDay(34, UACT, 0, 0); err == nil {
		t.Error("zero blocks per day returned no error")
	}
	if _, err := USDPerDay(-1, UACT, 14400, 0); err == nil {
		t.Error("a negative amount returned no error")
	}
	if _, err := CeilingPerBlock(0, UACT, 14400, 0); err == nil {
		t.Error("a zero USD limit returned no error")
	}
	// A limit so small it truncates to nothing is an error, not a free lease.
	if _, err := CeilingPerBlock(0.000001, UACT, 14400, 0); err == nil {
		t.Error("a limit that rounds the ceiling to 0 returned no error")
	}
}

// nearly compares floats; it is not named close because that would shadow the
// builtin for every test in the package.
func nearly(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}
