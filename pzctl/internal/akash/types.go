package akash

// The wire types. Each is the narrow subset of a Console API response that this
// package reads — never the whole document. Two things in here exist because the
// v1 bash client hit both of them in production:
//
//   - Num accepts a number or a quoted number. Bid prices arrive as decimal
//     strings ("32.000000000000000000") while provider stats arrive as numbers,
//     and provider coordinates have been seen both ways. jq papered over this;
//     Go's decoder will not, and a type error here reads as "no bids".
//   - providerList accepts a bare array or {"data": [...]}. The v1 jq expression
//     `if type == "array" then .[] else .data[] end` is the fossil record of the
//     endpoint having done both.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Num is a JSON number that may be written as a string.
type Num float64

func (n *Num) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*n = 0
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			*n = 0
			return nil
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return fmt.Errorf("expected a number, got %q", s)
		}
		*n = Num(f)
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	*n = Num(f)
	return nil
}

// F returns the value as a float64.
func (n Num) F() float64 { return float64(n) }

// Provider is one entry from GET /v1/providers.
type Provider struct {
	Owner          string `json:"owner"`
	IsOnline       bool   `json:"isOnline"`
	IsValidVersion bool   `json:"isValidVersion"`
	// FeatEndpointIP is whether the provider can lease a dedicated IP. The PZ
	// server needs one: players connect to a fixed address, and a shared
	// endpoint gives a random port on a shared host.
	FeatEndpointIP bool          `json:"featEndpointIp"`
	Uptime30d      Num           `json:"uptime30d"`
	Stats          ProviderStats `json:"stats"`
	IPCountryCode  string        `json:"ipCountryCode"`
	Country        string        `json:"country"`
	// IPCity and IPRegion are what the dashboard shows players. Both are optional
	// in the API's answer and often absent, so nothing may depend on them — see
	// Provider.Where for the fallback order.
	IPCity   string `json:"ipCity"`
	IPRegion string `json:"ipRegion"`
	IPLat    Num    `json:"ipLat"`
	IPLon    Num    `json:"ipLon"`
	HostURI  string `json:"hostUri"`
}

// Where is a human-readable location for the dashboard, or "" when the provider
// publishes nothing usable.
//
// Widest-first fallback, because a player wants the nearest true statement: a city
// with its country beats a country alone, which beats a bare code. Nothing is
// invented — a provider that reports no geography gets an empty string and the
// badge is omitted rather than filled with a guess from the hostname.
func (p Provider) Where() string {
	code := strings.ToUpper(strings.TrimSpace(p.IPCountryCode))
	country := strings.TrimSpace(p.Country)
	city := strings.TrimSpace(p.IPCity)
	if city == "" {
		city = strings.TrimSpace(p.IPRegion)
	}
	switch {
	case city != "" && code != "":
		return city + ", " + code
	case city != "":
		return city
	case country != "":
		return country
	default:
		return code
	}
}

// ProviderStats is the capacity the provider reports as available. CPU is in
// millicores; memory and storage in bytes.
type ProviderStats struct {
	CPU struct {
		Available Num `json:"available"`
	} `json:"cpu"`
	Memory struct {
		Available Num `json:"available"`
	} `json:"memory"`
	Storage struct {
		Ephemeral struct {
			Available Num `json:"available"`
		} `json:"ephemeral"`
	} `json:"storage"`
}

// providerList decodes either shape the providers endpoint returns.
type providerList []Provider

func (l *providerList) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var arr []Provider
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return err
		}
		*l = arr
		return nil
	}
	var wrapped struct {
		Data []Provider `json:"data"`
	}
	if err := json.Unmarshal(trimmed, &wrapped); err != nil {
		return err
	}
	*l = wrapped.Data
	return nil
}

// createDeploymentResponse is POST /v1/deployments. The manifest comes back with
// the deployment and must be handed to POST /v1/leases verbatim; it is the only
// piece of deploy state that exists solely in memory between the two calls, which
// is why losing it means an orphaned deployment paying for nothing.
type createDeploymentResponse struct {
	Data struct {
		DSeq     string `json:"dseq"`
		Manifest string `json:"manifest"`
	} `json:"data"`
}

// UnmarshalJSON tolerates a numeric dseq. It is a uint64 on chain and a string in
// every response we have seen, but a client that dies on `"dseq": 1787103872228`
// would be one API release away from being unable to deploy at all.
func (r *createDeploymentResponse) UnmarshalJSON(b []byte) error {
	var raw struct {
		Data struct {
			DSeq     json.RawMessage `json:"dseq"`
			Manifest string          `json:"manifest"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	dseq, err := decodeSeq(raw.Data.DSeq)
	if err != nil {
		return fmt.Errorf("dseq: %w", err)
	}
	r.Data.DSeq = dseq
	r.Data.Manifest = raw.Data.Manifest
	return nil
}

// decodeSeq renders a dseq as the decimal string we store, from either JSON form.
func decodeSeq(raw json.RawMessage) (string, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return "", nil
	}
	if s[0] == '"' {
		var out string
		if err := json.Unmarshal(raw, &out); err != nil {
			return "", err
		}
		return strings.TrimSpace(out), nil
	}
	// Decimal digits only: a float round trip through 1787103872228 is exact
	// today and silently wrong the day dseqs grow past 2^53.
	for _, r := range s {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("not a sequence number: %s", s)
		}
	}
	return s, nil
}

// Bid is one entry from GET /v1/bids.
type Bid struct {
	ID struct {
		Provider string `json:"provider"`
		GSeq     int    `json:"gseq"`
		OSeq     int    `json:"oseq"`
	} `json:"id"`
	Price struct {
		Denom  string `json:"denom"`
		Amount Num    `json:"amount"`
	} `json:"price"`
	State string `json:"state"`
}

// bidList decodes GET /v1/bids, whose entries nest the bid under a "bid" key —
// except when they do not. v1 handled both with `.bid // .`.
type bidList []Bid

func (l *bidList) UnmarshalJSON(b []byte) error {
	var wrapped struct {
		Data []struct {
			Bid *Bid `json:"bid"`
			// The inline form: the same fields, one level up.
			ID *struct {
				Provider string `json:"provider"`
				GSeq     int    `json:"gseq"`
				OSeq     int    `json:"oseq"`
			} `json:"id"`
			Price *struct {
				Denom  string `json:"denom"`
				Amount Num    `json:"amount"`
			} `json:"price"`
			State string `json:"state"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &wrapped); err != nil {
		return err
	}
	out := make([]Bid, 0, len(wrapped.Data))
	for _, e := range wrapped.Data {
		switch {
		case e.Bid != nil:
			out = append(out, *e.Bid)
		case e.ID != nil:
			var bid Bid
			bid.ID.Provider, bid.ID.GSeq, bid.ID.OSeq = e.ID.Provider, e.ID.GSeq, e.ID.OSeq
			if e.Price != nil {
				bid.Price.Denom, bid.Price.Amount = e.Price.Denom, e.Price.Amount
			}
			bid.State = e.State
			out = append(out, bid)
		}
	}
	*l = out
	return nil
}
