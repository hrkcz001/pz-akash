package sdl

import (
	"fmt"
	"math"
)

// MaxUAKTPerBlock converts a USD/day ceiling into the Akash bid ceiling in
// uakt per block.
//
// This reproduces the bash implementation exactly, including its truncation:
//
//	print(max(0, int(usd * 1e6 / (rate * bpd))))
//
// so that a v2 render can be diffed against a v1 deploy without the price
// moving underneath the comparison.
func MaxUAKTPerBlock(maxUSDPerDay, aktUSD float64, blocksPerDay int) (int, error) {
	if maxUSDPerDay <= 0 {
		return 0, fmt.Errorf("max USD/day must be greater than 0, got %g", maxUSDPerDay)
	}
	if aktUSD <= 0 {
		return 0, fmt.Errorf("AKT/USD must be greater than 0, got %g", aktUSD)
	}
	if blocksPerDay <= 0 {
		return 0, fmt.Errorf("blocks per day must be greater than 0, got %d", blocksPerDay)
	}
	v := int(maxUSDPerDay * 1e6 / (aktUSD * float64(blocksPerDay)))
	if v <= 0 {
		return 0, fmt.Errorf("computed bid ceiling is 0 uakt/block (max_usd_per_day=%g is too low at AKT/USD=%g)",
			maxUSDPerDay, aktUSD)
	}
	return v, nil
}

// InitialDepositUSD is the escrow deposit made at deploy time: a small number
// of days at the maximum price, with a margin. The funds loop later tops the
// escrow up to the full horizon at the *actual* lease price, so this only has
// to be large enough to get the lease created.
//
// Mirrors the bash implementation: max(floor, round(usd * days * margin, 2)).
func InitialDepositUSD(maxUSDPerDay float64, days int, margin, floor float64) float64 {
	v := math.Round(maxUSDPerDay*float64(days)*margin*100) / 100
	return math.Max(floor, v)
}
