package akash

// Bid-settle tests.
//
// These cover the one decision in this package that silently costs money forever.
// Everything else here either works or fails loudly; taking a bid that is $55/year
// worse than the one two polls behind it looks exactly like a successful deploy, and
// did — the live world was leased that way.
//
// Two providers, identical in every filter, differing only in price and in a name
// chosen so that alphabetical order favours the wrong one. If the settle window
// stops working, these fail on the owner name rather than passing by tie-break.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
)

const (
	// fastBidder answers immediately and dearly; slowBidder answers late and cheap.
	// The z is deliberate: SelectBid's last tie-break is the owner string, so a
	// broken settle window picks afast and cannot accidentally pick zcheap.
	fastBidder = "akash1afast"
	slowBidder = "akash1zcheap"
)

// bidEntry is one element of GET /v1/bids, in the nested {"bid":{…}} form the
// endpoint really sends.
func bidEntry(owner, amount string) string {
	return `{"bid":{"id":{"provider":"` + owner + `","gseq":1,"oseq":1},
	  "price":{"denom":"uact","amount":"` + amount + `"},"state":"open"}}`
}

func bidsListDoc(entries ...string) string {
	doc := `{"data":[`
	for i, e := range entries {
		if i > 0 {
			doc += ","
		}
		doc += e
	}
	return doc + `]}`
}

// settleProviders are two providers that pass every filter identically, so price is
// the only thing SelectBid can be choosing on.
func settleProviders() []Provider {
	return []Provider{good(fastBidder), good(slowBidder)}
}

// bidsArrivingAt registers a /v1/bids handler on the fake clock's terms: the
// expensive bid from the first poll, the cheap one only once `after` has elapsed.
// Fake time advances by one bid_poll per poll, so the poll index is the clock.
func bidsArrivingAt(f *fakeAPI, poll, after time.Duration) {
	expensive := bidEntry(fastBidder, "34.000000000000000000")
	cheap := bidEntry(slowBidder, "20.000000000000000000")
	polls := 0
	f.on("GET", "/v1/bids", func(*http.Request, []byte) (int, string) {
		elapsed := time.Duration(polls) * poll
		polls++
		if elapsed < after {
			return 200, bidsListDoc(expensive)
		}
		return 200, bidsListDoc(expensive, cheap)
	})
}

// TestWaitForBidPrefersACheaperLateBid is the regression for the live lease: an
// acceptable bid on the first poll must not end the search.
func TestWaitForBidPrefersACheaperLateBid(t *testing.T) {
	f := newFakeAPI(t)
	cfg := testConfig(t)
	poll := time.Duration(cfg.Akash.Timeouts.BidPoll)
	settle := time.Duration(cfg.Akash.Timeouts.BidSettle)
	if settle < 30*time.Second {
		t.Fatalf("config ships bid_settle = %v; too short to shop a real market", settle)
	}
	// Late, but comfortably inside the settle window the first bid opens.
	bidsArrivingAt(f, poll, 30*time.Second)

	d, clk := newTestDriver(t, f, cfg)
	choice, err := d.waitForBid(context.Background(), testDSeq, serverCriteria(), settleProviders())
	if err != nil {
		t.Fatalf("waitForBid: %v", err)
	}
	if choice.Provider.Owner != slowBidder {
		t.Errorf("leased %s at %.4f USD/day; the cheaper bid from %s was on the table",
			choice.Provider.Owner, choice.USDPerDay, slowBidder)
	}
	// The window has to have actually been waited out, not short-circuited by the
	// second bid arriving: settling means "stop looking at this time", not "stop
	// looking once someone undercuts".
	if clk.slept < settle {
		t.Errorf("slept %v before choosing, want at least the %v settle window", clk.slept, settle)
	}
}

// TestWaitForBidSettlesFromTheFirstBidNotFromEntry: a market that answers late must
// still get a full window, because slow bidders are the entire reason to wait.
func TestWaitForBidSettlesFromTheFirstBidNotFromEntry(t *testing.T) {
	f := newFakeAPI(t)
	cfg := testConfig(t)
	poll := time.Duration(cfg.Akash.Timeouts.BidPoll)
	settle := time.Duration(cfg.Akash.Timeouts.BidSettle)
	cfg.Akash.Timeouts.BidWait = config.Duration(settle * 4)

	// Nothing at all until well after entry, then the expensive bid, then the cheap
	// one near the end of the window that first bid opens.
	firstAt := settle
	cheapAt := settle * 2
	expensive := bidEntry(fastBidder, "34.000000000000000000")
	cheap := bidEntry(slowBidder, "20.000000000000000000")
	polls := 0
	f.on("GET", "/v1/bids", func(*http.Request, []byte) (int, string) {
		elapsed := time.Duration(polls) * poll
		polls++
		switch {
		case elapsed < firstAt:
			return 200, noBidsDoc
		case elapsed < cheapAt:
			return 200, bidsListDoc(expensive)
		default:
			return 200, bidsListDoc(expensive, cheap)
		}
	})

	d, _ := newTestDriver(t, f, cfg)
	choice, err := d.waitForBid(context.Background(), testDSeq, serverCriteria(), settleProviders())
	if err != nil {
		t.Fatalf("waitForBid: %v", err)
	}
	if choice.Provider.Owner != slowBidder {
		t.Errorf("leased %s; a window measured from the first bid would have seen %s undercut it",
			choice.Provider.Owner, slowBidder)
	}
}

// TestWaitForBidZeroSettleTakesTheFirstAcceptableBid pins the documented meaning of
// bid_settle: 0. It is the pre-fix behaviour, kept reachable on purpose so a market
// that has to be leased right now can be.
func TestWaitForBidZeroSettleTakesTheFirstAcceptableBid(t *testing.T) {
	f := newFakeAPI(t)
	cfg := testConfig(t)
	cfg.Akash.Timeouts.BidSettle = 0
	bidsArrivingAt(f, time.Duration(cfg.Akash.Timeouts.BidPoll), 30*time.Second)

	d, clk := newTestDriver(t, f, cfg)
	choice, err := d.waitForBid(context.Background(), testDSeq, serverCriteria(), settleProviders())
	if err != nil {
		t.Fatalf("waitForBid: %v", err)
	}
	if choice.Provider.Owner != fastBidder {
		t.Errorf("owner = %q, want the first acceptable bidder %q", choice.Provider.Owner, fastBidder)
	}
	if clk.slept != 0 {
		t.Errorf("slept %v with bid_settle = 0; it should return on the first poll", clk.slept)
	}
}

// TestWaitForBidTakesWhatItHasAtTheDeadline: bid_wait is the ceiling and settling is
// a preference. A deploy must never fail holding a usable bid — that trades a
// working server for a cheaper one that may not exist.
func TestWaitForBidTakesWhatItHasAtTheDeadline(t *testing.T) {
	f := newFakeAPI(t)
	cfg := testConfig(t)
	poll := time.Duration(cfg.Akash.Timeouts.BidPoll)
	wait := time.Duration(cfg.Akash.Timeouts.BidWait)
	settle := time.Duration(cfg.Akash.Timeouts.BidSettle)

	// The only bid lands late enough that its settle window runs past bid_wait, and
	// nothing ever undercuts it.
	firstAt := wait - settle/2
	if firstAt <= 0 {
		t.Fatalf("bid_wait %v is not longer than half of bid_settle %v", wait, settle)
	}
	expensive := bidEntry(fastBidder, "34.000000000000000000")
	polls := 0
	f.on("GET", "/v1/bids", func(*http.Request, []byte) (int, string) {
		elapsed := time.Duration(polls) * poll
		polls++
		if elapsed < firstAt {
			return 200, noBidsDoc
		}
		return 200, bidsListDoc(expensive)
	})

	d, clk := newTestDriver(t, f, cfg)
	choice, err := d.waitForBid(context.Background(), testDSeq, serverCriteria(), settleProviders())
	if err != nil {
		t.Fatalf("waitForBid gave up holding a usable bid: %v", err)
	}
	if choice.Provider.Owner != fastBidder {
		t.Errorf("owner = %q, want %q", choice.Provider.Owner, fastBidder)
	}
	if clk.slept > wait {
		t.Errorf("slept %v, past the %v bid_wait ceiling", clk.slept, wait)
	}
}
