package dns

// What these tests are for: the game record is the one thing in this package a
// player sees, and the two ways to get it wrong — proxying it, or leaving a stale
// record behind next to the new one — both look like "the server is down" from the
// outside. The controller half is v1's behaviour, and the tests below are written
// against update_cloudflare.py rather than against the code.

import (
	"context"
	"strings"
	"testing"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
)

func TestSyncGameCreatesADNSOnlyRecord(t *testing.T) {
	f := newFakeCF(t)
	c := f.client(t, testZone())

	changes, err := c.SyncGame(context.Background(), "203.0.113.7")
	if err != nil {
		t.Fatalf("SyncGame: %v", err)
	}
	if len(changes) != 1 || changes[0].Action != Created {
		t.Fatalf("changes = %+v, want one Created", changes)
	}

	got := f.one("pz.vsrania.online")
	if got.Type != "A" || got.Content != "203.0.113.7" {
		t.Errorf("record = %+v, want an A to 203.0.113.7", got)
	}
	if got.Proxied {
		t.Error("the game record is proxied — every player would be sent to Cloudflare instead of the server")
	}
	if got.TTL != 60 {
		t.Errorf("ttl = %d, want dns.game_ttl (60): a long TTL outlives the lease it points at", got.TTL)
	}

	// The wire body, not the decoded record: an omitted `proxied` decodes to false
	// and would pass the check above while relying on Cloudflare's default.
	body := f.bodyOf("POST /zones/" + testZoneID + "/dns_records")
	if !strings.Contains(body, `"proxied":false`) {
		t.Errorf("the create body does not state proxied:false — %s", body)
	}
	if !strings.Contains(body, recordComment) {
		t.Errorf("the create body carries no comment marking the record as ours — %s", body)
	}
}

func TestSyncGameTakesAnIPv6Address(t *testing.T) {
	f := newFakeCF(t)
	c := f.client(t, testZone())
	if _, err := c.SyncGame(context.Background(), "2001:db8::1"); err != nil {
		t.Fatalf("SyncGame: %v", err)
	}
	if got := f.one("pz.vsrania.online"); got.Type != "AAAA" {
		t.Errorf("type = %q, want AAAA", got.Type)
	}
}

// TestSyncGameWritesNothingWhenNothingChanged: this runs on every deploy, and a
// zone whose history holds one write per redeploy is a zone where the write that
// mattered is invisible.
func TestSyncGameWritesNothingWhenNothingChanged(t *testing.T) {
	f := newFakeCF(t)
	c := f.client(t, testZone())
	ctx := context.Background()

	if _, err := c.SyncGame(ctx, "203.0.113.7"); err != nil {
		t.Fatalf("first SyncGame: %v", err)
	}
	f.reset()

	changes, err := c.SyncGame(ctx, "203.0.113.7")
	if err != nil {
		t.Fatalf("second SyncGame: %v", err)
	}
	if len(changes) != 1 || changes[0].Action != Unchanged {
		t.Fatalf("changes = %+v, want one Unchanged", changes)
	}
	if w := f.writes(); len(w) != 0 {
		t.Errorf("a no-op sync wrote to the zone: %v", w)
	}
}

func TestSyncGameMovesTheAddress(t *testing.T) {
	f := newFakeCF(t)
	c := f.client(t, testZone())
	ctx := context.Background()

	if _, err := c.SyncGame(ctx, "203.0.113.7"); err != nil {
		t.Fatalf("SyncGame: %v", err)
	}
	changes, err := c.SyncGame(ctx, "203.0.113.9")
	if err != nil {
		t.Fatalf("SyncGame: %v", err)
	}
	if len(changes) != 1 || changes[0].Action != Updated {
		t.Fatalf("changes = %+v, want one Updated", changes)
	}
	if got := f.one("pz.vsrania.online"); got.Content != "203.0.113.9" {
		t.Errorf("content = %q, want the new address", got.Content)
	}
}

// TestSyncGameRemovesStaleAddressRecords: a leftover AAAA or CNAME at the same name
// means some resolvers answer with the old address and some with the new one, which
// presents as "the server works for half the players".
func TestSyncGameRemovesStaleAddressRecords(t *testing.T) {
	f := newFakeCF(t)
	f.seed(record{Name: "pz.vsrania.online", Type: "AAAA", Content: "2001:db8::dead", TTL: 300})
	f.seed(record{Name: "pz.vsrania.online", Type: "CNAME", Content: "old.provider.example", TTL: 300})
	c := f.client(t, testZone())

	if _, err := c.SyncGame(context.Background(), "203.0.113.7"); err != nil {
		t.Fatalf("SyncGame: %v", err)
	}
	got := f.one("pz.vsrania.online")
	if got.Type != "A" || got.Content != "203.0.113.7" {
		t.Errorf("record = %+v, want only the new A", got)
	}
}

// TestSyncGameConvertsARecordOfTheWrongType: with only a CNAME there, the name is
// rewritten in place rather than deleted and recreated, so the name never resolves
// to nothing.
func TestSyncGameConvertsARecordOfTheWrongType(t *testing.T) {
	f := newFakeCF(t)
	id := f.seed(record{Name: "pz.vsrania.online", Type: "CNAME", Content: "old.example", TTL: 300})
	c := f.client(t, testZone())

	if _, err := c.SyncGame(context.Background(), "203.0.113.7"); err != nil {
		t.Fatalf("SyncGame: %v", err)
	}
	got := f.one("pz.vsrania.online")
	if got.ID != id {
		t.Errorf("record id = %q, want the existing %q rewritten in place", got.ID, id)
	}
	if got.Type != "A" {
		t.Errorf("type = %q, want A", got.Type)
	}
	for _, call := range f.log() {
		if strings.HasPrefix(call, "DELETE ") {
			t.Errorf("the name was deleted before being recreated: %v", f.log())
			break
		}
	}
}

func TestSyncGameStripsAPort(t *testing.T) {
	f := newFakeCF(t)
	c := f.client(t, testZone())
	// The endpoint the FSM publishes carries a port. Writing "203.0.113.7:16261"
	// into an A record is a name that resolves to nothing.
	if _, err := c.SyncGame(context.Background(), "203.0.113.7:16261"); err != nil {
		t.Fatalf("SyncGame: %v", err)
	}
	if got := f.one("pz.vsrania.online"); got.Content != "203.0.113.7" {
		t.Errorf("content = %q, want the address without the port", got.Content)
	}
}

func TestSyncGameRefusesAnEmptyAddress(t *testing.T) {
	f := newFakeCF(t)
	c := f.client(t, testZone())
	if _, err := c.SyncGame(context.Background(), "  "); err == nil {
		t.Fatal("an empty address was accepted")
	}
	if calls := f.log(); len(calls) != 0 {
		t.Errorf("the zone was called for an address we do not have: %v", calls)
	}
}

func TestSyncGameIsOffWithoutAGameRecord(t *testing.T) {
	f := newFakeCF(t)
	z := testZone()
	z.GameRecord = ""
	c := f.client(t, z)

	changes, err := c.SyncGame(context.Background(), "203.0.113.7")
	if err != nil || changes != nil {
		t.Fatalf("SyncGame = %+v, %v; want nothing at all", changes, err)
	}
	if calls := f.log(); len(calls) != 0 {
		t.Errorf("game_record is empty but the zone was called: %v", calls)
	}
}

func TestClearGame(t *testing.T) {
	f := newFakeCF(t)
	c := f.client(t, testZone())
	ctx := context.Background()

	if _, err := c.SyncGame(ctx, "203.0.113.7"); err != nil {
		t.Fatalf("SyncGame: %v", err)
	}
	changes, err := c.ClearGame(ctx)
	if err != nil {
		t.Fatalf("ClearGame: %v", err)
	}
	if len(changes) != 1 || changes[0].Action != Deleted {
		t.Fatalf("changes = %+v, want one Deleted", changes)
	}
	if got := f.byName("pz.vsrania.online"); len(got) != 0 {
		t.Errorf("the record survived: %+v", got)
	}

	// Clearing twice is how a controller that restarted mid-close behaves.
	if _, err := c.ClearGame(ctx); err != nil {
		t.Errorf("clearing an absent record failed: %v", err)
	}
}

// TestClearGameLeavesTheApexAlone guards the sharpest edge in this package: the
// deletion path is name-scoped, and a game_record of "" would make GameHost() the
// apex if it were built by concatenation.
func TestClearGameLeavesTheApexAlone(t *testing.T) {
	f := newFakeCF(t)
	f.seed(record{Name: "vsrania.online", Type: "CNAME", Content: "provider.example", Proxied: true, TTL: 1})
	z := testZone()
	z.GameRecord = ""
	c := f.client(t, z)

	if _, err := c.ClearGame(context.Background()); err != nil {
		t.Fatalf("ClearGame: %v", err)
	}
	if got := f.byName("vsrania.online"); len(got) != 1 {
		t.Fatalf("the apex holds %d records, want the one we seeded", len(got))
	}
}

func TestNewReturnsNothingWhenDNSIsDisabled(t *testing.T) {
	z := testZone()
	z.Enabled = false
	c, err := New(Options{Zone: z, Token: "t"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c != nil {
		t.Fatal("a disabled zone produced a client")
	}
	// Every method must be safe on that nil, or every caller needs the check.
	if got, err := c.SyncGame(context.Background(), "203.0.113.7"); err != nil || got != nil {
		t.Errorf("SyncGame on a nil client = %+v, %v", got, err)
	}
	if got, err := c.SyncController(context.Background(), "http://x.example"); err != nil || got != nil {
		t.Errorf("SyncController on a nil client = %+v, %v", got, err)
	}
	if got, err := c.ClearGame(context.Background()); err != nil || got != nil {
		t.Errorf("ClearGame on a nil client = %+v, %v", got, err)
	}
	if got := c.PublicURL(); got != "" {
		t.Errorf("PublicURL on a nil client = %q", got)
	}
}

func TestNewRequiresATokenAndAZone(t *testing.T) {
	cases := []struct {
		name  string
		mutef func(*config.DNS)
		token string
	}{
		{"no token", func(*config.DNS) {}, ""},
		{"no zone id", func(z *config.DNS) { z.ZoneID = "" }, "t"},
		{"no api base", func(z *config.DNS) { z.APIBase = "" }, "t"},
		{"an api base that is not a URL", func(z *config.DNS) { z.APIBase = "api.cloudflare.com" }, "t"},
		{"another provider", func(z *config.DNS) { z.Provider = "route53" }, "t"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			z := testZone()
			z.APIBase = "https://api.cloudflare.example"
			tc.mutef(&z)
			if _, err := New(Options{Zone: z, Token: tc.token}); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}
