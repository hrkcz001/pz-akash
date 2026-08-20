package config

// Validation tests for the rules that exist because of a specific v1 failure:
// the denomination arithmetic that made every bid look unaffordable, the
// hand-deploy price placeholder that could contradict the dollar limit next to
// it, and the DNS names that end up in a Cloudflare record.

import (
	"strings"
	"testing"
)

func TestValidateRejectsAnUnknownDenom(t *testing.T) {
	c := mustLoadReal(t)
	c.Akash.Price.Denom = "uusdc"
	c.Akash.Price.AllowedDenoms = []string{"uusdc"}
	err := c.Validate()
	if err == nil {
		t.Fatal("a denomination this build cannot convert to USD was accepted")
	}
	// Both the ceiling we bid in and the prices we read back have to be
	// convertible, so both are reported.
	for _, want := range []string{"akash.price.denom", "akash.price.allowed_denoms[0]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%v", want, err)
		}
	}
}

// TestValidateRequiresTheBidDenomToBeReadable: bidding in a denomination we would
// then refuse to price is a deploy that discards its own winning bid.
func TestValidateRequiresTheBidDenomToBeReadable(t *testing.T) {
	c := mustLoadReal(t)
	c.Akash.Price.Denom = "uakt"
	c.Akash.Price.AllowedDenoms = []string{"uact"}
	err := c.Validate()
	if err == nil {
		t.Fatal("a bid denomination outside allowed_denoms was accepted")
	}
	if !strings.Contains(err.Error(), "must also appear in allowed_denoms") {
		t.Errorf("error does not explain the contradiction:\n%v", err)
	}
}

// TestValidateRequiresARateSourceForAKT: uact is dollar-pegged and needs no
// oracle, which is the whole reason it is the default. uakt does, and a deploy
// that discovers that at bid time fails with "no bids found".
func TestValidateRequiresARateSourceForAKT(t *testing.T) {
	base := func(t *testing.T) *Config {
		c := mustLoadReal(t)
		c.Akash.Price.Denom = "uakt"
		c.Akash.Price.AllowedDenoms = []string{"uakt", "uact"}
		return c
	}

	c := base(t)
	c.Akash.Price.PriceOracleURL = ""
	c.Akash.Price.AKTUSDFallback = 0
	if err := c.Validate(); err == nil {
		t.Fatal("an AKT-denominated config with no rate source was accepted")
	} else if !strings.Contains(err.Error(), "price_oracle_url or akt_usd_fallback") {
		t.Errorf("error does not name the two ways out:\n%v", err)
	}

	// Either source on its own is enough.
	c = base(t)
	c.Akash.Price.PriceOracleURL = ""
	c.Akash.Price.AKTUSDFallback = 3.5
	if err := c.Validate(); err != nil {
		t.Errorf("a fallback rate should satisfy the requirement: %v", err)
	}

	c = base(t)
	c.Akash.Price.AKTUSDFallback = 0
	if err := c.Validate(); err != nil {
		t.Errorf("an oracle URL should satisfy the requirement: %v", err)
	}
}

// TestValidateRejectsAPlaceholderAboveTheDollarLimit: pricing_amount is what a
// hand-deploy bids, and it sits three sections away from the limit it must
// respect. Checking it is the only thing that keeps the two honest.
func TestValidateRejectsAPlaceholderAboveTheDollarLimit(t *testing.T) {
	// 10000 uact/block over 14400 blocks is $144/day against a $3 limit.
	c := mustLoadReal(t)
	c.Server.PricingAmount = 10000
	err := c.Validate()
	if err == nil {
		t.Fatal("a hand-deploy ceiling far above max_usd_per_day was accepted")
	}
	if !strings.Contains(err.Error(), "server.pricing_amount") ||
		!strings.Contains(err.Error(), "max_usd_per_day") {
		t.Errorf("error does not connect the two settings:\n%v", err)
	}

	c = mustLoadReal(t)
	c.Controller.PricingAmount = 10000
	if err := c.Validate(); err == nil {
		t.Fatal("the controller's ceiling is not checked against the same limit")
	}

	// The shipping values must be inside the limit, or the check is decorative.
	if err := mustLoadReal(t).Validate(); err != nil {
		t.Fatalf("the shipping config does not satisfy its own price check: %v", err)
	}
}

// TestValidateSkipsAnUnpriceablePlaceholder: with an AKT-denominated ceiling the
// comparison needs a live rate, so it cannot be made at load time. Guessing one
// would reject a valid config, so the check stands down.
func TestValidateSkipsAnUnpriceablePlaceholder(t *testing.T) {
	c := mustLoadReal(t)
	c.Akash.Price.Denom = "uakt"
	c.Akash.Price.AllowedDenoms = []string{"uakt", "uact"}
	c.Akash.Price.AKTUSDFallback = 3.5
	c.Server.PricingAmount = 10000
	if err := c.Validate(); err != nil {
		t.Errorf("an unpriceable placeholder should not fail validation: %v", err)
	}
}

func TestValidateChecksMinUptime(t *testing.T) {
	for _, v := range []float64{-0.1, 1.5} {
		c := mustLoadReal(t)
		c.Akash.Placement.MinUptime30d = v
		if err := c.Validate(); err == nil {
			t.Errorf("min_uptime_30d %g was accepted; it is a fraction, not a percentage", v)
		}
	}
	// 0 disables the check; 1 demands perfection. Both are meaningful.
	for _, v := range []float64{0, 0.95, 1} {
		c := mustLoadReal(t)
		c.Akash.Placement.MinUptime30d = v
		if err := c.Validate(); err != nil {
			t.Errorf("min_uptime_30d %g was rejected: %v", v, err)
		}
	}
}

// --- akash.api ---

func TestValidateChecksAPIClientSettings(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"negative retries", func(c *Config) { c.Akash.API.Retries = -1 }, "akash.api.retries"},
		{"no retry wait", func(c *Config) { c.Akash.API.RetryWait = 0 }, "akash.api.retry_wait"},
		{"no timeout", func(c *Config) { c.Akash.API.Timeout = 0 }, "akash.api.timeout"},
		{
			// One stalled connection would otherwise consume the entire bid window,
			// and the deploy would report "no provider bid at all" for a request that
			// was never answered.
			"a timeout longer than the bid window",
			func(c *Config) { c.Akash.API.Timeout = c.Akash.Timeouts.BidWait + 1 },
			"exceeds akash.timeouts.bid_wait",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mustLoadReal(t)
			tc.mutate(c)
			err := c.Validate()
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not mention %q:\n%v", tc.want, err)
			}
		})
	}

	// Zero retries is a legitimate choice: fail fast and let the FSM's attempt
	// loop decide, rather than retrying inside a call it is already timing.
	c := mustLoadReal(t)
	c.Akash.API.Retries = 0
	if err := c.Validate(); err != nil {
		t.Errorf("retries: 0 was rejected: %v", err)
	}
}

// --- dns.game_record ---

func TestValidateChecksGameRecord(t *testing.T) {
	cases := []struct {
		name string
		rec  string
		ok   bool
	}{
		{"the shipping value", "pz", true},
		{"a hyphenated label", "pz-eu", true},
		{"empty disables the record", "", true},
		{"a trailing dot", "pz.", false},
		{"a leading hyphen", "-pz", false},
		{"a trailing hyphen", "pz-", false},
		{"a space", "pz server", false},
		{"an underscore", "pz_server", false},
		{"a label over 63 characters", strings.Repeat("a", 64), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mustLoadReal(t)
			c.DNS.GameRecord = tc.rec
			err := c.Validate()
			if tc.ok && err != nil {
				t.Fatalf("game_record %q was rejected: %v", tc.rec, err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatalf("game_record %q was accepted", tc.rec)
				}
				if !strings.Contains(err.Error(), "dns.game_record") {
					t.Errorf("error does not name the field:\n%v", err)
				}
			}
		})
	}
}

// TestValidateRejectsAGameRecordCollision: include_www points www at the
// controller. Pointing the same name at the game server's IP would break the
// dashboard on the next deploy, and the two records are written by different code
// paths, so nothing else would notice.
func TestValidateRejectsAGameRecordCollision(t *testing.T) {
	c := mustLoadReal(t)
	c.DNS.IncludeWWW = true
	c.DNS.GameRecord = "www"
	err := c.Validate()
	if err == nil {
		t.Fatal("game_record www was accepted alongside include_www")
	}
	if !strings.Contains(err.Error(), "include_www") {
		t.Errorf("error does not name the setting it collides with:\n%v", err)
	}

	// Without include_www the name is free.
	c = mustLoadReal(t)
	c.DNS.IncludeWWW = false
	c.DNS.GameRecord = "www"
	if err := c.Validate(); err != nil {
		t.Errorf("game_record www was rejected with include_www off: %v", err)
	}
}

func TestGameHost(t *testing.T) {
	c := mustLoadReal(t)
	want := c.DNS.GameRecord + "." + c.DNS.Domain
	if got := c.DNS.GameHost(); got != want {
		t.Errorf("GameHost() = %q, want %q", got, want)
	}

	// Every way of saying "there is no game record" gives the empty string rather
	// than a half-formed name like ".vsrania.online".
	for _, mutate := range []func(*DNS){
		func(d *DNS) { d.Enabled = false },
		func(d *DNS) { d.GameRecord = "" },
		func(d *DNS) { d.Domain = "" },
	} {
		d := mustLoadReal(t).DNS
		mutate(&d)
		if got := d.GameHost(); got != "" {
			t.Errorf("GameHost() = %q, want empty", got)
		}
	}
}
