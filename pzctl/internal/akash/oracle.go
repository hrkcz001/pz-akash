package akash

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Oracle reads the AKT/USD rate.
//
// It is deliberately not a method on Client: the price feed is a different host
// with different failure modes and no API key, and mixing it into the Console
// client invites sending our key somewhere it does not belong.
//
// Nothing here is on the critical path while the wallet bids in a dollar-pegged
// denomination — see package denom. That is the point: a deploy that cannot start
// because CoinGecko is rate-limiting us is a bad trade, and v1 made it every
// time.
type Oracle struct {
	URL      string
	HTTP     *http.Client
	Timeout  time.Duration
	Fallback float64
	Logf     func(string, ...any)
}

// Rate returns the AKT/USD rate and where it came from, for the log and for the
// state document. An unreachable oracle with a configured fallback is a warning,
// not an error; with no fallback it is an error, because bidding against a rate
// of zero would reject every bid on the market.
func (o Oracle) Rate(ctx context.Context) (float64, string, error) {
	rate, err := o.fetch(ctx)
	if err == nil && rate > 0 {
		return rate, "oracle", nil
	}
	if err == nil {
		err = fmt.Errorf("oracle returned %g", rate)
	}
	if o.Fallback > 0 {
		if o.Logf != nil {
			o.Logf("akash: price oracle unavailable (%v); using the configured fallback of $%g/AKT", err, o.Fallback)
		}
		return o.Fallback, "akash.price.akt_usd_fallback", nil
	}
	return 0, "", fmt.Errorf("no AKT/USD rate: %w (set akash.price.akt_usd_fallback)", err)
}

// fetch reads the rate from the configured URL.
//
// The response is decoded as "some object containing some object containing a
// usd key", which is CoinGecko's shape without hardcoding CoinGecko's coin id.
// The id is part of a URL in the config file, and a config change that renames it
// should not need a code change to match.
func (o Oracle) fetch(ctx context.Context) (float64, error) {
	url := strings.TrimSpace(o.URL)
	if url == "" {
		return 0, fmt.Errorf("akash.price.price_oracle_url is empty")
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")

	hc := o.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	body := io.LimitReader(resp.Body, 1<<20)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return 0, fmt.Errorf("GET %s: HTTP %d: %s", url, resp.StatusCode, readSnippet(body))
	}
	var out map[string]map[string]Num
	if err := json.NewDecoder(body).Decode(&out); err != nil {
		return 0, fmt.Errorf("decoding the oracle response: %w", err)
	}
	for _, prices := range out {
		for cur, v := range prices {
			if strings.EqualFold(cur, "usd") && v.F() > 0 {
				return v.F(), nil
			}
		}
	}
	return 0, fmt.Errorf("no usd price in the oracle response")
}
