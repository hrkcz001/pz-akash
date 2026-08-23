package config

// Validation tests for the rules that exist because of a specific v1 failure:
// the denomination arithmetic that made every bid look unaffordable, the
// hand-deploy price placeholder that could contradict the dollar limit next to
// it, and the DNS names that end up in a Cloudflare record.

import (
	"strings"
	"testing"
	"time"
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

// TestValidateChecksGameTTL: the TTL is the only DNS number with a real cost
// attached. Too long and a redeploy leaves players resolving a dead lease for as
// long as the record says; 1 means "let Cloudflare decide", which is only correct
// for a proxied record and the game record is never proxied. Cloudflare's own floor
// for an unproxied record is 60.
func TestValidateChecksGameTTL(t *testing.T) {
	cases := []struct {
		ttl int
		ok  bool
	}{
		{60, true},
		{300, true},
		{86400, true},
		{1, true}, // automatic
		{0, false},
		{30, false},
		{59, false},
		{86401, false},
		{-1, false},
	}
	for _, tc := range cases {
		c := mustLoadReal(t)
		c.DNS.GameTTL = tc.ttl
		err := c.Validate()
		switch {
		case tc.ok && err != nil:
			t.Errorf("game_ttl %d was rejected: %v", tc.ttl, err)
		case !tc.ok && err == nil:
			t.Errorf("game_ttl %d was accepted", tc.ttl)
		case !tc.ok && !strings.Contains(err.Error(), "dns.game_ttl"):
			t.Errorf("game_ttl %d: error does not name the field:\n%v", tc.ttl, err)
		}
	}

	// With no game record there is no TTL to be wrong about, so the check does not
	// fire: an operator who has turned the record off should not have to keep a
	// number valid for it.
	c := mustLoadReal(t)
	c.DNS.GameRecord = ""
	c.DNS.GameTTL = 0
	if err := c.Validate(); err != nil {
		t.Errorf("game_ttl 0 was rejected with the game record disabled: %v", err)
	}
}

// TestValidateChecksTheCloudflareCall covers the four keys that describe the HTTP
// call itself. They exist because v1 hardcoded all four, and a hardcoded timeout is
// the reason a Cloudflare outage could hold a deploy open indefinitely.
func TestValidateChecksTheCloudflareCall(t *testing.T) {
	cases := []struct {
		name  string
		spoil func(*Config)
		field string
	}{
		{"an api_base that is not a URL", func(c *Config) { c.DNS.APIBase = "api.cloudflare.com/client/v4" }, "dns.api_base"},
		{"an empty api_base", func(c *Config) { c.DNS.APIBase = "" }, "dns.api_base"},
		{"a zero timeout", func(c *Config) { c.DNS.Timeout = Duration(0) }, "dns.timeout"},
		{"a negative timeout", func(c *Config) { c.DNS.Timeout = Duration(-time.Second) }, "dns.timeout"},
		{"negative retries", func(c *Config) { c.DNS.Retries = -1 }, "dns.retries"},
		{"a zero retry_wait", func(c *Config) { c.DNS.RetryWait = Duration(0) }, "dns.retry_wait"},
		{"no zone_id", func(c *Config) { c.DNS.ZoneID = "" }, "dns.zone_id"},
		{"no domain", func(c *Config) { c.DNS.Domain = "" }, "dns.domain"},
		{"an unsupported provider", func(c *Config) { c.DNS.Provider = "route53" }, "dns.provider"},
		{"an unknown ssl_mode", func(c *Config) { c.DNS.SSLMode = "half" }, "dns.ssl_mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mustLoadReal(t)
			tc.spoil(c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error does not name %s:\n%v", tc.field, err)
			}
		})
	}

	// Every one of them is ignored when DNS is off. A deployment that does not manage
	// a zone must not be blocked by a stale zone id in the file.
	c := mustLoadReal(t)
	c.DNS.Enabled = false
	c.DNS.ZoneID, c.DNS.Domain, c.DNS.APIBase = "", "", "nonsense"
	c.DNS.Retries, c.DNS.Timeout = -5, Duration(0)
	if err := c.Validate(); err != nil {
		t.Errorf("a broken dns block was rejected with dns.enabled false: %v", err)
	}
}

// TestValidateSeparatesTheTwoAttemptBudgets: closes and deploys retry for different
// reasons and at wildly different cost — an API call against an escrow plus a bid
// window plus a lease-ready wait. One knob for both would mean either abandoning a
// billing lease too early or churning deployments for hours, so there are two, and
// both have to be positive.
func TestValidateSeparatesTheTwoAttemptBudgets(t *testing.T) {
	for _, tc := range []struct {
		field string
		spoil func(*Config)
	}{
		{"akash.max_attempts", func(c *Config) { c.Akash.MaxAttempts = 0 }},
		{"akash.max_attempts", func(c *Config) { c.Akash.MaxAttempts = -1 }},
		{"akash.max_deploy_attempts", func(c *Config) { c.Akash.MaxDeployAttempts = 0 }},
		{"akash.max_deploy_attempts", func(c *Config) { c.Akash.MaxDeployAttempts = -3 }},
	} {
		c := mustLoadReal(t)
		tc.spoil(c)
		err := c.Validate()
		if err == nil {
			t.Errorf("%s: a non-positive budget was accepted", tc.field)
			continue
		}
		if !strings.Contains(err.Error(), tc.field) {
			t.Errorf("error does not name %s:\n%v", tc.field, err)
		}
	}

	// They are genuinely independent settings, not one aliased twice.
	c := mustLoadReal(t)
	if c.Akash.MaxDeployAttempts == c.Akash.MaxAttempts {
		t.Errorf("both budgets ship as %d; a deploy retry costs far more than a close retry",
			c.Akash.MaxAttempts)
	}
}

// TestValidateBoundsTheBidSettleWindow: bid_settle is a preference and bid_wait is
// the ceiling that bounds it. A window at or past the ceiling makes every deploy pay
// the whole of bid_wait — the loop spends it refusing to choose and then chooses
// anyway — so the two values would describe one behaviour between them, badly.
func TestValidateBoundsTheBidSettleWindow(t *testing.T) {
	for _, tc := range []struct {
		what  string
		spoil func(*Config)
	}{
		{"negative", func(c *Config) { c.Akash.Timeouts.BidSettle = Duration(-time.Second) }},
		{"equal to bid_wait", func(c *Config) { c.Akash.Timeouts.BidSettle = c.Akash.Timeouts.BidWait }},
		{"past bid_wait", func(c *Config) {
			c.Akash.Timeouts.BidSettle = Duration(time.Duration(c.Akash.Timeouts.BidWait) + time.Minute)
		}},
	} {
		c := mustLoadReal(t)
		tc.spoil(c)
		err := c.Validate()
		if err == nil {
			t.Errorf("%s: a bid_settle %s was accepted", tc.what, tc.what)
			continue
		}
		if !strings.Contains(err.Error(), "akash.timeouts.bid_settle") {
			t.Errorf("%s: error does not name akash.timeouts.bid_settle:\n%v", tc.what, err)
		}
	}

	// Zero is legal and means "take the first acceptable bid" — the behaviour before
	// the window existed, kept reachable for a market that has to be leased now.
	c := mustLoadReal(t)
	c.Akash.Timeouts.BidSettle = 0
	if err := c.Validate(); err != nil {
		t.Errorf("bid_settle: 0 was rejected: %v", err)
	}

	// And the shipped config has to actually shop around: this is the setting that
	// leased the live world $55/year above the cheapest eligible bid.
	c = mustLoadReal(t)
	if time.Duration(c.Akash.Timeouts.BidSettle) < 30*time.Second {
		t.Errorf("shipped bid_settle is %v; too short to see a market that answers over tens of seconds",
			time.Duration(c.Akash.Timeouts.BidSettle))
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
