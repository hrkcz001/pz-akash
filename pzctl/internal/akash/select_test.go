package akash

import (
	"math"
	"strings"
	"testing"
)

func TestParseCPUAndSize(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"1", 1000},
		{"8", 8000},
		{"0.5", 500},
		{"500m", 500},
		{" 250m ", 250},
	} {
		got, err := ParseCPU(tc.in)
		if err != nil {
			t.Errorf("ParseCPU(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseCPU(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"512Mi", 512 << 20},
		{"2Gi", 2 << 30},
		{"16Gi", 16 << 30},
		{"1Ti", 1 << 40},
		{"1024", 1024},
	} {
		got, err := ParseSize(tc.in)
		if err != nil {
			t.Errorf("ParseSize(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
	// Akash wants binary units. "16GB" is a config typo, and silently reading it
	// as 16 bytes would reject every provider on the network.
	for _, bad := range []string{"16GB", "lots", "-1", "1.2.3"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) returned no error", bad)
		}
	}
	if _, err := ParseCPU("many"); err == nil {
		t.Error("ParseCPU(\"many\") returned no error")
	}
}

// good is a provider that passes every filter, so each subtest can break exactly
// one thing and know that is what it is testing.
func good(owner string) Provider {
	p := Provider{
		Owner:          owner,
		IsOnline:       true,
		IsValidVersion: true,
		FeatEndpointIP: true,
		Uptime30d:      0.99,
		IPCountryCode:  "de",
		IPLat:          50.1109,
		IPLon:          8.6821,
	}
	p.Stats.CPU.Available = 16000
	p.Stats.Memory.Available = 64 << 30
	p.Stats.Storage.Ephemeral.Available = 500 << 30
	return p
}

func serverCriteria() Criteria {
	return Criteria{
		Countries:     []string{"DE", "PL"},
		MinUptime30d:  0.95,
		RequireIP:     true,
		CPUMillicores: 8000,
		MemoryBytes:   16 << 30,
		StorageBytes:  30 << 30,
		RefLat:        52.2297,
		RefLon:        21.0122,
		AllowedDenoms: []string{"uact", "uakt"},
		MinUSDPerDay:  0.001,
		MaxUSDPerDay:  3.0,
		Tolerance:     0.20,
		BlocksPerDay:  14400,
	}
}

func TestSelectProvidersNamesEveryReason(t *testing.T) {
	cr := serverCriteria()

	offline := good("akash1offline")
	offline.IsOnline = false
	oldVersion := good("akash1old")
	oldVersion.IsValidVersion = false
	noIP := good("akash1noip")
	noIP.FeatEndpointIP = false
	flaky := good("akash1flaky")
	flaky.Uptime30d = 0.80
	elsewhere := good("akash1fr")
	elsewhere.IPCountryCode = "FR"
	nowhere := good("akash1nowhere")
	nowhere.IPCountryCode, nowhere.Country = "", ""
	smallCPU := good("akash1cpu")
	smallCPU.Stats.CPU.Available = 2000
	smallMem := good("akash1mem")
	smallMem.Stats.Memory.Available = 8 << 30
	smallDisk := good("akash1disk")
	smallDisk.Stats.Storage.Ephemeral.Available = 10 << 30
	denied := good("akash1denied")
	skipped := good("akash1skipped")
	anon := good("")

	cr.Deny = []string{"AKASH1DENIED"} // case must not matter

	all := []Provider{
		good("akash1keep"), offline, oldVersion, noIP, flaky, elsewhere, nowhere,
		smallCPU, smallMem, smallDisk, denied, skipped, anon,
	}
	ok, bad := SelectProviders(cr, all, func(owner string) bool { return owner == "akash1skipped" })

	if len(ok) != 1 || ok[0].Owner != "akash1keep" {
		t.Fatalf("eligible = %v, want just akash1keep", owners(ok))
	}
	if len(bad) != len(all)-1 {
		t.Fatalf("got %d rejections for %d bad providers", len(bad), len(all)-1)
	}
	// Each reason must say something specific; a filter that rejects without
	// explaining is how "no bids found" became unactionable in v1.
	want := map[string]string{
		"akash1offline": "offline",
		"akash1old":     "version",
		"akash1noip":    "dedicated IP",
		"akash1flaky":   "uptime",
		"akash1fr":      "FR",
		"akash1nowhere": "no country",
		"akash1cpu":     "CPU",
		"akash1mem":     "memory",
		"akash1disk":    "storage",
		"akash1denied":  "deny_providers",
		"akash1skipped": "skip list",
	}
	byOwner := map[string]string{}
	for _, r := range bad {
		byOwner[r.Owner] = r.Why
	}
	for owner, fragment := range want {
		if got := byOwner[owner]; !strings.Contains(got, fragment) {
			t.Errorf("%s rejected as %q, want a reason mentioning %q", owner, got, fragment)
		}
	}
}

// A provider that reports no capacity at all is not thereby out of capacity. The
// alternative reads a missing field as zero and rejects the whole market.
func TestUnreportedCapacityIsNotZeroCapacity(t *testing.T) {
	cr := serverCriteria()
	silent := good("akash1silent")
	silent.Stats.CPU.Available = 0
	silent.Stats.Memory.Available = 0
	silent.Stats.Storage.Ephemeral.Available = 0

	ok, bad := SelectProviders(cr, []Provider{silent}, nil)
	if len(ok) != 1 {
		t.Errorf("a provider with no stats was rejected: %v", bad)
	}
}

func TestSelectProvidersWithNoFiltersAcceptsAnyLiveProvider(t *testing.T) {
	// The controller's placement: no country list, no capacity numbers, no IP.
	cr := Criteria{BlocksPerDay: 14400, MaxUSDPerDay: 3}
	p := good("akash1anywhere")
	p.IPCountryCode = "SG"
	p.FeatEndpointIP = false
	ok, bad := SelectProviders(cr, []Provider{p}, nil)
	if len(ok) != 1 {
		t.Errorf("empty criteria rejected a healthy provider: %v", bad)
	}
}

func owners(ps []Provider) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Owner)
	}
	return out
}

// --- bid selection ---

func bid(owner string, amount float64, d string) Bid {
	var b Bid
	b.ID.Provider = owner
	b.ID.GSeq, b.ID.OSeq = 1, 1
	b.Price.Denom = d
	b.Price.Amount = Num(amount)
	b.State = "open"
	return b
}

// The regression that matters most: the live server lease is 34 uact/block, and
// it must be affordable with no AKT rate available at all. Under v1's arithmetic
// this same bid was priced as micro-AKT and rejected outright once AKT passed a
// couple of dollars.
func TestUACTBidIsAffordableWithoutAnOracle(t *testing.T) {
	cr := serverCriteria() // AKTUSD deliberately 0
	p := good("akash1live")
	choice, bad, err := SelectBid(cr, []Bid{bid(p.Owner, 34, "uact")}, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	if choice == nil {
		t.Fatalf("the live price was rejected: %s", Reasons(bad))
	}
	if want := 0.4896; math.Abs(choice.USDPerDay-want) > 1e-9 {
		t.Errorf("USD/day = %v, want %v", choice.USDPerDay, want)
	}
	if choice.AmountPerBlock != 34 || choice.Denom != "uact" {
		t.Errorf("price recorded as %d %s, want 34 uact", choice.AmountPerBlock, choice.Denom)
	}
	if math.Abs(choice.USDPerHour-choice.USDPerDay/24) > 1e-12 {
		t.Errorf("USD/hour %v is not USD/day %v over 24", choice.USDPerHour, choice.USDPerDay)
	}
}

// Within the tolerance band, geography wins. That is the whole point of the band:
// a few cents a day is invisible and 1500 km of latency is not.
func TestWithinToleranceTheClosestProviderWins(t *testing.T) {
	cr := serverCriteria()

	warsaw := good("akash1warsaw")
	warsaw.IPCountryCode, warsaw.IPLat, warsaw.IPLon = "PL", 52.2297, 21.0122
	frankfurt := good("akash1frankfurt")

	// Frankfurt is cheaper, Warsaw is 5% dearer and sits on the reference point.
	bids := []Bid{bid(frankfurt.Owner, 100, "uact"), bid(warsaw.Owner, 105, "uact")}
	choice, bad, err := SelectBid(cr, bids, []Provider{frankfurt, warsaw})
	if err != nil {
		t.Fatal(err)
	}
	if choice == nil {
		t.Fatalf("no choice: %s", Reasons(bad))
	}
	if choice.Provider.Owner != warsaw.Owner {
		t.Errorf("chose %s at %.4f USD/day; Warsaw was within tolerance and closer",
			choice.Provider.Owner, choice.USDPerDay)
	}

	// Outside the band, price wins again: 130 is 30% over 100 and tolerance is 20%.
	bids = []Bid{bid(frankfurt.Owner, 100, "uact"), bid(warsaw.Owner, 130, "uact")}
	choice, _, err = SelectBid(cr, bids, []Provider{frankfurt, warsaw})
	if err != nil {
		t.Fatal(err)
	}
	if choice == nil || choice.Provider.Owner != frankfurt.Owner {
		t.Errorf("a bid 30%% over the cheapest won with tolerance at 20%%: %+v", choice)
	}
}

func TestSelectBidExplainsEveryDroppedBid(t *testing.T) {
	cr := serverCriteria()
	keep := good("akash1keep")
	filtered := good("akash1filtered") // eligible list omits it
	other := good("akash1other")

	bids := []Bid{
		bid(keep.Owner, 34, "uact"),
		bid(filtered.Owner, 34, "uact"),
		bid(other.Owner, 500, "uact"),   // $7.20/day, over the $3 limit
		bid(other.Owner, 0, "uact"),     // free, therefore not real
		bid(other.Owner, 34, "uosmo"),   // a currency we do not read
		bid(other.Owner, 34, "uakt"),    // needs the oracle, which is absent
		closed(other.Owner, 34, "uact"), // already leased by someone else
	}
	choice, bad, err := SelectBid(cr, bids, []Provider{keep, other})
	if err != nil {
		t.Fatal(err)
	}
	if choice == nil || choice.Provider.Owner != keep.Owner {
		t.Fatalf("choice = %+v, want the one affordable bid", choice)
	}
	if len(bad) != 6 {
		t.Fatalf("got %d rejections, want 6: %s", len(bad), Reasons(bad))
	}
	joined := Reasons(bad)
	for _, fragment := range []string{
		"placement filter", // filtered
		"above the",        // over the limit
		"below the",        // free
		"allowed_denoms",   // uosmo
		"cannot price",     // uakt with no rate
		"not open",         // closed
	} {
		if !strings.Contains(joined, fragment) {
			t.Errorf("no rejection mentioning %q in: %s", fragment, joined)
		}
	}
}

// A uakt bid with the oracle up prices normally; the denomination is supported,
// it just costs a network call the dollar-pegged one does not.
func TestUAKTBidPricesWithTheOracleRate(t *testing.T) {
	cr := serverCriteria()
	cr.AKTUSD = 1.5
	p := good("akash1uakt")
	choice, bad, err := SelectBid(cr, []Bid{bid(p.Owner, 34, "uakt")}, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	if choice == nil {
		t.Fatalf("a priceable uakt bid was rejected: %s", Reasons(bad))
	}
	if want := 34 * 1.5e-6 * 14400; math.Abs(choice.USDPerDay-want) > 1e-9 {
		t.Errorf("USD/day = %v, want %v", choice.USDPerDay, want)
	}
}

func TestSelectBidRefusesNonsenseCriteria(t *testing.T) {
	p := good("akash1x")
	bids := []Bid{bid(p.Owner, 34, "uact")}

	cr := serverCriteria()
	cr.BlocksPerDay = 0
	if _, _, err := SelectBid(cr, bids, []Provider{p}); err == nil {
		t.Error("blocks_per_day 0 returned no error")
	}
	cr = serverCriteria()
	cr.MaxUSDPerDay = 0
	if _, _, err := SelectBid(cr, bids, []Provider{p}); err == nil {
		t.Error("max_usd_per_day 0 returned no error: every bid would be too expensive")
	}
}

func TestNoBidsIsNotAnError(t *testing.T) {
	choice, bad, err := SelectBid(serverCriteria(), nil, []Provider{good("akash1x")})
	if err != nil {
		t.Fatalf("an empty bid list is a normal early poll, not an error: %v", err)
	}
	if choice != nil || len(bad) != 0 {
		t.Errorf("choice = %+v, rejections = %v", choice, bad)
	}
	if Reasons(nil) != "none" {
		t.Errorf("Reasons(nil) = %q", Reasons(nil))
	}
}

func TestDistanceKM(t *testing.T) {
	// Warsaw to Frankfurt is about 900 km by great circle.
	got := DistanceKM(52.2297, 21.0122, 50.1109, 8.6821)
	if got < 850 || got > 950 {
		t.Errorf("Warsaw->Frankfurt = %.0f km, want ~900", got)
	}
	if d := DistanceKM(52.2297, 21.0122, 52.2297, 21.0122); d > 1e-9 {
		t.Errorf("a point to itself = %v", d)
	}
	// (0,0) is what a provider reports when it reports nothing, and an unknown
	// distance must never look like the closest one.
	for _, tc := range [][4]float64{
		{52.2297, 21.0122, 0, 0},
		{0, 0, 50.1109, 8.6821},
		{52.2297, 21.0122, 91, 8},
		{52.2297, 21.0122, math.NaN(), 8},
	} {
		if d := DistanceKM(tc[0], tc[1], tc[2], tc[3]); !math.IsInf(d, 1) {
			t.Errorf("DistanceKM%v = %v, want +Inf", tc, d)
		}
	}
}

// A provider with no coordinates still wins on price alone, because refusing to
// deploy over a missing latitude is worse than deploying far away.
func TestProviderWithoutCoordinatesCanStillWin(t *testing.T) {
	cr := serverCriteria()
	blind := good("akash1blind")
	blind.IPLat, blind.IPLon = 0, 0
	choice, bad, err := SelectBid(cr, []Bid{bid(blind.Owner, 34, "uact")}, []Provider{blind})
	if err != nil {
		t.Fatal(err)
	}
	if choice == nil {
		t.Fatalf("rejected for having no coordinates: %s", Reasons(bad))
	}
	if !math.IsInf(choice.DistanceKM, 1) {
		t.Errorf("distance = %v, want +Inf", choice.DistanceKM)
	}
	if !strings.Contains(choice.String(), "unknown distance") {
		t.Errorf("String() hides the missing measurement: %s", choice)
	}
}

func closed(owner string, amount float64, d string) Bid {
	b := bid(owner, amount, d)
	b.State = "closed"
	return b
}
