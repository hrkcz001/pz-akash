package main

// The two small decisions in `pzctl controller` that are not the FSM: what the
// plain-text status line says, and whether a remote needs the deploy key.

import (
	"strings"
	"testing"

	"github.com/hrkcz001/pz-akash/pzctl/internal/fsm"
	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// TestStatusLineDoesNotReportAPhaseItCannotHave is the fix for a line that read
// `status=offline intent=stopped phase=starting` on a world that had just been halted
// — which names the one thing that was definitely not happening.
//
// The phase in a snapshot is the agent's, and I16 only believes a report that names
// the lease we hold. With no lease there is no report to believe, so the value is the
// default of the empty document standing in for one, and state.NewAgent quite
// correctly defaults a booting agent to "starting". Nothing is wrong upstream; the
// print site is what has to know that a phase without a lease is not a reading.
func TestStatusLineDoesNotReportAPhaseItCannotHave(t *testing.T) {
	for _, tc := range []struct {
		name string
		snap fsm.Snapshot
		want string
	}{
		{
			// The halt, as it actually looked.
			name: "stopped with the lease closed",
			snap: fsm.Snapshot{
				Status: state.StatusOffline, Intent: state.IntentStopped,
				Phase: state.PhaseStarting,
			},
			want: "status=offline intent=stopped phase=none",
		},
		{
			name: "running, where the phase is a real reading",
			snap: fsm.Snapshot{
				Status: state.StatusOnline, Intent: state.IntentRunning,
				Phase: state.PhaseOnline,
				Lease: &state.Lease{DSeq: "1787468373720"},
			},
			want: "status=online intent=running phase=online",
		},
		{
			// A lease exists and the agent has not reported into it yet. "starting" is
			// true here, and it is the same stored value the first case suppresses — so
			// this is the case that proves the lease is what the decision turns on.
			name: "a lease with no report yet",
			snap: fsm.Snapshot{
				Status: state.StatusBooting, Intent: state.IntentRunning,
				Phase: state.PhaseStarting,
				Lease: &state.Lease{DSeq: "1787468373720"},
			},
			want: "status=booting intent=running phase=starting",
		},
		{
			// A controller that has not read the branch yet. Every field is empty, and
			// the line still has to parse as three key=value pairs, because this is the
			// answer `curl .../state` gives during the first seconds of a boot.
			name: "nothing known yet",
			snap: fsm.Snapshot{},
			want: "status= intent= phase=none",
		},
	} {
		if got := statusLine(tc.snap); got != tc.want {
			t.Errorf("%s: statusLine = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestStatusLineIsTheSameSentenceOnBothPorts is why this is a function and not two
// format strings. /state and the two-port /healthz answer the same question, and a
// status line that disagrees with itself depending on which port was asked is the
// sort of thing an operator diagnoses for an hour.
func TestStatusLineIsTheSameSentenceOnBothPorts(t *testing.T) {
	snap := fsm.Snapshot{
		Status: state.StatusOnline, Intent: state.IntentRunning,
		Phase: state.PhaseOnline, Lease: &state.Lease{DSeq: "1"},
	}
	line := statusLine(snap)
	for _, want := range []string{"status=", "intent=", "phase="} {
		if !strings.Contains(line, want) {
			t.Errorf("status line %q is missing %q", line, want)
		}
	}
	if strings.Contains(line, "\n") {
		t.Errorf("status line %q spans lines; both handlers add their own newline", line)
	}
}

// TestNeedsDeployKeyOnlyForRemotesThatCanUseOne guards the case it was written for: a
// --dry-run against a local clone must not be refused for want of a credential its
// transport has no place to put. That is not hypothetical — it is the only way to walk
// the whole lifecycle without touching the live repository.
func TestNeedsDeployKeyOnlyForRemotesThatCanUseOne(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"git@github.com:hrkcz001/pz-saves.git", true},
		{"ssh://git@github.com/hrkcz001/pz-saves.git", true},

		{"", false},
		{"https://github.com/hrkcz001/pz-saves.git", false},
		{"http://localhost:8080/pz-saves.git", false},
		{"file:///tmp/pz-saves/.git", false},
		{"/tmp/pz-saves/.git", false},
		{"../pz-saves/.git", false},
		{`C:\Users\hrkcz001\pz-saves\.git`, false},
	} {
		if got := needsDeployKey(tc.url); got != tc.want {
			t.Errorf("needsDeployKey(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}
