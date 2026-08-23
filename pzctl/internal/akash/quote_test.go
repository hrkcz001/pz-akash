package akash

// Quote tests.
//
// A quote creates a real deployment with a real deposit and then throws it away, so
// the property worth testing is not the table it prints — it is that the deployment
// is gone on every path out. An unclosed quote is a funded escrow with no lease:
// money in a deployment that will never do anything, and nothing in the system will
// ever notice it, because the FSM only knows about leases it took.
//
// The second property is that pricing the configuration we do not run must not
// change the configuration we do. QuoteServer flips server.ip_lease on a copy, and a
// shallow copy that turned out to share the wrong field would silently deploy the
// unpriced SDL on the next start.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

const secondBidder = "akash1second"

// quoteProvidersDoc is two providers that both pass every filter, so a bid can be
// rejected on price alone.
func quoteProvidersDoc(hostURI string) string {
	return `{"data":[
	  {"owner":"` + testProvider + `","isOnline":true,"isValidVersion":true,"featEndpointIp":true,
	   "uptime30d":0.995,"ipCountryCode":"DE","ipLat":50.11,"ipLon":8.68,
	   "hostUri":"` + hostURI + `",
	   "stats":{"cpu":{"available":32000},"memory":{"available":137438953472},
	            "storage":{"ephemeral":{"available":1099511627776}}}},
	  {"owner":"` + secondBidder + `","isOnline":true,"isValidVersion":true,"featEndpointIp":true,
	   "uptime30d":0.990,"ipCountryCode":"DE","ipLat":50.12,"ipLon":8.69,
	   "hostUri":"` + hostURI + `",
	   "stats":{"cpu":{"available":32000},"memory":{"available":137438953472},
	            "storage":{"ephemeral":{"available":1099511627776}}}}
	]}`
}

// offlineProviderDoc is a market that cannot host the spec at all.
const offlineProviderDoc = `{"data":[
  {"owner":"akash1down","isOnline":false,"isValidVersion":true,"featEndpointIp":true,
   "uptime30d":0.999,"ipCountryCode":"DE","ipLat":50.11,"ipLon":8.68,
   "stats":{"cpu":{"available":32000},"memory":{"available":137438953472},
            "storage":{"ephemeral":{"available":1099511627776}}}}
]}`

// TestCheapestIgnoresBidsWeCouldNotHaveTaken: the number this returns is quoted to a
// human as the market rate. A rejected bid is not a price we could have paid, and
// reporting the cheapest of those would recommend a provider that would have refused
// the lease — or an address that was never actually on offer.
func TestCheapestIgnoresBidsWeCouldNotHaveTaken(t *testing.T) {
	cases := []struct {
		name  string
		in    []Quote
		want  float64
		found bool
	}{
		{"nobody bid", nil, 0, false},
		{"everybody was rejected", []Quote{
			{Provider: "a", USDPerDay: 0.10, Why: "no ip capacity"},
			{Provider: "b", USDPerDay: 0.20, Why: "too expensive"},
		}, 0, false},
		{"the cheap bid was unacceptable", []Quote{
			{Provider: "a", USDPerDay: 0.10, Why: "no ip capacity"},
			{Provider: "b", USDPerDay: 0.80, Won: true},
		}, 0.80, true},
		// A bid with no reason and no win is one that would have been acceptable but
		// was not the cheapest. It is still a price the market offered.
		{"an acceptable runner-up counts", []Quote{
			{Provider: "a", USDPerDay: 0.90},
			{Provider: "b", USDPerDay: 0.80, Won: true},
		}, 0.80, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found := QuoteResult{Quotes: tc.in}.Cheapest()
			if found != tc.found {
				t.Fatalf("found = %v, want %v", found, tc.found)
			}
			if found && got.USDPerDay != tc.want {
				t.Errorf("cheapest = %.2f (%s), want %.2f", got.USDPerDay, got.Provider, tc.want)
			}
		})
	}
}

// TestQuoteClosesWhatItCreated is the invariant that costs money.
func TestQuoteClosesWhatItCreated(t *testing.T) {
	f := newFakeAPI(t)
	f.json("GET", "/v1/providers", providersDoc(f.url()))
	f.json("POST", "/v1/deployments", createdDoc)
	f.json("GET", "/v1/bids", bidsDoc(testProvider, "34.000000000000000000"))
	f.json("DELETE", "/v1/deployments/", `{"data":{"dseq":"`+testDSeq+`"}}`)

	d, _ := newTestDriver(t, f, nil)
	res, err := d.QuoteServer(context.Background(), true)
	if err != nil {
		t.Fatalf("QuoteServer: %v", err)
	}

	if res.DSeq != testDSeq {
		t.Errorf("dseq = %q, want %q — a quote that will not say what it created cannot be cleaned up by hand", res.DSeq, testDSeq)
	}
	if n := f.countCalls("DELETE", "/v1/deployments/"+testDSeq); n != 1 {
		t.Errorf("sent %d DELETEs, want 1: the deposit stays locked otherwise", n)
	}
	if n := f.countCalls("POST", "/v1/leases"); n != 0 {
		t.Errorf("took %d leases; a quote must never lease, because a lease is what bills", n)
	}
	best, found := res.Cheapest()
	if !found {
		t.Fatal("no cheapest bid from a market that bid acceptably")
	}
	if best.Provider != testProvider || !best.Won {
		t.Errorf("cheapest = %+v, want the winning bid from %s", best, testProvider)
	}
	if res.Eligible != 1 {
		t.Errorf("eligible = %d, want 1 of the three in the fixture", res.Eligible)
	}
	if res.CeilingUSD != d.Cfg.Akash.Price.MaxUSDPerDay {
		t.Errorf("ceiling = %v, want the configured %v", res.CeilingUSD, d.Cfg.Akash.Price.MaxUSDPerDay)
	}
}

// TestQuoteClosesEvenWhenTheBidsFail: the close is deferred rather than written at
// the end of the happy path, because the paths that fail are the ones that leave
// money behind.
func TestQuoteClosesEvenWhenTheBidsFail(t *testing.T) {
	f := newFakeAPI(t)
	f.json("GET", "/v1/providers", providersDoc(f.url()))
	f.json("POST", "/v1/deployments", createdDoc)
	f.fail("GET", "/v1/bids", 500, `{"error":"internal"}`)
	f.json("DELETE", "/v1/deployments/", `{"data":{"dseq":"`+testDSeq+`"}}`)

	d, _ := newTestDriver(t, f, nil)
	res, err := d.QuoteServer(context.Background(), true)
	if err == nil {
		t.Fatal("QuoteServer reported success with the bids endpoint down")
	}
	if !strings.Contains(err.Error(), "reading bids") {
		t.Errorf("err = %v, want it to name the bid read", err)
	}
	// The dseq has to survive the error for the same reason a deploy's does: it is
	// the only handle on what was created.
	if res.DSeq != testDSeq {
		t.Errorf("dseq = %q on the error path, want %q", res.DSeq, testDSeq)
	}
	if n := f.countCalls("DELETE", "/v1/deployments/"+testDSeq); n != 1 {
		t.Errorf("sent %d DELETEs after a failed quote, want 1", n)
	}
}

// TestQuoteWithNoEligibleProviderCreatesNothing: "nobody can host this" is an answer,
// not an error — it is the answer for a dedicated IP on most of the network. It must
// be reached without spending a deposit to find out.
func TestQuoteWithNoEligibleProviderCreatesNothing(t *testing.T) {
	f := newFakeAPI(t)
	f.json("GET", "/v1/providers", offlineProviderDoc)

	d, clk := newTestDriver(t, f, nil)
	res, err := d.QuoteServer(context.Background(), true)
	if err != nil {
		t.Fatalf("an empty market was reported as an error: %v", err)
	}
	if res.Eligible != 0 {
		t.Errorf("eligible = %d, want 0", res.Eligible)
	}
	if res.DSeq != "" {
		t.Errorf("dseq = %q, want none: nothing should have been created", res.DSeq)
	}
	if n := f.countCalls("POST", "/v1/deployments"); n != 0 {
		t.Errorf("created %d deployments with nobody able to host them", n)
	}
	if clk.slept != 0 {
		t.Errorf("slept %s waiting for bids that nobody could place", clk.slept)
	}
}

// TestQuoteDoesNotFlipTheRealConfig: the no-IP round renders an SDL this system does
// not deploy. If that flag were written through to the shared config, the next
// DeployServer in the same process would deploy a world with no address — and the
// failure would surface minutes later as a lease that never becomes routable.
func TestQuoteDoesNotFlipTheRealConfig(t *testing.T) {
	f := newFakeAPI(t)
	f.json("GET", "/v1/providers", providersDoc(f.url()))
	f.json("POST", "/v1/deployments", createdDoc)
	f.json("GET", "/v1/bids", bidsDoc(testProvider, "34.000000000000000000"))
	f.json("DELETE", "/v1/deployments/", `{"data":{"dseq":"`+testDSeq+`"}}`)

	d, _ := newTestDriver(t, f, nil)
	before := d.Cfg.Server.IPLease
	if _, err := d.QuoteServer(context.Background(), !before); err != nil {
		t.Fatalf("QuoteServer: %v", err)
	}
	if got := d.Cfg.Server.IPLease; got != before {
		t.Errorf("server.ip_lease = %v after quoting the other way, want %v unchanged", got, before)
	}
}

// TestQuotePollsTheWholeWindowAndKeepsEveryReason: a deploy wants one bid soon; a
// quote wants the market. Returning at the first acceptable bid would under-report
// how many providers answered, and that count is half of what makes an
// with-IP/without-IP comparison mean anything.
func TestQuotePollsTheWholeWindowAndKeepsEveryReason(t *testing.T) {
	f := newFakeAPI(t)
	f.json("GET", "/v1/providers", quoteProvidersDoc(f.url()))
	f.json("POST", "/v1/deployments", createdDoc)
	f.json("DELETE", "/v1/deployments/", `{"data":{"dseq":"`+testDSeq+`"}}`)

	polls := 0
	f.on("GET", "/v1/bids", func(*http.Request, []byte) (int, string) {
		polls++
		return 200, bidsListDoc(
			bidEntry(testProvider, "34.000000000000000000"),
			// Two orders of magnitude over the ceiling: acceptable-looking bidder,
			// unacceptable price.
			bidEntry(secondBidder, "3400.000000000000000000"),
		)
	})

	d, clk := newTestDriver(t, f, nil)
	res, err := d.QuoteServer(context.Background(), true)
	if err != nil {
		t.Fatalf("QuoteServer: %v", err)
	}

	poll := time.Duration(d.Cfg.Akash.Timeouts.BidPoll)
	wait := time.Duration(d.Cfg.Akash.Timeouts.BidWait)
	if want := int(wait/poll) + 1; polls != want {
		t.Errorf("polled bids %d times, want %d (the whole %s window, not the first acceptable bid)",
			polls, want, wait)
	}
	// The close waits for the refund, so the total is the window plus deposit_settle.
	if want := wait + time.Duration(d.Cfg.Akash.Timeouts.DepositSettle); clk.slept != want {
		t.Errorf("slept %s, want %s (%s of bidding + the deposit settle)", clk.slept, want, wait)
	}

	if len(res.Quotes) != 2 {
		t.Fatalf("recorded %d quotes, want both bidders: %+v", len(res.Quotes), res.Quotes)
	}
	byOwner := map[string]Quote{}
	for _, q := range res.Quotes {
		byOwner[q.Provider] = q
	}
	won := byOwner[testProvider]
	if !won.Won || won.Why != "" {
		t.Errorf("%s: won=%v why=%q, want the winner with no reason", testProvider, won.Won, won.Why)
	}
	if won.Country != "DE" {
		t.Errorf("%s: country = %q, want it carried through from the provider list", testProvider, won.Country)
	}
	if won.USDPerDay <= 0 {
		t.Errorf("%s: USD/day = %v, want a converted price", testProvider, won.USDPerDay)
	}
	// The rejected bidder is the more informative half of the table: "too expensive"
	// and "no IP capacity" are different answers to the same question, and dropping
	// the row would hide a bidder entirely.
	lost := byOwner[secondBidder]
	if lost.Won {
		t.Errorf("%s won at %v/day against a %v ceiling", secondBidder, lost.USDPerDay, res.CeilingUSD)
	}
	if lost.Why == "" {
		t.Errorf("%s was dropped without a reason", secondBidder)
	}
	if lost.AmountPerBlock == 0 || lost.Denom == "" {
		t.Errorf("%s: bid recorded as %v %q, want the chain's own numbers", secondBidder, lost.AmountPerBlock, lost.Denom)
	}
	// Sorted by price, so a printed table reads cheapest-first.
	if res.Quotes[0].USDPerDay > res.Quotes[1].USDPerDay {
		t.Errorf("quotes are not sorted by price: %v then %v", res.Quotes[0].USDPerDay, res.Quotes[1].USDPerDay)
	}
}
