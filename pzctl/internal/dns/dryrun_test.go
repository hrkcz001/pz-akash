package dns

// What these tests are for: --dry-run is the only safe way to point v2 at a zone v1
// has been managing, and its whole value is that the report is trustworthy. Two ways
// it could fail to be: it could write something, or — the one the live gate actually
// caught — it could report in a tense that reads exactly like a real run, so an
// operator walks away believing a record exists.

import (
	"context"
	"strings"
	"testing"
)

// TestDryRunPlansWithoutWriting covers all three verbs against a zone that already
// holds a record, because the interesting case is not the empty zone: it is the one
// where v1 left something behind and the question is what v2 would do to it.
func TestDryRunPlansWithoutWriting(t *testing.T) {
	cases := []struct {
		name string
		seed string // the address already at pz.vsrania.online, "" for none
		want Action
		verb string
	}{
		{"an empty zone", "", Created, "would create"},
		{"a record at the wrong address", "203.0.113.1", Updated, "would update"},
		{"a record already correct", "203.0.113.7", Unchanged, "unchanged"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeCF(t)
			if tc.seed != "" {
				f.seed(record{
					Name: "pz.vsrania.online", Type: "A", Content: tc.seed, TTL: 60,
				})
			}
			c := f.dryClient(t, testZone())

			changes, err := c.SyncGame(context.Background(), "203.0.113.7")
			if err != nil {
				t.Fatalf("SyncGame: %v", err)
			}
			if len(changes) != 1 {
				t.Fatalf("changes = %+v, want one", changes)
			}
			ch := changes[0]

			// The plan has to be the plan a real run would have made: a dry run that
			// says "unchanged" about a record it never compared is worse than no dry
			// run, because it is evidence.
			if ch.Action != tc.want {
				t.Errorf("action = %s, want %s", ch.Action, tc.want)
			}
			if !ch.Planned {
				t.Error("the change is not marked Planned, so it prints as something that happened")
			}
			if got := ch.String(); !strings.HasPrefix(got, tc.verb+" ") {
				t.Errorf("printed %q, want it to start with %q", got, tc.verb)
			}

			// And nothing was written. This is the assertion that makes the report
			// safe to run against the live zone.
			if w := f.writes(); len(w) != 0 {
				t.Errorf("a dry run wrote to the zone: %v", w)
			}
			if tc.seed != "" {
				if got := f.one("pz.vsrania.online").Content; got != tc.seed {
					t.Errorf("the existing record now holds %q, want %q untouched", got, tc.seed)
				}
			} else if got := f.byName("pz.vsrania.online"); len(got) != 0 {
				t.Errorf("a dry run created %d record(s): %+v", len(got), got)
			}
		})
	}
}

// TestDryRunPlansTheClear: clear-game is the destructive one, so it is the one an
// operator is most likely to rehearse first.
func TestDryRunPlansTheClear(t *testing.T) {
	f := newFakeCF(t)
	f.seed(record{Name: "pz.vsrania.online", Type: "A", Content: "203.0.113.7", TTL: 60})
	c := f.dryClient(t, testZone())

	changes, err := c.ClearGame(context.Background())
	if err != nil {
		t.Fatalf("ClearGame: %v", err)
	}
	if len(changes) != 1 || changes[0].Action != Deleted {
		t.Fatalf("changes = %+v, want one Deleted", changes)
	}
	if got := changes[0].String(); !strings.HasPrefix(got, "would delete ") {
		t.Errorf("printed %q, want it to start with \"would delete\"", got)
	}
	if w := f.writes(); len(w) != 0 {
		t.Errorf("a dry-run clear deleted something: %v", w)
	}
	if len(f.byName("pz.vsrania.online")) != 1 {
		t.Error("the record is gone after a dry-run clear")
	}
}

// TestRealRunReportsInThePastTense is the other half of the pair: Planned must be off
// when the write happened, or the fix above would have made every run look like a
// rehearsal.
func TestRealRunReportsInThePastTense(t *testing.T) {
	f := newFakeCF(t)
	c := f.client(t, testZone())

	changes, err := c.SyncGame(context.Background(), "203.0.113.7")
	if err != nil {
		t.Fatalf("SyncGame: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("changes = %+v, want one", changes)
	}
	if changes[0].Planned {
		t.Error("a real change is marked Planned")
	}
	if got := changes[0].String(); !strings.HasPrefix(got, "created ") {
		t.Errorf("printed %q, want it to start with \"created\"", got)
	}
}
