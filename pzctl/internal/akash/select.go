package akash

// Provider and bid selection, kept pure: no HTTP, no clock, no config lookups
// past the Criteria struct. Every decision in here was a one-line jq filter in
// v1's deploy.sh, spread across three pipelines, and when the result was empty
// the log said only "no bids found" — which is the same message for "every
// provider is in Poland but you asked for Germany", "the oracle was down so the
// ceiling was nonsense", and "there genuinely were no bids". So each rejection
// here carries the reason, and the caller logs them.

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/denom"
)

// bidStateOpen is the only bid state worth looking at. A bid moves to active
// once someone leases it and closed when the deployment ends; taking a lease on
// either is an error the API reports late, after we have already stopped
// looking for alternatives.
const bidStateOpen = "open"

// Criteria is everything selection needs. It is a flat value rather than a
// *config.Config so that a test can state one condition without building a
// whole configuration, and so that the AKT rate — which comes from the network,
// not the file — arrives by the same door as everything else.
type Criteria struct {
	// Countries is an allow-list of ISO 3166-1 alpha-2 codes. Empty means any.
	Countries []string
	// Deny lists provider owner addresses never to use, whatever they bid.
	Deny []string
	// MinUptime30d is a fraction in [0, 1]; 0 disables the check.
	MinUptime30d float64
	// RequireIP demands featEndpointIp. The game server needs a dedicated IP:
	// players connect to an address in a DNS record, and a shared endpoint hands
	// out an arbitrary port on a shared host.
	RequireIP bool

	// Capacity the deployment needs. CPU is in millicores, the rest in bytes.
	// Zero means "do not check this dimension".
	CPUMillicores int64
	MemoryBytes   int64
	StorageBytes  int64

	// RefLat and RefLon are the point distance is measured from — the players,
	// not the controller.
	RefLat, RefLon float64

	// AllowedDenoms are the bid denominations we can price. A bid in anything
	// else is skipped, never guessed at.
	AllowedDenoms []string
	// MinUSDPerDay guards against a bid too good to be true: a provider pricing
	// at zero is one that has misconfigured itself and will drop the lease.
	MinUSDPerDay float64
	MaxUSDPerDay float64
	// Tolerance widens the winning band: any bid within this fraction of the
	// cheapest is eligible, and the closest of those wins.
	Tolerance    float64
	BlocksPerDay int
	// AKTUSD is consulted only for denominations that need it.
	AKTUSD float64
}

// CriteriaFor lifts the criteria out of config for one deployment role.
//
// res is the compute profile being deployed, because capacity filtering has to
// compare against what this deployment actually asks for; the controller and the
// server ask for very different machines.
func CriteriaFor(c *config.Config, res config.Resources, requireIP bool, aktUSD float64) (Criteria, error) {
	cr := Criteria{
		Countries:     c.Akash.Placement.Countries,
		Deny:          c.Akash.Placement.DenyProviders,
		MinUptime30d:  c.Akash.Placement.MinUptime30d,
		RequireIP:     requireIP,
		RefLat:        c.Akash.Placement.RefLat,
		RefLon:        c.Akash.Placement.RefLon,
		AllowedDenoms: c.Akash.Price.AllowedDenoms,
		MinUSDPerDay:  c.Akash.Price.MinUSDPerDay,
		MaxUSDPerDay:  c.Akash.Price.MaxUSDPerDay,
		Tolerance:     c.Akash.Price.Tolerance,
		BlocksPerDay:  c.Akash.BlocksPerDay,
		AKTUSD:        aktUSD,
	}
	var err error
	if cr.CPUMillicores, err = ParseCPU(res.CPU); err != nil {
		return Criteria{}, fmt.Errorf("cpu: %w", err)
	}
	if cr.MemoryBytes, err = ParseSize(res.Memory); err != nil {
		return Criteria{}, fmt.Errorf("memory: %w", err)
	}
	if cr.StorageBytes, err = ParseSize(res.Storage); err != nil {
		return Criteria{}, fmt.Errorf("storage: %w", err)
	}
	return cr, nil
}

// ParseCPU reads an Akash CPU quantity as millicores: "8" is 8000, "500m" is
// 500. Kubernetes units, because that is what the provider reports back.
func ParseCPU(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if rest, ok := strings.CutSuffix(s, "m"); ok {
		n, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%q is not a CPU quantity", s)
		}
		return int64(math.Round(n)), nil
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%q is not a CPU quantity", s)
	}
	return int64(math.Round(n * 1000)), nil
}

// ParseSize reads an Akash memory or storage size in binary units: 16Gi, 512Mi.
// A bare number is bytes, which is what the SDL allows and what the provider
// stats endpoint returns.
func ParseSize(s string) (int64, error) {
	orig := strings.TrimSpace(s)
	s = orig
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	for suffix, m := range map[string]int64{
		"Ki": 1 << 10,
		"Mi": 1 << 20,
		"Gi": 1 << 30,
		"Ti": 1 << 40,
	} {
		if rest, ok := strings.CutSuffix(s, suffix); ok {
			s, mult = strings.TrimSpace(rest), m
			break
		}
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%q is not an Akash size", orig)
	}
	return int64(n * float64(mult)), nil
}

// --- provider filter ---

// Rejection is one candidate we declined, and why. These are the log lines that
// turn "no bids found" into something an operator can act on.
type Rejection struct {
	Owner string
	Why   string
}

func (r Rejection) String() string { return r.Owner + ": " + r.Why }

// Reasons renders rejections compactly for a log line, most common first.
func Reasons(rs []Rejection) string {
	if len(rs) == 0 {
		return "none"
	}
	count := map[string]int{}
	for _, r := range rs {
		count[r.Why]++
	}
	keys := make([]string, 0, len(count))
	for k := range count {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if count[keys[i]] != count[keys[j]] {
			return count[keys[i]] > count[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s ×%d", k, count[k]))
	}
	return strings.Join(parts, ", ")
}

// SelectProviders returns the providers we are willing to lease from, and a
// rejection for every one we are not.
//
// skipped reports whether an owner is on the TTL skip list — providers that
// failed us recently. It is a function rather than a slice because the list is
// time-dependent and lives in the controller's state document; nil means nothing
// is skipped.
//
// The capacity checks only apply to a dimension the provider actually reports.
// A provider whose stats are absent is not thereby infinite, but treating a
// missing number as zero rejects the entire market the day Console changes that
// part of the response — and an over-committed provider simply does not bid,
// which is a failure we recover from in seconds.
func SelectProviders(cr Criteria, all []Provider, skipped func(owner string) bool) ([]Provider, []Rejection) {
	countries := upperSet(cr.Countries)
	deny := lowerSet(cr.Deny)

	ok := make([]Provider, 0, len(all))
	var bad []Rejection
	reject := func(p Provider, why string, args ...any) {
		bad = append(bad, Rejection{Owner: p.Owner, Why: fmt.Sprintf(why, args...)})
	}

	for _, p := range all {
		switch {
		case p.Owner == "":
			reject(p, "no owner address in the API response")
		case deny[strings.ToLower(p.Owner)]:
			reject(p, "on akash.placement.deny_providers")
		case skipped != nil && skipped(p.Owner):
			reject(p, "on the skip list after a recent failure")
		case !p.IsOnline:
			reject(p, "offline")
		case !p.IsValidVersion:
			reject(p, "running an unsupported provider version")
		case cr.RequireIP && !p.FeatEndpointIP:
			reject(p, "cannot lease a dedicated IP")
		case cr.MinUptime30d > 0 && p.Uptime30d.F() < cr.MinUptime30d:
			reject(p, "30-day uptime %.1f%% below the %.1f%% minimum",
				p.Uptime30d.F()*100, cr.MinUptime30d*100)
		case len(countries) > 0 && p.CountryCode() == "":
			reject(p, "reports no country and a country allow-list is set")
		case len(countries) > 0 && !countries[p.CountryCode()]:
			reject(p, "in %s, which is not in akash.placement.countries", p.CountryCode())
		case short(cr.CPUMillicores, int64(p.Stats.CPU.Available.F())):
			reject(p, "%dm CPU available, %dm needed",
				int64(p.Stats.CPU.Available.F()), cr.CPUMillicores)
		case short(cr.MemoryBytes, int64(p.Stats.Memory.Available.F())):
			reject(p, "%s memory available, %s needed",
				humanBytes(int64(p.Stats.Memory.Available.F())), humanBytes(cr.MemoryBytes))
		case short(cr.StorageBytes, int64(p.Stats.Storage.Ephemeral.Available.F())):
			reject(p, "%s storage available, %s needed",
				humanBytes(int64(p.Stats.Storage.Ephemeral.Available.F())), humanBytes(cr.StorageBytes))
		default:
			ok = append(ok, p)
		}
	}
	return ok, bad
}

// short reports whether a reported availability falls below what we need. A
// zero report is "not reported" rather than "nothing left"; see SelectProviders.
func short(need, have int64) bool { return need > 0 && have > 0 && have < need }

// CountryCode is the provider's location as an upper-case alpha-2 code. The
// API's own ipCountryCode is preferred; country is a display name ("Germany")
// and only usable when it happens to already be a code.
func (p Provider) CountryCode() string {
	if c := strings.ToUpper(strings.TrimSpace(p.IPCountryCode)); len(c) == 2 {
		return c
	}
	if c := strings.ToUpper(strings.TrimSpace(p.Country)); len(c) == 2 {
		return c
	}
	return ""
}

func upperSet(in []string) map[string]bool {
	out := map[string]bool{}
	for _, s := range in {
		if s = strings.ToUpper(strings.TrimSpace(s)); s != "" {
			out[s] = true
		}
	}
	return out
}

func lowerSet(in []string) map[string]bool {
	out := map[string]bool{}
	for _, s := range in {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out[s] = true
		}
	}
	return out
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGi", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0fMi", float64(n)/(1<<20))
	default:
		return strconv.FormatInt(n, 10) + "B"
	}
}

// --- bid selection ---

// Choice is the bid we intend to lease, with the numbers that justified it. The
// price is recorded in the state document, so it has to be carried out of here
// rather than recomputed later against a rate that has since moved.
type Choice struct {
	Bid      Bid
	Provider Provider

	AmountPerBlock int
	Denom          string
	USDPerDay      float64
	USDPerHour     float64
	// DistanceKM from the reference point, or +Inf if the provider reports no
	// usable coordinates.
	DistanceKM float64
}

func (c Choice) String() string {
	dist := "unknown distance"
	if !math.IsInf(c.DistanceKM, 1) {
		dist = fmt.Sprintf("%.0f km", c.DistanceKM)
	}
	return fmt.Sprintf("%s in %s: %d %s/block = %.4f USD/day, %s",
		c.Provider.Owner, orUnknown(c.Provider.CountryCode()), c.AmountPerBlock, c.Denom, c.USDPerDay, dist)
}

// SelectBid picks the bid to lease from the open bids on a deployment.
//
// The rule, unchanged from v1 because it is a good rule: of the bids we can
// afford, take the band within Tolerance of the cheapest, and from that band the
// provider closest to the players. Latency is what a player feels; a few cents a
// day is what nobody feels.
//
// eligible is the output of SelectProviders. A bid from a provider not in that
// list is rejected here rather than filtered earlier, so the reason survives into
// the log: "the only provider that bid is one we skip-listed" is a diagnosis, and
// an empty list is not.
func SelectBid(cr Criteria, bids []Bid, eligible []Provider) (*Choice, []Rejection, error) {
	if cr.BlocksPerDay <= 0 {
		return nil, nil, fmt.Errorf("blocks per day must be greater than 0, got %d", cr.BlocksPerDay)
	}
	if cr.MaxUSDPerDay <= 0 {
		return nil, nil, fmt.Errorf("max USD/day must be greater than 0, got %g", cr.MaxUSDPerDay)
	}
	byOwner := make(map[string]Provider, len(eligible))
	for _, p := range eligible {
		byOwner[p.Owner] = p
	}
	allowed := lowerSet(cr.AllowedDenoms)

	var (
		cands []Choice
		bad   []Rejection
	)
	for _, b := range bids {
		owner := b.ID.Provider
		add := func(why string, args ...any) {
			bad = append(bad, Rejection{Owner: owner, Why: fmt.Sprintf(why, args...)})
		}
		if b.State != "" && !strings.EqualFold(b.State, bidStateOpen) {
			add("bid is %s, not open", b.State)
			continue
		}
		p, ok := byOwner[owner]
		if !ok {
			add("bid from a provider that failed the placement filter")
			continue
		}
		d := denom.Normalize(b.Price.Denom)
		if len(allowed) > 0 && !allowed[d] {
			// Not an error: it is a price in a currency we decided not to read.
			add("priced in %s, not in akash.price.allowed_denoms", orUnknown(b.Price.Denom))
			continue
		}
		usdDay, err := denom.USDPerDay(b.Price.Amount.F(), d, cr.BlocksPerDay, cr.AKTUSD)
		if err != nil {
			// The oracle being unreachable lands here for uakt bids. It must stay
			// visible: silently dropping them looks identical to nobody bidding.
			add("cannot price %g %s: %v", b.Price.Amount.F(), orUnknown(b.Price.Denom), err)
			continue
		}
		switch {
		case usdDay > cr.MaxUSDPerDay:
			add("%.4f USD/day is above the %.2f limit", usdDay, cr.MaxUSDPerDay)
		case cr.MinUSDPerDay > 0 && usdDay < cr.MinUSDPerDay:
			// A provider bidding implausibly low has misconfigured itself, and a
			// lease it cannot honour costs a redeploy and a rolled-back world.
			add("%.6f USD/day is below the %g floor", usdDay, cr.MinUSDPerDay)
		default:
			cands = append(cands, Choice{
				Bid:            b,
				Provider:       p,
				AmountPerBlock: int(math.Round(b.Price.Amount.F())),
				Denom:          d,
				USDPerDay:      usdDay,
				USDPerHour:     usdDay / 24,
				DistanceKM:     DistanceKM(cr.RefLat, cr.RefLon, p.IPLat.F(), p.IPLon.F()),
			})
		}
	}
	if len(cands) == 0 {
		return nil, bad, nil
	}

	// Cheapest first, then closest, then owner: the last key only breaks ties, but
	// without it two identical providers can swap places between polls and the
	// choice stops being reproducible from the log.
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.USDPerDay != b.USDPerDay {
			return a.USDPerDay < b.USDPerDay
		}
		if a.DistanceKM != b.DistanceKM {
			return a.DistanceKM < b.DistanceKM
		}
		return a.Provider.Owner < b.Provider.Owner
	})

	band := cands[0].USDPerDay * (1 + math.Max(0, cr.Tolerance))
	best := cands[0]
	for _, c := range cands[1:] {
		if c.USDPerDay > band {
			break
		}
		if c.DistanceKM < best.DistanceKM {
			best = c
		}
	}
	return &best, bad, nil
}

// DistanceKM is the great-circle distance between two points.
//
// A provider reporting (0, 0) is reporting nothing — the coordinate is in the
// Gulf of Guinea and no datacentre is there — so it returns +Inf. That keeps such
// a provider eligible on price while never winning a tie on distance, which is
// the honest handling of a missing measurement.
func DistanceKM(lat1, lon1, lat2, lon2 float64) float64 {
	if !usableCoord(lat1, lon1) || !usableCoord(lat2, lon2) {
		return math.Inf(1)
	}
	const earthRadiusKM = 6371.0
	rad := math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLon := (lon2 - lon1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadiusKM * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

func usableCoord(lat, lon float64) bool {
	if math.IsNaN(lat) || math.IsNaN(lon) || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return false
	}
	return lat != 0 || lon != 0
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}
