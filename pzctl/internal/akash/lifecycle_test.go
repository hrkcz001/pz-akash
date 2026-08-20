package akash

// Lifecycle tests: Close, Alive, Adopt, Escrow, TopUp.
//
// These are the calls that decide whether money keeps being spent, so the
// assertions are mostly about which way each one errs. Close treats a 404 as
// success; Alive treats an unreadable answer as "still alive"; Adopt claims a
// deployment it cannot identify only when told to. Every one of those choices is
// the expensive-but-recoverable direction.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

var testLease = state.Lease{DSeq: testDSeq, GSeq: 1, OSeq: 1, Provider: testProvider}

func TestCloseIsIdempotent(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		wantOK bool
		// wantCalls is how many DELETEs should reach the API. A 500 is retryable and
		// closing twice is harmless, so it is tried again; a 404 is the answer we
		// wanted and must not be.
		wantCalls int
	}{
		{"closed", 200, `{"data":{"dseq":"` + testDSeq + `"}}`, true, 1},
		// The deployment is already gone, which is the outcome we asked for.
		{"already gone", 404, `{"error":"deployment not found"}`, true, 1},
		// Unknown: the caller has to be able to try again, so this must not be
		// reported as a successful close.
		{"server error", 500, `{"error":"internal"}`, false, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t)
			f.fail("DELETE", "/v1/deployments/", tc.status, tc.body)
			d, clk := newTestDriver(t, f, nil)

			err := d.Close(context.Background(), testLease)
			if tc.wantOK && err != nil {
				t.Fatalf("Close: %v", err)
			}
			if !tc.wantOK && err == nil {
				t.Fatal("Close reported success on an unknown outcome")
			}
			if tc.wantOK {
				// The refund lands a few seconds after the close; the next deploy has to
				// see the reclaimed balance rather than bounce off a briefly short wallet.
				if want := time.Duration(d.Cfg.Akash.Timeouts.DepositSettle); clk.slept != want {
					t.Errorf("slept %s after closing, want the deposit_settle %s", clk.slept, want)
				}
			}
			if n := f.countCalls("DELETE", "/v1/deployments/"+testDSeq); n != tc.wantCalls {
				t.Errorf("sent %d DELETEs, want %d", n, tc.wantCalls)
			}
		})
	}
}

// TestCloseWithoutDSeq: the FSM calls Close on a document whose lease may already
// have been cleared. Nothing to close is not a failure, and it must not become a
// DELETE against an empty path.
func TestCloseWithoutDSeq(t *testing.T) {
	f := newFakeAPI(t)
	d, clk := newTestDriver(t, f, nil)

	if err := d.Close(context.Background(), state.Lease{}); err != nil {
		t.Fatalf("Close on an empty lease: %v", err)
	}
	if n := len(f.calls); n != 0 {
		t.Errorf("made %d calls for an empty lease, want 0: %v", n, f.calls)
	}
	if clk.slept != 0 {
		t.Errorf("slept %s waiting for a refund that was never due", clk.slept)
	}
}

func TestAlive(t *testing.T) {
	// A lease also carries reason: "lease_closed_invalid" while perfectly active —
	// it is an enum's zero value, not a statement. One case below includes it, so a
	// future reader of that field breaks a test rather than a server.
	cases := []struct {
		name    string
		status  int
		body    string
		want    bool
		wantErr bool
	}{
		{
			name:   "active lease",
			status: 200,
			body: `{"data":{"deployment":{"state":"active"},"leases":[
			  {"id":{"gseq":1,"oseq":1,"provider":"` + testProvider + `"},"state":"active",
			   "reason":"lease_closed_invalid"}]}}`,
			want: true,
		},
		{
			name:   "open deployment, lease closed under us",
			status: 200,
			body: `{"data":{"deployment":{"state":"open"},"leases":[
			  {"id":{"gseq":1,"oseq":1,"provider":"` + testProvider + `"},"state":"closed"}]}}`,
			want: false,
		},
		{
			// Escrow is still funded and nobody else is going to close it.
			name:   "open deployment with no lease",
			status: 200,
			body:   `{"data":{"deployment":{"state":"open"},"leases":[]}}`,
			want:   true,
		},
		{
			name:   "another provider's lease is not ours",
			status: 200,
			body: `{"data":{"deployment":{"state":"open"},"leases":[
			  {"id":{"gseq":1,"oseq":1,"provider":"akash1someoneelse"},"state":"active"}]}}`,
			want: true,
		},
		{
			name:   "deployment closed",
			status: 200,
			body:   `{"data":{"deployment":{"state":"closed"},"leases":[]}}`,
			want:   false,
		},
		{
			name:   "gone",
			status: 404,
			body:   `{"error":"deployment not found"}`,
			want:   false,
		},
		{
			// Unknown, not dead. Assuming dead here starts a second server alongside
			// a live one, which is the one outcome worse than paying twice.
			name:    "unreadable",
			status:  500,
			body:    `{"error":"internal"}`,
			wantErr: true,
		},
		{
			name:    "no state reported",
			status:  200,
			body:    `{"data":{"deployment":{},"leases":[]}}`,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t)
			f.fail("GET", "/v1/deployments/", tc.status, tc.body)
			d, _ := newTestDriver(t, f, nil)

			got, err := d.Alive(context.Background(), testLease)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Alive = %v, want an error the caller can treat as unknown", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Alive: %v", err)
			}
			if got != tc.want {
				t.Errorf("Alive = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAliveWithoutDSeq: no dseq is nothing to close.
func TestAliveWithoutDSeq(t *testing.T) {
	f := newFakeAPI(t)
	d, _ := newTestDriver(t, f, nil)
	alive, err := d.Alive(context.Background(), state.Lease{})
	if err != nil || alive {
		t.Fatalf("Alive on an empty lease = %v, %v; want false, nil", alive, err)
	}
	if len(f.calls) != 0 {
		t.Errorf("made calls for an empty lease: %v", f.calls)
	}
}

// TestAliveMatchesPartialIdentity: a dseq recorded before a bid was chosen has no
// gseq, oseq or provider. It still has to match the lease that exists, or the
// controller decides its own live deployment is not alive.
func TestAliveMatchesPartialIdentity(t *testing.T) {
	f := newFakeAPI(t)
	f.json("GET", "/v1/deployments/", `{"data":{"deployment":{"state":"active"},"leases":[
	  {"id":{"gseq":7,"oseq":3,"provider":"akash1whoever"},"state":"active"}]}}`)
	d, _ := newTestDriver(t, f, nil)

	alive, err := d.Alive(context.Background(), state.Lease{DSeq: testDSeq})
	if err != nil {
		t.Fatal(err)
	}
	if !alive {
		t.Error("Alive = false for a live lease identified only by dseq")
	}
}

// --- adoption ---

// adoptListDoc is GET /v1/deployments: our server, the controller, an unleased
// deployment, and one that is already closed.
const adoptListDoc = `{"data":[{"deployments":[
  {"deployment":{"id":{"owner":"akash1owner","dseq":"5001"},"state":"active"},
   "leases":[{"id":{"dseq":"5001","gseq":1,"oseq":1,"provider":"akash1a"},"state":"active"}]},
  {"deployment":{"id":{"owner":"akash1owner","dseq":"5002"},"state":"active"},
   "leases":[{"id":{"dseq":"5002","gseq":1,"oseq":1,"provider":"akash1b"},"state":"active"}]},
  {"deployment":{"id":{"owner":"akash1owner","dseq":"5003"},"state":"open"},"leases":[]},
  {"deployment":{"id":{"owner":"akash1owner","dseq":"5004"},"state":"closed"},"leases":[]}
], "pagination":{"total":4,"skip":0,"limit":1000,"hasMore":false}}]}`

// adoptDetail answers the per-deployment call Adopt makes to read service names.
func adoptDetail(r *http.Request, _ []byte) (int, string) {
	dseq := strings.TrimPrefix(r.URL.Path, "/v1/deployments/")
	switch dseq {
	case "5001": // ours: the server service is running in it
		return 200, `{"data":{"deployment":{"id":{"dseq":"5001"},"state":"active"},
		  "leases":[{"id":{"dseq":"5001","gseq":1,"oseq":1,"provider":"akash1a"},"state":"active",
		    "status":{"services":{"pz-server":{"name":"pz-server","available":1,"ready_replicas":1}}}}]}}`
	case "5002": // the controller — this must never be adopted
		return 200, `{"data":{"deployment":{"id":{"dseq":"5002"},"state":"active"},
		  "leases":[{"id":{"dseq":"5002","gseq":1,"oseq":1,"provider":"akash1b"},"state":"active",
		    "status":{"services":{"controller":{"name":"controller","available":1,"ready_replicas":1}}}}]}}`
	case "5003": // open, never leased: the wreckage of a crash between create and lease
		return 200, `{"data":{"deployment":{"id":{"dseq":"5003"},"state":"open"},"leases":[]}}`
	}
	return 404, `{"error":"deployment not found"}`
}

func TestAdopt(t *testing.T) {
	f := newFakeAPI(t)
	f.json("GET", "/v1/deployments", adoptListDoc)
	f.on("GET", "/v1/deployments/", adoptDetail)
	d, _ := newTestDriver(t, f, nil)

	got, err := d.Adopt(context.Background())
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	claimed := map[string]state.Lease{}
	for _, l := range got {
		claimed[l.DSeq] = l
	}
	if _, ok := claimed["5001"]; !ok {
		t.Error("did not claim 5001, the deployment running pz-server")
	}
	// The identification is the service name, which is why the controller cannot be
	// adopted — and why Adopt never needs to know its own dseq.
	if _, ok := claimed["5002"]; ok {
		t.Error("claimed 5002, the controller's own deployment")
	}
	if _, ok := claimed["5003"]; !ok {
		t.Error("did not claim 5003, an open deployment with no lease, with adopt_unleased on")
	}
	// Closed deployments hold no escrow, so they are not even looked at.
	if _, ok := claimed["5004"]; ok {
		t.Error("claimed 5004, which is already closed")
	}
	if n := f.countCalls("GET", "/v1/deployments/5004"); n != 0 {
		t.Errorf("inspected a closed deployment %d times, want 0", n)
	}
	if l := claimed["5001"]; l.GSeq != 1 || l.OSeq != 1 || l.Provider != "akash1a" {
		t.Errorf("claimed 5001 as %+v; a lease that cannot be identified cannot be closed", l)
	}
}

// TestAdoptUnleasedOff: on a wallet shared with hand-made deployments, an unleased
// one may be somebody else's, seconds old. The switch is what makes that safe.
func TestAdoptUnleasedOff(t *testing.T) {
	cfg := testConfig(t)
	cfg.Akash.AdoptUnleased = false

	f := newFakeAPI(t)
	f.json("GET", "/v1/deployments", adoptListDoc)
	f.on("GET", "/v1/deployments/", adoptDetail)
	d, _ := newTestDriver(t, f, cfg)

	got, err := d.Adopt(context.Background())
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if len(got) != 1 || got[0].DSeq != "5001" {
		t.Errorf("adopted %+v, want only 5001", got)
	}
}

// TestAdoptSurvivesAnUnreadableDeployment: one deployment that cannot be inspected
// must not hide the others. Adopt is the last line of defence for "no lease is left
// billing unwatched"; returning nothing because of one bad answer defeats it.
func TestAdoptSurvivesAnUnreadableDeployment(t *testing.T) {
	f := newFakeAPI(t)
	f.json("GET", "/v1/deployments", adoptListDoc)
	f.on("GET", "/v1/deployments/", func(r *http.Request, b []byte) (int, string) {
		if strings.HasSuffix(r.URL.Path, "/5001") {
			return 500, `{"error":"internal"}`
		}
		return adoptDetail(r, b)
	})
	d, _ := newTestDriver(t, f, nil)

	got, err := d.Adopt(context.Background())
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if len(got) != 1 || got[0].DSeq != "5003" {
		t.Errorf("adopted %+v, want 5003 despite 5001 being unreadable", got)
	}
}

func TestAdoptListFailureIsAnError(t *testing.T) {
	f := newFakeAPI(t)
	f.fail("GET", "/v1/deployments", 500, `{"error":"internal"}`)
	d, _ := newTestDriver(t, f, nil)

	if _, err := d.Adopt(context.Background()); err == nil {
		t.Fatal("Adopt reported an empty result when the list could not be read")
	}
}

// --- escrow ---

func TestEscrow(t *testing.T) {
	f := newFakeAPI(t)
	f.json("GET", "/v1/deployments/", detailDoc(serverReady))
	d, _ := newTestDriver(t, f, nil)

	e, err := d.Escrow(context.Background(), testDSeq)
	if err != nil {
		t.Fatalf("Escrow: %v", err)
	}
	if !e.Known {
		t.Fatal("escrow reported unknown for a uact balance")
	}
	// 2500000 uact at 1e-6 USD each.
	if !approx(e.RemainingUSD, 2.5, 1e-9) {
		t.Errorf("remaining = %g, want 2.50", e.RemainingUSD)
	}
	if !approx(e.SpentUSD, 0.5, 1e-9) {
		t.Errorf("spent = %g, want 0.50", e.SpentUSD)
	}
	if e.Denom != "uact" {
		t.Errorf("denom = %q, want uact", e.Denom)
	}
	// A dollar-pegged credit needs no rate, so nothing left the process to price it.
	if n := f.countCalls("GET", "/v1/providers"); n != 0 {
		t.Error("pricing a uact escrow consulted something it did not need")
	}
}

// TestEscrowUnpriceableIsNotZero is the assertion that protects the wallet: an
// escrow holding only denominations this build cannot price is unknown, and a
// funds loop that reads unknown as zero tops up a deployment that needed nothing.
func TestEscrowUnpriceableIsNotZero(t *testing.T) {
	f := newFakeAPI(t)
	f.json("GET", "/v1/deployments/", `{"data":{"deployment":{"state":"active"},"leases":[],
	  "escrow_account":{"state":{"state":"open",
	    "funds":[{"denom":"ibc/12345","amount":"9999"}],"transferred":[]}}}}`)
	d, _ := newTestDriver(t, f, nil)

	e, err := d.Escrow(context.Background(), testDSeq)
	if err != nil {
		t.Fatalf("Escrow: %v", err)
	}
	if e.Known {
		t.Fatal("escrow claims to know a balance it could not price")
	}
	if e.RemainingUSD != 0 || e.SpentUSD != 0 {
		t.Errorf("unknown escrow reported %+v, want zeroes alongside Known=false", e)
	}
}

// --- top-up ---

func TestTopUp(t *testing.T) {
	f := newFakeAPI(t)
	f.json("POST", "/v1/deposit-deployment", `{"data":{"dseq":"`+testDSeq+`"}}`)
	d, _ := newTestDriver(t, f, nil)

	// Below the API's own minimum: rounding up spends a little more than asked,
	// which beats a loop that retries a rejected deposit forever.
	got, err := d.TopUp(context.Background(), testDSeq, 0.02)
	if err != nil {
		t.Fatalf("TopUp: %v", err)
	}
	floor := d.Cfg.Akash.Funds.MinTopupUSD
	if got != floor {
		t.Errorf("topped up $%g, want the $%g minimum", got, floor)
	}
	var body struct {
		Data struct {
			DSeq    string  `json:"dseq"`
			Deposit float64 `json:"deposit"`
		} `json:"data"`
	}
	f.lastBody(t, &body, "POST", "/v1/deposit-deployment")
	if body.Data.DSeq != testDSeq || body.Data.Deposit != floor {
		t.Errorf("posted %+v, want dseq %s and deposit %g", body.Data, testDSeq, floor)
	}

	// Above the minimum: asked for is what is sent.
	if got, err := d.TopUp(context.Background(), testDSeq, 4); err != nil || got != 4 {
		t.Fatalf("TopUp(4) = %g, %v; want 4, nil", got, err)
	}
	f.lastBody(t, &body, "POST", "/v1/deposit-deployment")
	if body.Data.Deposit != 4 {
		t.Errorf("posted deposit %g, want 4", body.Data.Deposit)
	}

	// Nothing to add is not a call. A zero-dollar deposit is an API error, and a
	// funds loop that makes one every tick is a rate limit waiting to happen.
	before := f.countCalls("POST", "/v1/deposit-deployment")
	if got, err := d.TopUp(context.Background(), testDSeq, 0); err != nil || got != 0 {
		t.Fatalf("TopUp(0) = %g, %v; want 0, nil", got, err)
	}
	if after := f.countCalls("POST", "/v1/deposit-deployment"); after != before {
		t.Errorf("TopUp(0) sent a request")
	}
}

func TestTopUpFailureIsReported(t *testing.T) {
	f := newFakeAPI(t)
	f.fail("POST", "/v1/deposit-deployment", 400, `{"error":"insufficient balance"}`)
	d, _ := newTestDriver(t, f, nil)

	got, err := d.TopUp(context.Background(), testDSeq, 1)
	if err == nil {
		t.Fatal("TopUp reported success on a rejected deposit")
	}
	if got != 0 {
		t.Errorf("TopUp = %g on failure, want 0: the caller must not think it is funded", got)
	}
	if !strings.Contains(err.Error(), "insufficient balance") {
		t.Errorf("error %q does not carry what the API said", err)
	}
}

// TestNotFoundIsTypedNotStringMatched: the FSM decides whether a close succeeded
// from this, so it must survive being wrapped.
func TestNotFoundIsTypedNotStringMatched(t *testing.T) {
	f := newFakeAPI(t)
	f.fail("GET", "/v1/deployments/", 404, `{"error":"deployment not found"}`)
	d, _ := newTestDriver(t, f, nil)

	_, err := d.Escrow(context.Background(), testDSeq)
	if err == nil {
		t.Fatal("Escrow succeeded against a deployment that does not exist")
	}
	if !NotFound(err) {
		t.Errorf("NotFound(%v) = false; the status did not survive wrapping", err)
	}
	if Status(err) != http.StatusNotFound {
		t.Errorf("Status = %d, want 404", Status(err))
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatal("the error is not an *APIError")
	}
	if !json.Valid([]byte(ae.Body)) {
		t.Errorf("the API error body %q was not preserved", ae.Body)
	}
}
