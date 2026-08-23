package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
)

// The .ini is a shared file: the controller substitutes the passwords into it,
// an operator hand-edits sandbox settings in it, and the agent owns a fixed set
// of keys inside it. These tests pin that boundary, because crossing it in either
// direction is a data-loss bug — one way the passwords vanish, the other way
// config.yaml stops being the source of truth.

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	c := config.Defaults()
	c.Identity.ServerName = "vsrania"
	c.Identity.Timezone = "Europe/Prague"
	c.Game.Map = "Muldraugh, KY"
	c.Game.PublicName = "vsrania"
	c.Game.MaxPlayers = 24
	return c
}

func TestRenderServerINIKeepsWhatItDoesNotOwn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vsrania.ini")
	original := strings.Join([]string{
		"# hand-written header",
		"Password=lobbyword",
		"RCONPassword=rconword",
		"AdminPassword=adminword",
		"MaxPlayers=16",
		"",
		"; a comment with a semicolon",
		"ZombieConfig=hordes",
		"PublicName=vsrania",
	}, "\n") + "\n"
	writeFile(t, path, original)

	changed, err := renderServerINI(path, testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if !containsStr(changed, "MaxPlayers") {
		t.Errorf("changed = %v, want MaxPlayers among them", changed)
	}
	// PublicName already matched, so it is not drift and must not be reported.
	if containsStr(changed, "PublicName") {
		t.Errorf("changed = %v, want PublicName absent (it already matched)", changed)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)

	// The three passwords are the whole reason this function patches instead of
	// generating. Losing them locks every player out and the operator has no copy:
	// they only exist in the controller's secret store and in this file.
	for _, keep := range []string{
		"Password=lobbyword",
		"RCONPassword=rconword",
		"AdminPassword=adminword",
		"ZombieConfig=hordes",
		"# hand-written header",
		"; a comment with a semicolon",
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("%q was dropped from the rendered .ini:\n%s", keep, got)
		}
	}
	if !strings.Contains(got, "MaxPlayers=24") {
		t.Errorf("MaxPlayers was not set to 24:\n%s", got)
	}
	if strings.Contains(got, "MaxPlayers=16") {
		t.Errorf("the old MaxPlayers line survived:\n%s", got)
	}

	// Order is preserved so that a `git diff` of two boots shows the drift and
	// nothing else.
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if lines[0] != "# hand-written header" || lines[1] != "Password=lobbyword" {
		t.Errorf("the file was reordered; first lines are %q, %q", lines[0], lines[1])
	}
}

func TestRenderServerINIIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vsrania.ini")
	cfg := testConfig(t)

	first, err := renderServerINI(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing existed, so every owned key was appended.
	if len(first) != len(ownedINI(cfg)) {
		t.Fatalf("first render changed %d keys, want all %d", len(first), len(ownedINI(cfg)))
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	second, err := renderServerINI(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Errorf("second render reported %v as changed; a boot with no config change must be a no-op", second)
	}
	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(after) {
		t.Errorf("the file changed on the second render:\n%s\n---\n%s", after, again)
	}
}

func TestReadINIKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vsrania.ini")
	writeFile(t, path, strings.Join([]string{
		"# a comment",
		"AdminPassword = spaced out ",
		"RCONPassword=__RCON_PASSWORD__",
		"Password=",
		"NotAPair",
	}, "\n")+"\n")

	for _, tc := range []struct {
		key, want, why string
	}{
		{"AdminPassword", "spaced out", "surrounding whitespace is not part of the value"},
		{"RCONPassword", "", "an unsubstituted placeholder is not a password"},
		{"Password", "", "an empty value reads as absent"},
		{"NotAPair", "", "a line with no = has no value"},
		{"Missing", "", "a key the file does not have"},
	} {
		got, err := readINIKey(path, tc.key)
		if err != nil {
			t.Fatalf("%s: %v", tc.key, err)
		}
		if got != tc.want {
			t.Errorf("%s = %q, want %q (%s)", tc.key, got, tc.want, tc.why)
		}
	}

	// A missing file is how the very first boot looks. Not an error: the caller logs
	// that PZ starts without the flag and carries on.
	got, err := readINIKey(filepath.Join(dir, "absent.ini"), "AdminPassword")
	if err != nil || got != "" {
		t.Errorf("absent file: got %q, %v; want an empty value and no error", got, err)
	}
}

func TestRenderServerINIWritesRCONPortOnlyWhenEnabled(t *testing.T) {
	cfg := testConfig(t)
	if _, ok := ownedINI(cfg)["RCONPort"]; ok {
		t.Error("RCONPort is owned while RCON is disabled; that opens a port the SDL does not expose")
	}
	cfg.Server.RCON.Enabled = true
	cfg.Server.RCON.Port = 27015
	if got := ownedINI(cfg)["RCONPort"]; got != "27015" {
		t.Errorf("RCONPort = %q, want 27015 once RCON is enabled", got)
	}
}

func TestOwnedINICarriesThePortsFromTheServerSection(t *testing.T) {
	cfg := testConfig(t)
	cfg.Server.Ports.Game = 16271
	cfg.Server.Ports.UDP = 16272
	owned := ownedINI(cfg)
	// The .ini ports and the SDL's exposed ports come from one place. In v1 they
	// were separate literals in two files, so changing one silently produced a
	// server nobody could reach.
	if owned["DefaultPort"] != "16271" || owned["UDPPort"] != "16272" {
		t.Errorf("DefaultPort=%q UDPPort=%q, want 16271/16272", owned["DefaultPort"], owned["UDPPort"])
	}

	// Zero means "PZ binds one UDP socket", which has to leave the key unowned
	// rather than write UDPPort=0: nothing exposes it in the SDL either, and a 0
	// there would either be rejected or bind something arbitrary.
	cfg.Server.Ports.UDP = 0
	owned = ownedINI(cfg)
	if got, ok := owned["UDPPort"]; ok {
		t.Errorf("UDPPort = %q with ports.udp: 0; the key must stay unowned", got)
	}
	if owned["DefaultPort"] != "16271" {
		t.Errorf("DefaultPort=%q, want 16271 regardless of the second socket", owned["DefaultPort"])
	}
}

func TestSplitINI(t *testing.T) {
	for _, tc := range []struct {
		line, key, value string
		ok               bool
	}{
		{"MaxPlayers=32", "MaxPlayers", "32", true},
		{"  MaxPlayers = 32  ", "MaxPlayers", "32", true},
		{"Password=", "Password", "", true},
		{"Mods=a;b;c", "Mods", "a;b;c", true},
		{"# MaxPlayers=32", "", "", false},
		{"; MaxPlayers=32", "", "", false},
		{"", "", "", false},
		{"   ", "", "", false},
		{"no equals here", "", "", false},
		{"=orphan", "", "", false},
	} {
		key, value, ok := splitINI(tc.line)
		if ok != tc.ok || key != tc.key || value != tc.value {
			t.Errorf("splitINI(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.line, key, value, ok, tc.key, tc.value, tc.ok)
		}
	}
}

func TestPatchLaunchJSONReplacesTheHeapAndKeepsTheRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ProjectZomboid64.json")
	writeFile(t, path, `{
  "mainClass": "zombie/network/GameServer",
  "classpath": ["."],
  "vmArgs": ["-Xmx2048m", "-Xms1024m", "-Dzomboid.steam=1", "-XX:+UseZGC"]
}`)

	changed, err := patchLaunchJSON(path, "12288m", "12288m")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("patchLaunchJSON reported no change")
	}

	var doc map[string]any
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["mainClass"] != "zombie/network/GameServer" {
		t.Errorf("mainClass = %v, want it preserved", doc["mainClass"])
	}
	if _, ok := doc["classpath"]; !ok {
		t.Error("classpath was dropped")
	}

	var args []string
	for _, a := range doc["vmArgs"].([]any) {
		args = append(args, a.(string))
	}
	// The heap flags belong here and nowhere else: pzexe drops -Xmx from the
	// command line without an error, which is how v1 ran a 16Gi container on the
	// JVM's default heap.
	if !containsStr(args, "-Xmx12288m") || !containsStr(args, "-Xms12288m") {
		t.Errorf("vmArgs = %v, want the new heap flags", args)
	}
	for _, gone := range []string{"-Xmx2048m", "-Xms1024m", steamOn} {
		if containsStr(args, gone) {
			t.Errorf("vmArgs = %v, want %q removed", args, gone)
		}
	}
	if !containsStr(args, steamOff) {
		t.Errorf("vmArgs = %v, want %q — the server has no Steam credentials", args, steamOff)
	}
	if !containsStr(args, "-XX:+UseZGC") {
		t.Errorf("vmArgs = %v, want the unrelated JVM flag preserved", args)
	}
}

func TestPatchLaunchJSONAddsSteamOffWhenAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ProjectZomboid64.json")
	writeFile(t, path, `{"vmArgs": []}`)
	if _, err := patchLaunchJSON(path, "1g", ""); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if !strings.Contains(got, steamOff) || !strings.Contains(got, "-Xmx1g") {
		t.Errorf("vmArgs = %s, want steam off and the heap set", got)
	}
	if strings.Contains(got, "-Xms") {
		t.Errorf("vmArgs = %s, want no -Xms when memory_min is empty", got)
	}
}

func TestPatchLaunchJSONReportsNoChangeWhenAlreadyCorrect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ProjectZomboid64.json")
	writeFile(t, path, `{"vmArgs": ["-Dzomboid.steam=0"]}`)
	if _, err := patchLaunchJSON(path, "4g", "4g"); err != nil {
		t.Fatal(err)
	}
	// Second pass over an already-patched file: the boot log must stay quiet, and
	// an unnecessary rewrite of a file the JVM may be reading is worth avoiding.
	changed, err := patchLaunchJSON(path, "4g", "4g")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("patchLaunchJSON rewrote a file that was already correct")
	}
}

func TestPatchLaunchJSONRejectsBrokenJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ProjectZomboid64.json")
	writeFile(t, path, "{not json")
	if _, err := patchLaunchJSON(path, "4g", "4g"); err == nil {
		t.Fatal("patchLaunchJSON accepted a file that is not JSON")
	}
	// The original must still be there: boot continues on the image's defaults
	// rather than with a truncated launcher config.
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "{not json" {
		t.Fatalf("the file was modified: %q (%v)", body, err)
	}
}

func TestWriteFileAtomicLeavesNoTemporaries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vsrania.ini")
	if err := writeFileAtomic(path, []byte("MaxPlayers=32\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "vsrania.ini" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory holds %v, want only vsrania.ini", names)
	}
}
