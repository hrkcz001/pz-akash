package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

func prague(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		t.Fatalf("Europe/Prague: %v (embedded tzdata missing?)", err)
	}
	return loc
}

// TestReportDistinguishesUnknownFromZeroPlayers is bug 1 at the presentation
// layer, which is where the operator actually met it. A count of zero and a count
// nobody measured must not render the same, or a working RCON query and a dead
// one look identical on the dashboard.
func TestReportDistinguishesUnknownFromZeroPlayers(t *testing.T) {
	loc := prague(t)

	unknown := state.NewAgent(loc) // PlayersUnknown by construction
	var buf bytes.Buffer
	printReport(&buf, reportInput{
		Loc: loc, Source: "test",
		Controller: state.NewController(loc), Backups: state.NewBackups(),
		Agent: unknown, Repairs: &state.Repairs{}, AgentRepairs: &state.Repairs{},
	})
	if got := buf.String(); !strings.Contains(got, "players    unknown") &&
		!strings.Contains(got, "unknown — not measured") {
		t.Fatalf("an unmeasured count did not render as unknown:\n%s", got)
	}
	if strings.Contains(buf.String(), "players    0") {
		t.Fatal("an unmeasured count rendered as 0, which is bug 1")
	}

	empty := state.NewAgent(loc)
	empty.SetPlayers(0, state.Now(loc))
	buf.Reset()
	printReport(&buf, reportInput{
		Loc: loc, Source: "test",
		Controller: state.NewController(loc), Backups: state.NewBackups(),
		Agent: empty, Repairs: &state.Repairs{}, AgentRepairs: &state.Repairs{},
	})
	if got := buf.String(); !strings.Contains(got, "0 (measured") {
		t.Fatalf("a measured zero did not render as a measurement:\n%s", got)
	}
}

// TestReportNeverPrintsAZeroStampAsADate keeps the report honest about fields
// nothing has written. v1's dashboard showed 1970-01-01 for these, which reads as
// an observation rather than as an absence.
func TestReportNeverPrintsAZeroStampAsADate(t *testing.T) {
	loc := prague(t)
	doc := state.NewController(loc)
	doc.Since = state.Stamp{} // never entered

	var buf bytes.Buffer
	printReport(&buf, reportInput{
		Loc: loc, Source: "test",
		Controller: doc, Backups: state.NewBackups(),
		Repairs: &state.Repairs{}, AgentRepairs: &state.Repairs{},
	})
	got := buf.String()
	for _, bad := range []string{"1970-01-01", "0001-01-01"} {
		if strings.Contains(got, bad) {
			t.Fatalf("report printed %s as if it were an observation:\n%s", bad, got)
		}
	}
	if !strings.Contains(got, "never") {
		t.Fatalf("an unstamped field did not render as never:\n%s", got)
	}
}

// TestReportShowsTheAgentIsAbsentRatherThanDefault matters because the two
// readings license different actions: an agent that has never published means
// "nothing is running", while a published document full of defaults would mean
// "something is running and reporting nonsense".
func TestReportShowsTheAgentIsAbsentRatherThanDefault(t *testing.T) {
	loc := prague(t)
	var buf bytes.Buffer
	printReport(&buf, reportInput{
		Loc: loc, Source: "test",
		Controller: state.NewController(loc), Backups: state.NewBackups(),
		Agent: nil, Repairs: &state.Repairs{}, AgentRepairs: &state.Repairs{},
	})
	if got := buf.String(); !strings.Contains(got, "never published") {
		t.Fatalf("a missing agent document was not reported as missing:\n%s", got)
	}
}

// TestReportPrintsTheAddressAnEndpointActuallyHas covers both shapes a lease can
// take, because the report line was written when only one of them existed. On a
// shared endpoint there is no IP at all — the address is the provider's hostname —
// and printing the IP field rendered the live world as ":30975", which reads like a
// broken endpoint rather than a working one on a borrowed name.
//
// Ready() already distinguished the two, so nothing else in the program was wrong;
// this is the one place that asked the endpoint for the wrong field, and only an
// operator reading the output would have noticed.
func TestReportPrintsTheAddressAnEndpointActuallyHas(t *testing.T) {
	loc := prague(t)
	for name, tc := range map[string]struct {
		ep   state.Endpoint
		want string
	}{
		"shared endpoint": {
			state.Endpoint{Host: "provider.akash.metz.live", GamePort: 30975},
			"provider.akash.metz.live:30975",
		},
		"dedicated ip": {
			state.Endpoint{IP: "194.107.163.7", GamePort: 16261},
			"194.107.163.7:16261",
		},
	} {
		doc := state.NewController(loc)
		doc.Endpoint = tc.ep

		var buf bytes.Buffer
		printReport(&buf, reportInput{
			Loc: loc, Source: "test",
			Controller: doc, Backups: state.NewBackups(),
			Repairs: &state.Repairs{}, AgentRepairs: &state.Repairs{},
		})
		got := buf.String()
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: report does not contain %q:\n%s", name, tc.want, got)
		}
		// A port with nothing in front of it is the failure mode, not just a
		// missing name: it is what the operator saw.
		if strings.Contains(got, "endpoint  :") {
			t.Errorf("%s: report printed a bare port with no address:\n%s", name, got)
		}
	}
}

func TestBytesHuman(t *testing.T) {
	for in, want := range map[int64]string{
		0: "0 B", -1: "0 B", 512: "512 B",
		1024: "1.0 KiB", 1536: "1.5 KiB",
		1024 * 1024: "1.0 MiB", 3 * 1024 * 1024 * 1024: "3.0 GiB",
	} {
		if got := bytesHuman(in); got != want {
			t.Errorf("bytesHuman(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestFirstLineIsBoundedAndSurvivesEmptyBodies(t *testing.T) {
	if got := firstLine(nil); got != "(empty)" {
		t.Errorf("firstLine(nil) = %q", got)
	}
	// A trigger body is operator-authored and arrives over the network. It is
	// echoed into a terminal, so its length has to be bounded here.
	long := firstLine([]byte(strings.Repeat("x", 500)))
	if len(long) > 80 {
		t.Errorf("firstLine did not bound a 500-byte body: %d chars", len(long))
	}
	if got := firstLine([]byte("first\nsecond")); got != "first …" {
		t.Errorf("firstLine of a multi-line body = %q", got)
	}
}

func TestDurReadsAtEveryScale(t *testing.T) {
	for in, want := range map[time.Duration]string{
		30 * time.Second: "30s",
		90 * time.Second: "1m",
		2 * time.Hour:    "2h0m",
		90 * time.Minute: "1h30m",
		72 * time.Hour:   "3d",
		400 * time.Hour:  "16d",
	} {
		if got := dur(in); got != want {
			t.Errorf("dur(%s) = %q, want %q", in, got, want)
		}
	}
}
