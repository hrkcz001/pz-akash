package akash

// Deploy tests, written against the invariant that costs money: every path out of
// run() that has created something must hand the caller its dseq.
//
// The fixtures below are trimmed copies of real Console API responses, kept as raw
// JSON rather than built from the wire structs on purpose — a struct literal would
// still pass if a json tag were wrong, and a wrong tag here reads in production as
// "no bids" or "never became routable", the two failures that cost the most.

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

const (
	testDSeq     = "1787103872228"
	testProvider = "akash1good"
	testIP       = "203.0.113.10"
)

// providersDoc is GET /v1/providers: one provider we should pick, and two we
// should not, so a passing test also proves the filter ran.
func providersDoc(hostURI string) string {
	return `{"data":[
	  {"owner":"` + testProvider + `","isOnline":true,"isValidVersion":true,"featEndpointIp":true,
	   "uptime30d":0.995,"ipCountryCode":"DE","ipLat":50.11,"ipLon":8.68,
	   "hostUri":"` + hostURI + `",
	   "stats":{"cpu":{"available":32000},"memory":{"available":137438953472},
	            "storage":{"ephemeral":{"available":1099511627776}}}},
	  {"owner":"akash1faraway","isOnline":true,"isValidVersion":true,"featEndpointIp":true,
	   "uptime30d":0.999,"ipCountryCode":"US","ipLat":37.77,"ipLon":-122.41,
	   "stats":{"cpu":{"available":32000},"memory":{"available":137438953472},
	            "storage":{"ephemeral":{"available":1099511627776}}}},
	  {"owner":"akash1flaky","isOnline":true,"isValidVersion":true,"featEndpointIp":true,
	   "uptime30d":0.80,"ipCountryCode":"DE","ipLat":52.52,"ipLon":13.40,
	   "stats":{"cpu":{"available":32000},"memory":{"available":137438953472},
	            "storage":{"ephemeral":{"available":1099511627776}}}}
	]}`
}

// bidsDoc uses the nested {"bid":{…}} form and a decimal-string amount, which is
// what the endpoint actually sends.
func bidsDoc(owner string, amount string) string {
	return `{"data":[{"bid":{"id":{"provider":"` + owner + `","gseq":1,"oseq":1},
	  "price":{"denom":"uact","amount":"` + amount + `"},"state":"open"}}]}`
}

const noBidsDoc = `{"data":[]}`

const createdDoc = `{"data":{"dseq":"` + testDSeq + `","manifest":"MANIFEST-OPAQUE-BLOB"}}`

const leasedDoc = `{"data":{"deployment":{"state":"active"}}}`

// detailDoc is GET /v1/deployments/{dseq}. status is spliced in so a test can hand
// back the same deployment with and without an address.
func detailDoc(status string) string {
	return `{"data":{
	  "deployment":{"id":{"owner":"akash1owner","dseq":"` + testDSeq + `"},"state":"active"},
	  "leases":[{"id":{"owner":"akash1owner","dseq":"` + testDSeq + `","gseq":1,"oseq":1,
	                   "provider":"` + testProvider + `"},
	             "state":"active","price":{"denom":"uact","amount":"34.000000000000000000"},
	             "status":` + status + `}],
	  "escrow_account":{"state":{"state":"open",
	    "funds":[{"denom":"uact","amount":"2500000"}],
	    "transferred":[{"denom":"uact","amount":"500000"}]}}}}`
}

// serverReady is a lease status with a running server and its dedicated IP.
const serverReady = `{
  "services":{"pz-server":{"name":"pz-server","available":1,"total":1,"replicas":1,"ready_replicas":1}},
  "forwarded_ports":{},
  "ips":{"pz-server":[
    {"IP":"` + testIP + `","Port":16261,"ExternalPort":16261,"Protocol":"UDP"},
    {"IP":"` + testIP + `","Port":16262,"ExternalPort":16262,"Protocol":"UDP"}]}}`

// serverStarting is the same lease a minute earlier: the pod exists, nothing is
// ready, and no IP has been assigned.
const serverStarting = `{
  "services":{"pz-server":{"name":"pz-server","available":0,"total":1,"replicas":1,"ready_replicas":0}},
  "forwarded_ports":{},"ips":{}}`

// serverNoIP is the expensive one: the workload is up and billing, and the
// provider never publishes an address.
const serverNoIP = `{
  "services":{"pz-server":{"name":"pz-server","available":1,"total":1,"replicas":1,"ready_replicas":1}},
  "forwarded_ports":{},"ips":{}}`

// serverShared is the address the shipped config actually gets: no IP of our own,
// the provider's hostname, and an external port drawn from its pool. 31188 rather
// than 16261 because a shared endpoint ignores the SDL's `as:` — our own controller
// asked for 8000 and was handed 31188 — so a fixture that echoed the requested port
// would be testing a case that does not occur.
const serverShared = `{
  "services":{"pz-server":{"name":"pz-server","available":1,"total":1,"replicas":1,"ready_replicas":1}},
  "forwarded_ports":{"pz-server":[
    {"host":"provider.example.com","port":16261,"externalPort":31188,"proto":"UDP","name":"pz-server"}]},
  "ips":{}}`

// dedicatedIPConfig is the real config forced back onto the dedicated-IP path with
// PZ's second UDP socket enabled: the world the fixtures above describe.
//
// The shipped config no longer points there — no provider in the uact market would
// sell an IP, so the deployment moved to a shared endpoint with a single port — and
// both paths are still live code. So the dedicated tests pin the mode they are
// about rather than inheriting whichever way config.yaml happens to point, and the
// shared path gets tests of its own below. Everything else about the config stays
// real, which is what keeps a config edit breaking a test first.
func dedicatedIPConfig(t *testing.T) *config.Config {
	t.Helper()
	c := testConfig(t)
	c.Server.IPLease = true
	c.Server.Ports.UDP = 16262
	return c
}

func TestDeployServerHappyPath(t *testing.T) {
	f := newFakeAPI(t)
	f.json("GET", "/v1/providers", providersDoc(f.url()))
	f.json("POST", "/v1/deployments", createdDoc)
	f.json("POST", "/v1/leases", leasedDoc)

	// Bids arrive late and the workload becomes ready later still: the first poll of
	// each returns nothing useful, which is the normal case, not an error path.
	bidPolls := 0
	f.on("GET", "/v1/bids", func(*http.Request, []byte) (int, string) {
		bidPolls++
		if bidPolls < 3 {
			return 200, noBidsDoc
		}
		return 200, bidsDoc(testProvider, "34.000000000000000000")
	})
	detailPolls := 0
	f.on("GET", "/v1/deployments/", func(*http.Request, []byte) (int, string) {
		detailPolls++
		if detailPolls < 2 {
			return 200, detailDoc(serverStarting)
		}
		return 200, detailDoc(serverReady)
	})

	d, clk := newTestDriver(t, f, dedicatedIPConfig(t))
	res, err := d.DeployServer(context.Background(), DeployOptions{
		ControllerURL: "http://controller.example:8000",
		RestoreTarget: "latest",
		Attempt:       1,
	})
	if err != nil {
		t.Fatalf("DeployServer: %v", err)
	}

	if res.Lease.DSeq != testDSeq {
		t.Errorf("dseq = %q, want %q", res.Lease.DSeq, testDSeq)
	}
	if res.Lease.Provider != testProvider {
		t.Errorf("provider = %q, want %q", res.Lease.Provider, testProvider)
	}
	if res.Lease.GSeq != 1 || res.Lease.OSeq != 1 {
		t.Errorf("gseq/oseq = %d/%d, want 1/1", res.Lease.GSeq, res.Lease.OSeq)
	}
	// The stamps come from the driver's clock and identity.timezone, never from the
	// host: a lease created "at 13:04" in the provider's local time is a lease
	// nobody can correlate with a backup.
	if res.Lease.CreatedAt.Zero() {
		t.Error("the lease carries no creation timestamp")
	}
	if got, want := res.Lease.CreatedAt.Time.Location().String(), d.Cfg.Identity.Timezone; got != want {
		t.Errorf("CreatedAt is in %s, want identity.timezone %s", got, want)
	}
	if !res.Lease.CreatedAt.Time.Equal(testEpoch) {
		t.Errorf("CreatedAt = %s, want the driver clock's %s", res.Lease.CreatedAt, testEpoch)
	}
	if res.Price.QuotedAt.Zero() {
		t.Error("the price carries no quote timestamp")
	}
	if res.Price.QuotedAt.Time.Before(res.Lease.CreatedAt.Time) || res.Price.QuotedAt.Time.After(clk.Now()) {
		t.Errorf("QuotedAt %s is outside the deploy window %s..%s",
			res.Price.QuotedAt, res.Lease.CreatedAt, clk.Now())
	}
	if res.Endpoint.IP != testIP {
		t.Errorf("endpoint IP = %q, want %q", res.Endpoint.IP, testIP)
	}
	if res.Endpoint.GamePort != 16261 || res.Endpoint.UDPPort != 16262 {
		t.Errorf("ports = %d/%d, want 16261/16262", res.Endpoint.GamePort, res.Endpoint.UDPPort)
	}
	if !res.Endpoint.Ready() {
		t.Error("the endpoint is not Ready() after a successful deploy")
	}
	// The server has no shared endpoint, and a URL here would end up in a DNS A
	// record. See endpointFrom.
	if res.URL != "" {
		t.Errorf("URL = %q, want empty for a dedicated-IP deployment", res.URL)
	}

	// 34 uact/block × 14400 blocks/day ÷ 1e6 uact/USD. The whole fifth v1 bug was
	// this arithmetic being done as if uact were micro-AKT.
	if want := 34.0 * 14400 / 1e6; !approx(res.Price.USDPerDay, want, 1e-9) {
		t.Errorf("USD/day = %g, want %g", res.Price.USDPerDay, want)
	}
	if res.Price.AmountPerBlock != 34 || res.Price.Denom != "uact" {
		t.Errorf("price = %d %s, want 34 uact", res.Price.AmountPerBlock, res.Price.Denom)
	}
	if res.Price.AKTUSD != 0 {
		t.Errorf("AKT/USD = %g, want 0: a uact deploy must never consult the oracle", res.Price.AKTUSD)
	}
	if !approx(res.Price.USDPerHour, res.Price.USDPerDay/24, 1e-12) {
		t.Errorf("USD/hour %g is not USD/day %g ÷ 24", res.Price.USDPerHour, res.Price.USDPerDay)
	}

	// What was actually posted: the rendered SDL with the computed ceiling, and the
	// manifest handed back verbatim.
	var created struct {
		Data struct {
			SDL     string  `json:"sdl"`
			Deposit float64 `json:"deposit"`
		} `json:"data"`
	}
	f.lastBody(t, &created, "POST", "/v1/deployments")
	ceiling, err := d.ceiling(0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(created.Data.SDL, strconv.Itoa(ceiling)) {
		t.Errorf("the posted SDL does not carry the %d %s/block ceiling", ceiling, d.Cfg.Akash.Price.Denom)
	}
	if !strings.Contains(created.Data.SDL, "pz-server") {
		t.Error("the posted SDL does not declare the pz-server service")
	}
	if created.Data.Deposit != d.deposit() {
		t.Errorf("deposit = %g, want %g", created.Data.Deposit, d.deposit())
	}

	var leased struct {
		Manifest string `json:"manifest"`
		Leases   []struct {
			DSeq     string `json:"dseq"`
			GSeq     int    `json:"gseq"`
			Provider string `json:"provider"`
		} `json:"leases"`
	}
	f.lastBody(t, &leased, "POST", "/v1/leases")
	if leased.Manifest != "MANIFEST-OPAQUE-BLOB" {
		t.Errorf("manifest = %q; it must go back exactly as received", leased.Manifest)
	}
	if len(leased.Leases) != 1 || leased.Leases[0].DSeq != testDSeq ||
		leased.Leases[0].Provider != testProvider || leased.Leases[0].GSeq != 1 {
		t.Errorf("lease request = %+v, want one lease on %s from %s", leased.Leases, testDSeq, testProvider)
	}

	// Exactly one deployment was created. A retry loop that creates a second one
	// funds a second escrow nobody is tracking.
	if n := f.countCalls("POST", "/v1/deployments"); n != 1 {
		t.Errorf("created %d deployments, want 1", n)
	}
	// Two bid polls slept before any bid was acceptable (2×5s), then the settle
	// window runs from that first bid, then one detail poll (10s). The settle window
	// is on every deploy on purpose — see waitForBid — so it is named here rather
	// than folded into a total, and a config change moves the expectation with it.
	settle := time.Duration(d.Cfg.Akash.Timeouts.BidSettle)
	if want := 10*time.Second + settle + 10*time.Second; clk.slept != want {
		t.Errorf("slept %s, want %s (2 bid polls + %s settle + 1 detail poll)", clk.slept, want, settle)
	}
	if d.Skipped(testProvider) {
		t.Error("the provider we successfully leased from was skip-listed")
	}
}

// TestDeployServerNoEligibleProviders is the money test: when the market cannot
// host us, nothing is created, so there is no escrow to reclaim afterwards. v1
// created first and found out second, every single time.
func TestDeployServerNoEligibleProviders(t *testing.T) {
	f := newFakeAPI(t)
	f.json("GET", "/v1/providers", `{"data":[
	  {"owner":"akash1offline","isOnline":false,"isValidVersion":true,"featEndpointIp":true,"uptime30d":0.99,"ipCountryCode":"DE"},
	  {"owner":"akash1noip","isOnline":true,"isValidVersion":true,"featEndpointIp":false,"uptime30d":0.99,"ipCountryCode":"DE"}
	]}`)

	d, clk := newTestDriver(t, f, dedicatedIPConfig(t))
	res, err := d.DeployServer(context.Background(), DeployOptions{Attempt: 1})
	if err == nil {
		t.Fatal("DeployServer succeeded with no eligible provider")
	}
	if !strings.Contains(err.Error(), "placement criteria") {
		t.Errorf("error %q does not say the placement criteria were not met", err)
	}
	if res.Lease.DSeq != "" {
		t.Errorf("dseq = %q, want empty: nothing should have been created", res.Lease.DSeq)
	}
	if n := f.countCalls("POST", "/v1/deployments"); n != 0 {
		t.Errorf("created %d deployments before checking the market, want 0", n)
	}
	if clk.slept != 0 {
		t.Errorf("slept %s before finding an empty market, want 0", clk.slept)
	}
}

// TestDeployServerNoAcceptableBid: bids arrive, all unaffordable. The deployment
// exists by then, so the caller must get its dseq to close it — and the error has
// to say why, since "no bids found" was v1's least useful log line.
func TestDeployServerNoAcceptableBid(t *testing.T) {
	f := newFakeAPI(t)
	f.json("GET", "/v1/providers", providersDoc(f.url()))
	f.json("POST", "/v1/deployments", createdDoc)
	// 1e6 uact/block is 14400 USD/day against a 3.00 limit.
	f.json("GET", "/v1/bids", bidsDoc(testProvider, "1000000"))

	d, clk := newTestDriver(t, f, nil)
	res, err := d.DeployServer(context.Background(), DeployOptions{Attempt: 1})
	if err == nil {
		t.Fatal("DeployServer succeeded on a bid above the price ceiling")
	}
	if res.Lease.DSeq != testDSeq {
		t.Fatalf("dseq = %q, want %q: an unclosable deployment is a permanent charge",
			res.Lease.DSeq, testDSeq)
	}
	if !strings.Contains(err.Error(), "above the 3.00 limit") {
		t.Errorf("error %q does not name the price that was rejected", err)
	}
	if n := f.countCalls("POST", "/v1/leases"); n != 0 {
		t.Errorf("took %d leases on an unaffordable bid, want 0", n)
	}
	// bid_wait 90s at bid_poll 5s: the loop polls until the deadline and no further.
	if want := time.Duration(d.Cfg.Akash.Timeouts.BidWait); clk.slept != want {
		t.Errorf("slept %s waiting for bids, want %s", clk.slept, want)
	}
}

// TestDeployServerBidsNeverArrive distinguishes an empty market from a rejected
// one. Same symptom in v1, different diagnosis.
func TestDeployServerBidsNeverArrive(t *testing.T) {
	f := newFakeAPI(t)
	f.json("GET", "/v1/providers", providersDoc(f.url()))
	f.json("POST", "/v1/deployments", createdDoc)
	f.json("GET", "/v1/bids", noBidsDoc)

	d, _ := newTestDriver(t, f, nil)
	res, err := d.DeployServer(context.Background(), DeployOptions{Attempt: 1})
	if err == nil {
		t.Fatal("DeployServer succeeded with no bids at all")
	}
	if !strings.Contains(err.Error(), "no provider bid at all") {
		t.Errorf("error %q does not distinguish an empty market from a rejected bid", err)
	}
	if res.Lease.DSeq != testDSeq {
		t.Errorf("dseq = %q, want %q", res.Lease.DSeq, testDSeq)
	}
}

// TestDeployServerBidsMissingIsNotFatal: /v1/bids answers 404 until the deployment
// is indexed. Treating that as a failure aborts a deploy that was about to work.
func TestDeployServerBidsMissingIsNotFatal(t *testing.T) {
	f := newFakeAPI(t)
	f.json("GET", "/v1/providers", providersDoc(f.url()))
	f.json("POST", "/v1/deployments", createdDoc)
	f.json("POST", "/v1/leases", leasedDoc)
	f.json("GET", "/v1/deployments/", detailDoc(serverReady))

	polls := 0
	f.on("GET", "/v1/bids", func(*http.Request, []byte) (int, string) {
		polls++
		if polls == 1 {
			return 404, `{"error":"deployment not found"}`
		}
		return 200, bidsDoc(testProvider, "34")
	})

	d, _ := newTestDriver(t, f, dedicatedIPConfig(t))
	res, err := d.DeployServer(context.Background(), DeployOptions{Attempt: 1})
	if err != nil {
		t.Fatalf("DeployServer gave up on a 404 from the bids endpoint: %v", err)
	}
	if res.Endpoint.IP != testIP {
		t.Errorf("endpoint IP = %q, want %q", res.Endpoint.IP, testIP)
	}
}

// TestDeployServerLeaseRefused: the provider bid and then would not lease. The
// dseq must come back, and the provider must be skip-listed so the next attempt
// does not walk into the same wall.
func TestDeployServerLeaseRefused(t *testing.T) {
	f := newFakeAPI(t)
	f.json("GET", "/v1/providers", providersDoc(f.url()))
	f.json("POST", "/v1/deployments", createdDoc)
	f.json("GET", "/v1/bids", bidsDoc(testProvider, "34"))
	f.fail("POST", "/v1/leases", 400, `{"error":"bid no longer open"}`)

	d, _ := newTestDriver(t, f, nil)
	res, err := d.DeployServer(context.Background(), DeployOptions{Attempt: 1})
	if err == nil {
		t.Fatal("DeployServer succeeded although the lease was refused")
	}
	if res.Lease.DSeq != testDSeq {
		t.Errorf("dseq = %q, want %q", res.Lease.DSeq, testDSeq)
	}
	if !d.Skipped(testProvider) {
		t.Errorf("%s refused the lease and was not skip-listed", testProvider)
	}
}

// TestDeployServerLeaseNotActive: a 200 from /v1/leases is not success on its own.
// The response carries the deployment state, and anything but active means the
// lease did not take — while the deployment is funded either way.
func TestDeployServerLeaseNotActive(t *testing.T) {
	f := newFakeAPI(t)
	f.json("GET", "/v1/providers", providersDoc(f.url()))
	f.json("POST", "/v1/deployments", createdDoc)
	f.json("GET", "/v1/bids", bidsDoc(testProvider, "34"))
	f.json("POST", "/v1/leases", `{"data":{"deployment":{"state":"closed"}}}`)

	d, _ := newTestDriver(t, f, nil)
	res, err := d.DeployServer(context.Background(), DeployOptions{Attempt: 1})
	if err == nil {
		t.Fatal("DeployServer accepted a lease that left the deployment closed")
	}
	if !strings.Contains(err.Error(), `"closed"`) {
		t.Errorf("error %q does not report the state the deployment was left in", err)
	}
	if res.Lease.DSeq != testDSeq {
		t.Errorf("dseq = %q, want %q", res.Lease.DSeq, testDSeq)
	}
}

// TestDeployServerLeasedButNeverRoutable is the most expensive failure in the
// system: the lease is live and billing, and the workload never gets an address.
// Everything the caller needs to stop the bleeding has to be in the Result.
func TestDeployServerLeasedButNeverRoutable(t *testing.T) {
	f := newFakeAPI(t)
	f.json("GET", "/v1/providers", providersDoc(f.url()))
	f.json("POST", "/v1/deployments", createdDoc)
	f.json("GET", "/v1/bids", bidsDoc(testProvider, "34"))
	f.json("POST", "/v1/leases", leasedDoc)
	f.json("GET", "/v1/deployments/", detailDoc(serverNoIP))
	// The provider is asked directly every akash.provider_status.every polls and
	// agrees that there is no IP.
	f.json("POST", "/v1/create-jwt-token", `{"data":{"token":"scoped.status.token"}}`)
	f.json("GET", "/lease/", `{"services":{"pz-server":{"available":1,"ready_replicas":1}},"ips":{}}`)

	d, clk := newTestDriver(t, f, dedicatedIPConfig(t))
	res, err := d.DeployServer(context.Background(), DeployOptions{Attempt: 1})
	if err == nil {
		t.Fatal("DeployServer succeeded without an address")
	}
	if !strings.Contains(err.Error(), "did not become routable") {
		t.Errorf("error %q does not say what the wait was for", err)
	}
	if !strings.Contains(err.Error(), "no dedicated IP") {
		t.Errorf("error %q does not carry what the last poll saw", err)
	}
	// Everything needed to close the lease, on the error path.
	if res.Lease.DSeq != testDSeq || res.Lease.Provider != testProvider ||
		res.Lease.GSeq != 1 || res.Lease.OSeq != 1 {
		t.Errorf("lease = %+v; a lease that cannot be identified cannot be closed", res.Lease)
	}
	if !d.Skipped(testProvider) {
		t.Errorf("%s leased and never became routable and was not skip-listed", testProvider)
	}
	// The bid settle window, then the whole lease_ready budget spent waiting for an
	// address that never arrives. Both halves matter: the deploy is not allowed to
	// give up early, and it is not allowed to run past its budget either.
	settle := time.Duration(d.Cfg.Akash.Timeouts.BidSettle)
	if want := settle + time.Duration(d.Cfg.Akash.Timeouts.LeaseReady); clk.slept != want {
		t.Errorf("slept %s waiting for an address, want %s (%s settle + lease_ready)", clk.slept, want, settle)
	}
	// The provider was consulted, and its token was minted on its own cadence
	// rather than once per poll.
	if n := f.countCalls("GET", "/lease/"); n == 0 {
		t.Error("the provider was never asked directly")
	} else if polls := f.countCalls("GET", "/v1/deployments/"); n >= polls {
		t.Errorf("asked the provider %d times in %d polls; that is not a slower cadence", n, polls)
	}
}

// TestDeployServerProviderAnswersFirst: the Console API's lease status lags behind
// the provider's. v1 added the direct query because deploys were timing out
// waiting for an address the provider had already assigned.
func TestDeployServerProviderAnswersFirst(t *testing.T) {
	f := newFakeAPI(t)
	f.json("GET", "/v1/providers", providersDoc(f.url()))
	f.json("POST", "/v1/deployments", createdDoc)
	f.json("GET", "/v1/bids", bidsDoc(testProvider, "34"))
	f.json("POST", "/v1/leases", leasedDoc)
	// Console never catches up.
	f.json("GET", "/v1/deployments/", detailDoc(serverNoIP))
	f.json("POST", "/v1/create-jwt-token", `{"data":{"token":"scoped.status.token"}}`)
	f.json("GET", "/lease/", `{
	  "services":{"pz-server":{"name":"pz-server","available":1,"ready_replicas":1}},
	  "ips":{"pz-server":[
	    {"IP":"`+testIP+`","Port":16261,"ExternalPort":16261,"Protocol":"UDP"},
	    {"IP":"`+testIP+`","Port":16262,"ExternalPort":16262,"Protocol":"UDP"}]}}`)

	d, clk := newTestDriver(t, f, dedicatedIPConfig(t))
	res, err := d.DeployServer(context.Background(), DeployOptions{Attempt: 1})
	if err != nil {
		t.Fatalf("DeployServer: %v", err)
	}
	if res.Endpoint.IP != testIP || res.Endpoint.GamePort != 16261 {
		t.Errorf("endpoint = %+v, want %s:16261 from the provider", res.Endpoint, testIP)
	}
	// It returned at the first provider query rather than at the deadline: that is
	// the whole point of the second opinion. The bid settle window is separate and
	// comes first; subtracting it is what keeps this assertion about the endpoint.
	every := d.Cfg.Akash.ProviderStatus.Every
	settle := time.Duration(d.Cfg.Akash.Timeouts.BidSettle)
	endpointWait := clk.slept - settle
	if want := time.Duration(every-1) * time.Duration(d.Cfg.Akash.Timeouts.LeasePoll); endpointWait != want {
		t.Errorf("slept %s waiting for the endpoint, want %s (%d polls before the provider is asked)",
			endpointWait, want, every)
	}
	if n := f.countCalls("POST", "/v1/create-jwt-token"); n != 1 {
		t.Errorf("minted %d status tokens, want 1", n)
	}
}

// TestDeployServerUsesSharedEndpoint is the shipped configuration: no IP of our
// own, so the address is the provider's hostname and whatever external UDP port it
// drew from its pool. The port it drew is deliberately not the one the SDL asked
// for — a shared endpoint ignores `as:`, and a deploy that assumed otherwise would
// publish an address nobody can reach.
func TestDeployServerUsesSharedEndpoint(t *testing.T) {
	f := newFakeAPI(t)
	f.json("GET", "/v1/providers", providersDoc(f.url()))
	f.json("POST", "/v1/deployments", createdDoc)
	f.json("GET", "/v1/bids", bidsDoc(testProvider, "34"))
	f.json("POST", "/v1/leases", leasedDoc)
	f.json("GET", "/v1/deployments/", detailDoc(serverShared))

	d, _ := newTestDriver(t, f, nil)
	res, err := d.DeployServer(context.Background(), DeployOptions{Attempt: 1})
	if err != nil {
		t.Fatalf("DeployServer: %v", err)
	}
	if res.Endpoint.Host != "provider.example.com" {
		t.Errorf("host = %q, want the provider's own hostname", res.Endpoint.Host)
	}
	if res.Endpoint.GamePort != 31188 {
		t.Errorf("game port = %d, want the provider's 31188 and not the SDL's %d",
			res.Endpoint.GamePort, d.Cfg.Server.Ports.Game)
	}
	// Nothing invents an IP here. One in the endpoint would reach a DNS A record
	// and, being the shared ingress rather than ours, would point at every other
	// tenant on the provider.
	if res.Endpoint.IP != "" {
		t.Errorf("IP = %q, want empty without a dedicated IP", res.Endpoint.IP)
	}
	// One socket, one forward: UDPPort is a report of what exists on this path, so
	// with server.ports.udp: 0 there is nothing to report.
	if res.Endpoint.UDPPort != 0 {
		t.Errorf("UDPPort = %d, want 0 with a single configured socket", res.Endpoint.UDPPort)
	}
	if !res.Endpoint.Ready() || res.Endpoint.Addr() != "provider.example.com" {
		t.Errorf("endpoint %+v is not a usable address", res.Endpoint)
	}
	// Still a game address, so still no URL: the DNS record for it is a CNAME, and
	// anything with a scheme in it here would be written as one.
	if res.URL != "" {
		t.Errorf("URL = %q, want empty for a game address", res.URL)
	}
}

// TestDeployServerSharedEndpointNeedsAUDPForward: a provider that forwards our port
// over TCP has not given us a playable address, and accepting it would publish a
// server that answers a handshake and nothing else. The lease is billing by then,
// so this must surface as the routable failure rather than as success.
func TestDeployServerSharedEndpointNeedsAUDPForward(t *testing.T) {
	f := newFakeAPI(t)
	f.json("GET", "/v1/providers", providersDoc(f.url()))
	f.json("POST", "/v1/deployments", createdDoc)
	f.json("GET", "/v1/bids", bidsDoc(testProvider, "34"))
	f.json("POST", "/v1/leases", leasedDoc)
	f.json("GET", "/v1/deployments/", detailDoc(`{
	  "services":{"pz-server":{"name":"pz-server","available":1,"ready_replicas":1}},
	  "forwarded_ports":{"pz-server":[
	    {"host":"provider.example.com","port":16261,"externalPort":31188,"proto":"TCP","name":"pz-server"}]},
	  "ips":{}}`))
	f.json("POST", "/v1/create-jwt-token", `{"data":{"token":"scoped.status.token"}}`)
	f.json("GET", "/lease/", `{
	  "services":{"pz-server":{"name":"pz-server","available":1,"ready_replicas":1}},
	  "forwarded_ports":{},"ips":{}}`)

	d, _ := newTestDriver(t, f, nil)
	res, err := d.DeployServer(context.Background(), DeployOptions{Attempt: 1})
	if err == nil {
		t.Fatal("DeployServer accepted a TCP forward as a game address")
	}
	if !strings.Contains(err.Error(), "no forwarded udp port") {
		t.Errorf("error %q does not say the forward was not UDP", err)
	}
	// The dseq still comes back, because the lease is funded and has to be closable.
	if res.Lease.DSeq != testDSeq {
		t.Errorf("dseq = %q, want %q", res.Lease.DSeq, testDSeq)
	}
}

// TestDeployControllerUsesSharedEndpoint checks the other addressing mode: no
// dedicated IP, a provider hostname and an assigned port, and no restriction on
// geography — latency to a dashboard is nobody's problem.
func TestDeployControllerUsesSharedEndpoint(t *testing.T) {
	f := newFakeAPI(t)
	// Only the far-away provider is online, and it would fail the country filter the
	// server deploy applies.
	f.json("GET", "/v1/providers", `{"data":[
	  {"owner":"akash1faraway","isOnline":true,"isValidVersion":true,"featEndpointIp":false,
	   "uptime30d":0.99,"ipCountryCode":"US","ipLat":37.77,"ipLon":-122.41,
	   "stats":{"cpu":{"available":16000},"memory":{"available":68719476736},
	            "storage":{"ephemeral":{"available":549755813888}}}}]}`)
	f.json("POST", "/v1/deployments", createdDoc)
	f.json("GET", "/v1/bids", bidsDoc("akash1faraway", "34"))
	f.json("POST", "/v1/leases", leasedDoc)
	f.json("GET", "/v1/deployments/", `{"data":{
	  "deployment":{"id":{"dseq":"`+testDSeq+`"},"state":"active"},
	  "leases":[{"id":{"dseq":"`+testDSeq+`","gseq":1,"oseq":1,"provider":"akash1faraway"},
	    "state":"active","price":{"denom":"uact","amount":"34"},
	    "status":{"services":{"controller":{"name":"controller","available":1,"ready_replicas":1}},
	      "forwarded_ports":{"controller":[
	        {"host":"provider.example.com","port":8000,"externalPort":31234,"proto":"TCP","name":"controller"},
	        {"host":"provider.example.com","port":8080,"externalPort":31235,"proto":"TCP","name":"controller"}]},
	      "ips":{}}}]}}`)

	d, _ := newTestDriver(t, f, nil)
	res, err := d.DeployController(context.Background())
	if err != nil {
		t.Fatalf("DeployController: %v", err)
	}
	if want := "http://provider.example.com:31234"; res.URL != want {
		t.Errorf("URL = %q, want %q", res.URL, want)
	}
	// "Where players connect" is meaningless for the controller, so it stays zero.
	if res.Endpoint != (state.Endpoint{}) {
		t.Errorf("endpoint = %+v, want the zero value for a shared-endpoint deployment", res.Endpoint)
	}
	if res.Price.AmountPerBlock != 34 {
		t.Errorf("price = %d, want 34", res.Price.AmountPerBlock)
	}
}

// TestDeployControllerHTTPIngress: with controller.http_port set to 80 the service
// gets an ingress hostname and no forwarded port at all. Reading only
// forwarded_ports would wait out the whole lease_ready deadline for something that
// is never coming.
func TestDeployControllerHTTPIngress(t *testing.T) {
	cfg := testConfig(t)
	cfg.Controller.HTTPPort = 80

	f := newFakeAPI(t)
	f.json("GET", "/v1/providers", providersDoc(f.url()))
	f.json("POST", "/v1/deployments", createdDoc)
	f.json("GET", "/v1/bids", bidsDoc(testProvider, "34"))
	f.json("POST", "/v1/leases", leasedDoc)
	f.json("GET", "/v1/deployments/", `{"data":{
	  "deployment":{"id":{"dseq":"`+testDSeq+`"},"state":"active"},
	  "leases":[{"id":{"dseq":"`+testDSeq+`","gseq":1,"oseq":1,"provider":"`+testProvider+`"},
	    "state":"active","price":{"denom":"uact","amount":"34"},
	    "status":{"services":{"controller":{"name":"controller","available":1,"ready_replicas":1,
	      "uris":["xyz.ingress.provider.example.com"]}},
	      "forwarded_ports":{},"ips":{}}}]}}`)

	d, clk := newTestDriver(t, f, cfg)
	res, err := d.DeployController(context.Background())
	if err != nil {
		t.Fatalf("DeployController: %v", err)
	}
	if want := "http://xyz.ingress.provider.example.com"; res.URL != want {
		t.Errorf("URL = %q, want %q", res.URL, want)
	}
	// The address was there on the first poll, so the only thing slept is the bid
	// settle window. The controller pays it too: it is leased on the same market and
	// its price is decided the same way.
	if want := time.Duration(d.Cfg.Akash.Timeouts.BidSettle); clk.slept != want {
		t.Errorf("slept %s, want just the %s settle window — the address was available on the first poll",
			clk.slept, want)
	}
}

// approx compares floats that came out of a division.
func approx(got, want, tol float64) bool {
	d := got - want
	return d < tol && d > -tol
}
