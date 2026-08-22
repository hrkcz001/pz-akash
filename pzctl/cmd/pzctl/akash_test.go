package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hrkcz001/pz-akash/pzctl/internal/akash"
	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// These tests are about money, not about the Akash wire format — internal/akash
// covers that against its own fake. What is at stake here is the arithmetic of
// escrows: every deployment this loop creates has to be either the one it returns or
// one it closed, and there is no third case that is acceptable. A retry layered on
// top of an open deployment funds two escrows and can put two servers behind one DNS
// name, which is invariant I1.
//
// The live failure that prompted the loop was a provider that won the bid at 5
// uact/block and then answered `POST /v1/leases` with 404 — deposit taken, no lease.
// fakeDeployer reproduces that shape: a dseq comes back with the error.

type fakeDeployer struct {
	// results is consumed one entry per attempt.
	results []fakeAttempt
	// closeErr, when non-nil, fails every close. Modelling the case that must not
	// retry: we cannot release what we just funded.
	closeErr error

	deploys []int    // attempt numbers seen, in order
	closed  []string // dseqs closed, in order
	roles   []string // which method was called
}

type fakeAttempt struct {
	dseq string // "" means the request failed before anything was created
	url  string
	err  error
}

func (f *fakeDeployer) next(role string) (akash.Result, error) {
	f.roles = append(f.roles, role)
	i := len(f.deploys)
	f.deploys = append(f.deploys, i+1)
	if i >= len(f.results) {
		return akash.Result{}, fmt.Errorf("fake: attempt %d not scripted", i+1)
	}
	a := f.results[i]
	res := akash.Result{URL: a.url}
	if a.dseq != "" {
		res.Lease = state.Lease{DSeq: a.dseq, Provider: "akash1fake", GSeq: 1, OSeq: 1}
	}
	return res, a.err
}

func (f *fakeDeployer) DeployServer(context.Context, akash.DeployOptions) (akash.Result, error) {
	return f.next("server")
}

func (f *fakeDeployer) DeployController(context.Context) (akash.Result, error) {
	return f.next("controller")
}

func (f *fakeDeployer) Close(_ context.Context, l state.Lease) error {
	if f.closeErr != nil {
		return f.closeErr
	}
	f.closed = append(f.closed, l.DSeq)
	return nil
}

// cfgWithAttempts is the smallest config akashDeploy reads: a resource block for the
// role (so resourcesFor succeeds) and the attempt budget.
func cfgWithAttempts(n int) *config.Config {
	c := &config.Config{}
	c.Akash.MaxDeployAttempts = n
	return c
}

func quiet(string, ...any) {}

func TestAkashDeployRetriesPastAProviderThatRefusesTheLease(t *testing.T) {
	f := &fakeDeployer{results: []fakeAttempt{
		{dseq: "111", err: errors.New("POST /v1/leases: HTTP 404: no lease for deployment")},
		{dseq: "222", err: errors.New("POST /v1/leases: HTTP 404: no lease for deployment")},
		{dseq: "333", url: "https://provider.example:31234"},
	}}
	if err := akashDeploy(context.Background(), f, cfgWithAttempts(4),
		"controller", "", false, quiet); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if len(f.deploys) != 3 {
		t.Errorf("made %d attempts, want 3", len(f.deploys))
	}
	// The two failures are closed; the success is left open, because that is the
	// deployment the operator asked for.
	if got, want := strings.Join(f.closed, ","), "111,222"; got != want {
		t.Errorf("closed %q, want %q", got, want)
	}
}

// The attempt number has to reach the driver: internal/akash uses it for the deploy
// log line and to decide it is retrying, and passing a constant 1 was the bug that
// made every CLI attempt look like a first one.
func TestAkashDeployPassesTheAttemptNumberThrough(t *testing.T) {
	f := &fakeDeployer{results: []fakeAttempt{
		{dseq: "111", err: errors.New("no bid")},
		{dseq: "222", url: "https://ok.example"},
	}}
	var seen []int
	d := &attemptRecorder{fakeDeployer: f, seen: &seen}
	if err := akashDeploy(context.Background(), d, cfgWithAttempts(3),
		"server", "https://controller.example", false, quiet); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if len(seen) != 2 || seen[0] != 1 || seen[1] != 2 {
		t.Errorf("driver saw attempts %v, want [1 2]", seen)
	}
}

type attemptRecorder struct {
	*fakeDeployer
	seen *[]int
}

func (a *attemptRecorder) DeployServer(ctx context.Context, o akash.DeployOptions) (akash.Result, error) {
	*a.seen = append(*a.seen, o.Attempt)
	return a.fakeDeployer.DeployServer(ctx, o)
}

// The one path that must not retry. If the close fails we do not know the escrow was
// released, so funding a second one is how a stranded lease bills for a month.
func TestAkashDeployStopsWhenCloseFails(t *testing.T) {
	f := &fakeDeployer{
		results: []fakeAttempt{
			{dseq: "111", err: errors.New("would not accept the lease")},
			{dseq: "222", url: "https://never.example"},
		},
		closeErr: errors.New("HTTP 503"),
	}
	err := akashDeploy(context.Background(), f, cfgWithAttempts(4),
		"controller", "", false, quiet)
	if err == nil {
		t.Fatal("want an error when the failed deployment could not be closed")
	}
	if len(f.deploys) != 1 {
		t.Errorf("made %d attempts, want to stop after 1", len(f.deploys))
	}
	// The dseq has to be in the message; it is the only handle that stops the bill.
	for _, want := range []string{"111", "pzctl akash close"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// Nothing created means nothing to skip-list and nothing to release: the same
// request would fail the same way, so retrying only spends time.
func TestAkashDeployDoesNotRetryWhenNothingWasCreated(t *testing.T) {
	f := &fakeDeployer{results: []fakeAttempt{
		{err: errors.New("akash: no provider meets the placement criteria")},
		{dseq: "222", url: "https://unreached.example"},
	}}
	err := akashDeploy(context.Background(), f, cfgWithAttempts(4),
		"controller", "", false, quiet)
	if err == nil {
		t.Fatal("want the error through")
	}
	if len(f.deploys) != 1 {
		t.Errorf("made %d attempts, want 1", len(f.deploys))
	}
	if len(f.closed) != 0 {
		t.Errorf("closed %v, want nothing — no deployment existed", f.closed)
	}
}

// Exhausting the budget must leave nothing billing, and must say how many attempts
// were spent rather than reporting only the last provider's excuse.
func TestAkashDeployExhaustsTheBudgetAndClosesEverything(t *testing.T) {
	f := &fakeDeployer{results: []fakeAttempt{
		{dseq: "1", err: errors.New("refused")},
		{dseq: "2", err: errors.New("refused")},
	}}
	err := akashDeploy(context.Background(), f, cfgWithAttempts(2),
		"controller", "", false, quiet)
	if err == nil {
		t.Fatal("want an error when every attempt failed")
	}
	if !strings.Contains(err.Error(), "2 deploy attempts") {
		t.Errorf("error %q does not say how many attempts were spent", err)
	}
	if got, want := strings.Join(f.closed, ","), "1,2"; got != want {
		t.Errorf("closed %q, want %q — every created deployment must be released", got, want)
	}
}

// --close is a throwaway round trip, so its success also closes. It must not then be
// retried: the deploy worked.
func TestAkashDeployWithCloseAfterClosesTheSuccess(t *testing.T) {
	f := &fakeDeployer{results: []fakeAttempt{{dseq: "777", url: "https://gate.example"}}}
	if err := akashDeploy(context.Background(), f, cfgWithAttempts(4),
		"controller", "", true, quiet); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if got, want := strings.Join(f.closed, ","), "777"; got != want {
		t.Errorf("closed %q, want %q", got, want)
	}
	if len(f.deploys) != 1 {
		t.Errorf("made %d attempts, want 1", len(f.deploys))
	}
}

// A config that forgot the field, or set it to zero, must still deploy once. Reading
// it as "attempt nothing" would turn a missing value into a silent no-op.
func TestAkashDeployTreatsZeroAttemptsAsOne(t *testing.T) {
	f := &fakeDeployer{results: []fakeAttempt{{dseq: "9", err: errors.New("refused")}}}
	if err := akashDeploy(context.Background(), f, cfgWithAttempts(0),
		"controller", "", false, quiet); err == nil {
		t.Fatal("want the error through")
	}
	if len(f.deploys) != 1 {
		t.Errorf("made %d attempts, want exactly 1", len(f.deploys))
	}
}

func TestAkashDeployRejectsAnUnknownRole(t *testing.T) {
	f := &fakeDeployer{}
	err := akashDeploy(context.Background(), f, cfgWithAttempts(4),
		"database", "", false, quiet)
	if err == nil || !strings.Contains(err.Error(), "unknown role") {
		t.Fatalf("err = %v, want an unknown-role error", err)
	}
	if len(f.deploys) != 0 {
		t.Errorf("an unknown role reached the API %d times", len(f.deploys))
	}
}
