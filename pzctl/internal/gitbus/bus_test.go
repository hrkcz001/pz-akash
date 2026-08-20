package gitbus

import (
	"reflect"
	"strings"
	"testing"

	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

func newBuses(t *testing.T, remote string) (*ControllerBus, *AgentBus) {
	t.Helper()
	// Two independent mirrors, because the controller and the agent run in
	// different containers. Sharing one Repo here would hide staleness bugs.
	cb, err := NewControllerBus(openMirror(t, remote), testBranches())
	if err != nil {
		t.Fatalf("NewControllerBus: %v", err)
	}
	ab, err := NewAgentBus(openMirror(t, remote), testBranches())
	if err != nil {
		t.Fatalf("NewAgentBus: %v", err)
	}
	return cb, ab
}

func TestControllerAndAgentExchangeStateThroughTheirOwnBranches(t *testing.T) {
	t.Parallel()
	remote := seedRemote(t, liveish())
	cb, ab := newBuses(t, remote)
	loc := prague(t)

	doc := state.NewController(loc)
	doc.Intent = state.IntentRunning
	if err := doc.SetStatus(state.StatusDeploying, state.Now(loc)); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	doc.Lease = &state.Lease{DSeq: "20603991", GSeq: 1, OSeq: 1, Provider: "akash1prov", CreatedAt: state.Now(loc)}
	idx := state.NewBackups()
	idx.Upsert(state.Backup{Name: "backup_20260819_013623.zip", Size: 1234, CreatedAt: state.Now(loc)})
	if _, err := cb.Publish(t.Context(), doc, idx, "deploying"); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// The agent's only source of intent is the controller's branch.
	if err := ab.Fetch(t.Context()); err != nil {
		t.Fatalf("agent Fetch: %v", err)
	}
	gotDoc, gotIdx, r, err := ab.ReadController()
	if err != nil {
		t.Fatalf("ReadController: %v", err)
	}
	if !r.OK() {
		t.Fatalf("round trip needed repairs: %s", r)
	}
	if gotDoc.Intent != state.IntentRunning || gotDoc.Status != state.StatusDeploying {
		t.Fatalf("intent/status = %q/%q, want running/deploying", gotDoc.Intent, gotDoc.Status)
	}
	if gotDoc.Lease == nil || gotDoc.Lease.DSeq != "20603991" {
		t.Fatalf("lease = %+v, want dseq 20603991", gotDoc.Lease)
	}
	if len(gotIdx.Items) != 1 || gotIdx.Items[0].Size != 1234 {
		t.Fatalf("index = %+v", gotIdx.Items)
	}

	// And back the other way.
	agent := state.NewAgent(loc)
	agent.SetPhase(state.PhaseOnline, state.Now(loc))
	agent.SetPlayers(3, state.Now(loc))
	if _, err := ab.Publish(t.Context(), agent, "online"); err != nil {
		t.Fatalf("agent Publish: %v", err)
	}
	if err := cb.Fetch(t.Context()); err != nil {
		t.Fatalf("controller Fetch: %v", err)
	}
	gotAgent, r, err := cb.ReadAgent()
	if err != nil {
		t.Fatalf("ReadAgent: %v", err)
	}
	if !r.OK() {
		t.Fatalf("agent round trip needed repairs: %s", r)
	}
	if gotAgent.Phase != state.PhaseOnline {
		t.Fatalf("phase = %q, want online", gotAgent.Phase)
	}
	if !gotAgent.PlayersKnown() || gotAgent.PlayersCount != 3 {
		t.Fatalf("players = %d (known=%v), want a known 3", gotAgent.PlayersCount, gotAgent.PlayersKnown())
	}
	fsck(t, remote)
}

// TestBusMethodSetsArePinned is the enforcement of invariant I4. Ownership is
// meant to be structural: the controller cannot write agent.json because it has no
// method that does, not because we remember not to call one. Reflection is the
// only way to assert the absence of a method, and pinning the whole set means
// adding a writer to either side has to be a deliberate edit to this list.
func TestBusMethodSetsArePinned(t *testing.T) {
	t.Parallel()
	want := map[reflect.Type][]string{
		reflect.TypeOf(&ControllerBus{}): {
			// Exists was added in step 3 for the backups pause file. It is a read:
			// one ReadFile, with a missing path answered as false rather than an
			// error. It takes a branch name, so it can look at the agent's branch —
			// which is fine, and is what ReadAgent does too. Reading either side is
			// unrestricted; only writing is owned.
			"Branches", "ConsumeTriggers", "Exists", "Fetch", "Head", "Publish",
			"Pushed", "ReadAgent", "ReadConfigBytes", "ReadOwn", "ShouldAct", "Triggers",
		},
		reflect.TypeOf(&AgentBus{}): {
			"Fetch", "Publish", "ReadConfigBytes", "ReadController", "ReadOwn",
		},
	}
	for typ, exp := range want {
		var got []string
		for i := 0; i < typ.NumMethod(); i++ {
			got = append(got, typ.Method(i).Name)
		}
		if strings.Join(got, ",") != strings.Join(exp, ",") {
			t.Errorf("%s method set:\n got %v\nwant %v\n"+
				"if this is intentional, check the new method cannot write the other side's branch",
				typ, got, exp)
		}
	}
}

func TestPublishRefusesToDropTheIndexOnADedicatedBranch(t *testing.T) {
	t.Parallel()
	// A dedicated state branch is replaced whole, so publishing the document
	// without the index would delete backups.json — and the index is the only
	// record of which archives exist.
	cb, _ := newBuses(t, seedRemote(t, liveish()))
	if _, err := cb.Publish(t.Context(), state.NewController(prague(t)), nil, "no index"); err == nil {
		t.Fatal("Publish accepted a nil index on a dedicated branch")
	}
}

func TestSingleLayoutWritesAlongsideTheOperatorsFiles(t *testing.T) {
	t.Parallel()
	// The `single` layout is v1's shape, kept as a config escape hatch. It has to
	// work without destroying config.yaml, which is what a whole-tree replace on
	// main would do.
	remote := seedRemote(t, liveish())
	br := Branches{Main: "main", Controller: "main", Agent: "main", TriggersDir: "triggers"}
	cb, err := NewControllerBus(openMirror(t, remote), br)
	if err != nil {
		t.Fatalf("NewControllerBus: %v", err)
	}
	if _, err := cb.Publish(t.Context(), state.NewController(prague(t)), state.NewBackups(), "single"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	for _, path := range []string{"config.yaml", "README.md", "controller.json", "backups.json"} {
		if _, ok := remoteFile(t, remote, "main", path); !ok {
			t.Fatalf("%s missing after a single-layout publish", path)
		}
	}
	// Here the index may be omitted, because the files are independent.
	if _, err := cb.Publish(t.Context(), state.NewController(prague(t)), nil, "single, no index"); err != nil {
		t.Fatalf("Publish without index in single layout: %v", err)
	}
	if _, ok := remoteFile(t, remote, "main", "backups.json"); !ok {
		t.Fatal("backups.json was dropped by a document-only publish")
	}
}

func TestReadsOfAnUnpublishedBranchYieldNormalizedDefaults(t *testing.T) {
	t.Parallel()
	// First boot: nothing has ever been published. This must not be an error, and
	// the defaults must already satisfy their invariants — in particular the player
	// count must read as unknown, not as an empty server.
	cb, ab := newBuses(t, seedRemote(t, liveish()))

	agent, r, err := cb.ReadAgent()
	if err != nil || !r.OK() {
		t.Fatalf("ReadAgent on a fresh repo = %v, repairs %s", err, r)
	}
	if agent.PlayersKnown() {
		t.Fatalf("a never-published agent claims to know the count: %d", agent.PlayersCount)
	}

	doc, idx, r, err := ab.ReadController()
	if err != nil || !r.OK() {
		t.Fatalf("ReadController on a fresh repo = %v, repairs %s", err, r)
	}
	if doc.Intent != state.IntentStopped || doc.Status != state.StatusOffline || doc.Lease != nil {
		t.Fatalf("defaults = %q/%q/lease %+v; a controller that guesses \"running\" deploys on its own",
			doc.Intent, doc.Status, doc.Lease)
	}
	if idx.Items == nil || len(idx.Items) != 0 {
		t.Fatalf("index = %+v, want empty and non-nil", idx.Items)
	}
}

// TestRepairOnReadSurvivesTheTransport reproduces bug 3 across the bus. The
// document is the live corrupt one, byte for byte.
func TestRepairOnReadSurvivesTheTransport(t *testing.T) {
	t.Parallel()
	const corrupt = `{"ip": "", "port": 2222, "game_port": 16261, "status": "stopping", "players_count": 0, "price_per_hour": , "price_per_day": }`
	remote := seedRemote(t, liveish())
	r := openMirror(t, remote)
	if _, err := r.PutOrphan(t.Context(), "state/controller", map[string][]byte{
		"controller.json": []byte(corrupt),
		"backups.json":    []byte(`{"version":1,"items":[]}`),
	}, "corrupt"); err != nil {
		t.Fatalf("seed corrupt document: %v", err)
	}

	ab, err := NewAgentBus(openMirror(t, remote), testBranches())
	if err != nil {
		t.Fatalf("NewAgentBus: %v", err)
	}
	doc, _, rep, err := ab.ReadController()
	if err != nil {
		t.Fatalf("ReadController: %v", err)
	}
	if !rep.Fatal() {
		t.Fatalf("the corrupt document read clean: %s", rep)
	}
	// v1 read this and stopped doing periodic backups and restores, silently. Here
	// it costs the document's contents and nothing else: the reader is still
	// running, still holds usable defaults, and knows it must reconcile.
	if doc.Status != state.StatusOffline || doc.Intent != state.IntentStopped {
		t.Fatalf("fell back to %q/%q, want the safe defaults", doc.Status, doc.Intent)
	}
	if doc.Lease != nil {
		t.Fatalf("a fatally corrupt document produced a lease: %+v", doc.Lease)
	}
}

func TestTriggersAreListedThenConsumedExactlyOnce(t *testing.T) {
	t.Parallel()
	remote := seedRemote(t, liveish())
	cb, _ := newBuses(t, remote)

	trigs, err := cb.Triggers()
	if err != nil {
		t.Fatalf("Triggers: %v", err)
	}
	if len(trigs) != 2 || trigs[0].Name != "backup-please" || trigs[1].Name != "start" {
		t.Fatalf("Triggers = %+v", trigs)
	}
	if !strings.Contains(string(trigs[0].Body), "before the update") {
		t.Fatalf("trigger body = %q", trigs[0].Body)
	}

	sha, err := cb.ConsumeTriggers(t.Context(), []string{"start", "backup-please"})
	if err != nil {
		t.Fatalf("ConsumeTriggers: %v", err)
	}
	if sha == "" {
		t.Fatal("ConsumeTriggers made no commit")
	}
	// Deleting the file is what makes a trigger one-shot. v1 rewrote sentinel
	// contents instead, so a crash between acting and clearing re-ran the action.
	if got, err := cb.Triggers(); err != nil || len(got) != 0 {
		t.Fatalf("after consume, Triggers = %+v, %v", got, err)
	}
	if _, ok := remoteFile(t, remote, "main", "config.yaml"); !ok {
		t.Fatal("consuming a trigger deleted config.yaml")
	}

	// Idempotent: a redelivery finds nothing to do and pushes nothing.
	if sha, err := cb.ConsumeTriggers(t.Context(), []string{"start"}); err != nil || sha != "" {
		t.Fatalf("second consume = %q, %v; want no commit", sha, err)
	}
	if sha, err := cb.ConsumeTriggers(t.Context(), nil); err != nil || sha != "" {
		t.Fatalf("consume of nothing = %q, %v", sha, err)
	}
}

func TestShouldActRejectsOurEchoesAndStateBranches(t *testing.T) {
	t.Parallel()
	remote := seedRemote(t, liveish())
	cb, _ := newBuses(t, remote)

	ours, err := cb.Publish(t.Context(), state.NewController(prague(t)), state.NewBackups(), "publish")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	operator := runGit(t, remote, "rev-parse", "main")

	cases := []struct {
		ref, sha string
		want     bool
		why      string
	}{
		{"refs/heads/main", operator, true, "an operator push to the trigger branch is the only actionable event"},
		{"main", operator, true, "the short ref form is accepted too"},
		{"refs/heads/main", ours, false, "our own commit, redelivered"},
		{"refs/heads/state/controller", ours, false, "our state publish generates a delivery on every transition"},
		{"refs/heads/state/agent", operator, false, "the agent's branch carries no operator intent"},
		{"refs/tags/v1", operator, false, "not a branch we watch"},
	}
	for _, tc := range cases {
		if got := cb.ShouldAct(tc.ref, tc.sha); got != tc.want {
			t.Errorf("ShouldAct(%q, %.7s) = %v, want %v — %s", tc.ref, tc.sha, got, tc.want, tc.why)
		}
	}

	// The in-memory filter forgets across a restart, which is why the FSM also
	// consults the document's ring. Pushed() is what it folds in.
	found := false
	for _, sha := range cb.Pushed() {
		found = found || sha == ours
	}
	if !found {
		t.Fatalf("Pushed() = %v, missing %s", cb.Pushed(), ours)
	}
}

func TestCommitTimestampsCarryTheConfiguredOffset(t *testing.T) {
	t.Parallel()
	// The one wall-clock reference is identity.timezone, not the container's
	// clock. v1's controller image had no TZ and no zoneinfo, so `git log` and
	// backup names came out UTC by accident.
	remote := seedRemote(t, liveish())
	cb, _ := newBuses(t, remote)
	if _, err := cb.Publish(t.Context(), state.NewController(prague(t)), state.NewBackups(), "stamp"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	got := runGit(t, remote, "log", "-1", "--format=%ai", "state/controller")
	if !strings.HasSuffix(got, "+0200") && !strings.HasSuffix(got, "+0100") {
		t.Fatalf("commit date %q carries neither CEST (+0200) nor CET (+0100)", got)
	}
}

func TestBranchesValidation(t *testing.T) {
	t.Parallel()
	ok := testBranches()
	bad := []struct {
		br  Branches
		why string
	}{
		{Branches{}, "empty"},
		{Branches{Main: "main", Agent: "state/agent", TriggersDir: "t"}, "no controller branch"},
		{Branches{Main: "main", Controller: "state/x", Agent: "state/x", TriggersDir: "t"},
			"two writers force-pushing one dedicated branch would erase each other"},
		{Branches{Main: "main", Controller: "state/c", Agent: "state/a"}, "no triggers dir"},
	}
	for _, tc := range bad {
		if err := tc.br.validate(); err == nil {
			t.Errorf("validate(%+v) accepted: %s", tc.br, tc.why)
		}
	}
	if err := ok.validate(); err != nil {
		t.Fatalf("validate(%+v) = %v", ok, err)
	}
	// The single layout is the one case where the two state branches may coincide.
	single := Branches{Main: "main", Controller: "main", Agent: "main", TriggersDir: "triggers"}
	if err := single.validate(); err != nil {
		t.Fatalf("validate(single layout) = %v", err)
	}
}
