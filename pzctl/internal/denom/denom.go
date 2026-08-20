// Package denom converts Akash bid denominations into dollars.
//
// It exists as its own package because three layers need the same table and
// cannot share it any other way: config validates the configured denomination,
// sdl writes the bid ceiling into the SDL, and akash compares incoming bids
// against a dollar limit. A copy in each is how the units drift apart.
//
// The distinction the table encodes is not cosmetic. v1 converted every bid with
//
//	usd_day = amount / 1e6 * blocks_per_day * akt_usd
//
// which is right for uakt and wrong for uact — and every bid the live wallet has
// ever received is priced in uact. The error scales with the AKT price: near $1
// it merely overstates cost, but at AKT $5 the ceiling comes out five times too
// tight and every bid is discarded as too expensive. The symptom is not an
// overspend, it is "no bids found" and no server at all.
package denom

import (
	"fmt"
	"strings"
)

const (
	// UACT is the Akash Console credit: dollar-pegged, 1e6 to the dollar.
	// Console's wallet endpoint reports creditAmount in it alongside a
	// topUpMinAmountUsd, and an escrow deposit of $3.60 arrives as 3600000 uact.
	// Converting it needs no price oracle at all.
	UACT = "uact"

	// UAKT is micro-AKT, 1e6 to one AKT. Its dollar value is whatever the market
	// says, so it is the only denomination that needs the oracle.
	UAKT = "uakt"
)

// unitsPerWhole is the 1e6 in both denominations: uakt to AKT, uact to USD.
const unitsPerWhole = 1e6

// Known reports whether d is a denomination this build can price. The set is
// closed on purpose: an unrecognised denomination is refused rather than assumed
// dollar-pegged, because assuming wrong is how you accept a bid costing a
// thousand times what you meant to pay.
func Known(d string) bool {
	switch Normalize(d) {
	case UACT, UAKT:
		return true
	}
	return false
}

// Normalize lowercases and trims a denomination for comparison.
func Normalize(d string) string { return strings.ToLower(strings.TrimSpace(d)) }

// NeedsOracle reports whether pricing in d depends on the AKT/USD rate. A
// deployment bidding in uact can price itself with the oracle unreachable, which
// takes CoinGecko off the critical path of starting a server.
func NeedsOracle(d string) bool { return Normalize(d) == UAKT }

// USDPerUnit is the dollar value of one unit of d. aktUSD is consulted only when
// NeedsOracle(d) is true, and must be positive in that case.
func USDPerUnit(d string, aktUSD float64) (float64, error) {
	switch Normalize(d) {
	case UACT:
		return 1 / unitsPerWhole, nil
	case UAKT:
		if aktUSD <= 0 {
			return 0, fmt.Errorf("AKT/USD must be greater than 0 to price %s, got %g", UAKT, aktUSD)
		}
		return aktUSD / unitsPerWhole, nil
	case "":
		return 0, fmt.Errorf("no denomination given")
	default:
		return 0, fmt.Errorf("unknown denomination %q", d)
	}
}

// USDPerDay converts a per-block amount into dollars per day.
func USDPerDay(amountPerBlock float64, d string, blocksPerDay int, aktUSD float64) (float64, error) {
	if blocksPerDay <= 0 {
		return 0, fmt.Errorf("blocks per day must be greater than 0, got %d", blocksPerDay)
	}
	if amountPerBlock < 0 {
		return 0, fmt.Errorf("bid amount must not be negative, got %g", amountPerBlock)
	}
	rate, err := USDPerUnit(d, aktUSD)
	if err != nil {
		return 0, err
	}
	return amountPerBlock * rate * float64(blocksPerDay), nil
}

// CeilingPerBlock converts a USD/day limit into the integer per-block bid ceiling
// that goes into the SDL, in d.
//
// It truncates rather than rounds, so the ceiling is never above the stated
// dollar limit: the number in the SDL is a promise about spending.
func CeilingPerBlock(maxUSDPerDay float64, d string, blocksPerDay int, aktUSD float64) (int, error) {
	if maxUSDPerDay <= 0 {
		return 0, fmt.Errorf("max USD/day must be greater than 0, got %g", maxUSDPerDay)
	}
	if blocksPerDay <= 0 {
		return 0, fmt.Errorf("blocks per day must be greater than 0, got %d", blocksPerDay)
	}
	rate, err := USDPerUnit(d, aktUSD)
	if err != nil {
		return 0, err
	}
	v := int(maxUSDPerDay / (rate * float64(blocksPerDay)))
	if v <= 0 {
		return 0, fmt.Errorf("computed bid ceiling is 0 %s/block (max_usd_per_day=%g is too low)", d, maxUSDPerDay)
	}
	return v, nil
}
