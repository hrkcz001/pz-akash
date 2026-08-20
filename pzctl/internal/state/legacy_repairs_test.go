package state

import (
	"strings"
	"testing"
	"time"
)

// TestImportRepairsNameTheirFileWithoutADanglingColon covers the reporting path
// the live repository actually takes. server_info.json fails to parse as a whole,
// so its repair carries no field name; qualifying it unconditionally produced
// "server_info.json::", which reads like a truncated message rather than a
// whole-file failure.
func TestImportRepairsNameTheirFileWithoutADanglingColon(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		t.Fatal(err)
	}
	f := mapFetcher(map[string]string{
		// The live document, byte for byte.
		"server_info.json": `{"ip": "", "port": 2222, "game_port": 16261, "status": "stopping", "players_count": 0, "price_per_hour": , "price_per_day": }`,
		// And a file whose failure is one field, so the qualified form is exercised too.
		"controller_info.json": `{"storage_url": "https://example.invalid", "updated_at": "not a time"}`,
	})

	_, _, r := ImportLegacy(f, loc)
	if r.OK() {
		t.Fatal("the live corrupt document imported clean")
	}

	var whole, field bool
	for _, it := range r.Items {
		if strings.Contains(it.Field, "::") || strings.HasSuffix(it.Field, ":") {
			t.Errorf("repair field %q has a dangling colon", it.Field)
		}
		if it.Field == "server_info.json" {
			whole = true
		}
		if strings.HasPrefix(it.Field, "controller_info.json:") &&
			len(it.Field) > len("controller_info.json:") {
			field = true
		}
	}
	if !whole {
		t.Errorf("no repair is attributed to server_info.json as a whole: %s", r)
	}
	if !field {
		t.Errorf("no repair names a field inside controller_info.json: %s", r)
	}
	// The whole-file failure is what makes the document unusable, so it must be
	// fatal — the caller has to know it is holding defaults, not a reading.
	if !r.Fatal() {
		t.Errorf("a document that failed to parse was not reported as fatal: %s", r)
	}
}
