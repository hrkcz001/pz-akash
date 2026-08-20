package sdl

import "math"

// InitialDepositUSD is the escrow deposit made at deploy time: a small number of
// days at the maximum price, with a margin. The funds loop later tops the escrow
// up to the full horizon at the *actual* lease price, so this only has to be
// large enough to get the lease created.
//
// Mirrors the bash implementation: max(floor, round(usd * days * margin, 2)).
//
// The deposit is denominated in dollars by the Console API whatever the bid denom
// is — $3.60 requested, 3600000 uact escrowed. Bid ceilings, which are not in
// dollars, are in package denom.
func InitialDepositUSD(maxUSDPerDay float64, days int, margin, floor float64) float64 {
	v := math.Round(maxUSDPerDay*float64(days)*margin*100) / 100
	return math.Max(floor, v)
}
