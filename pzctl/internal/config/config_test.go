package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// realConfigPath is the authored config that ships to pz-saves. Keeping it
// under test is the gate that stops a schema change from silently invalidating
// the deployed configuration.
const realConfigPath = "../../config.yaml"

func TestRealConfigLoadsAndValidates(t *testing.T) {
	c, err := Load(realConfigPath)
	if err != nil {
		t.Fatalf("load %s: %v", realConfigPath, err)
	}

	// Spot-check a few values against the v1 system they were transcribed from,
	// so a careless edit to config.yaml does not quietly change a deploy.
	if c.Identity.ServerName != "vsrania" {
		t.Errorf("identity.server_name = %q, want vsrania", c.Identity.ServerName)
	}
	if got, want := c.Server.MemoryMax, "14336m"; got != want {
		t.Errorf("server.memory_max = %q, want %q", got, want)
	}
	if got, want := c.Akash.BlocksPerDay, 14400; got != want {
		t.Errorf("akash.blocks_per_day = %d, want %d", got, want)
	}
	if got, want := c.Backups.Interval.D(), time.Hour; got != want {
		t.Errorf("backups.interval = %v, want %v", got, want)
	}
	if c.Server.RCON.Enabled || c.Server.SSH.Enabled {
		t.Errorf("server.rcon.enabled=%v server.ssh.enabled=%v; both should be off by default in v2",
			c.Server.RCON.Enabled, c.Server.SSH.Enabled)
	}
}

// Backup filenames and every other wall-clock value are formatted in this
// location, so it is a functional requirement rather than a cosmetic one.
func TestRealConfigUsesPragueTime(t *testing.T) {
	c := mustLoadReal(t)
	if c.Identity.Timezone != "Europe/Prague" {
		t.Errorf("identity.timezone = %q, want Europe/Prague", c.Identity.Timezone)
	}
	loc := c.Location()
	if loc.String() != "Europe/Prague" {
		t.Fatalf("Location() = %q, want Europe/Prague", loc)
	}
	// Both sides of the DST boundary, to prove the zoneinfo database is really
	// embedded and not silently falling back to UTC.
	for _, tc := range []struct {
		utc    string
		offset int // seconds east of UTC
	}{
		{"2026-01-15T12:00:00Z", 3600}, // CET
		{"2026-08-15T12:00:00Z", 7200}, // CEST
	} {
		when, err := time.Parse(time.RFC3339, tc.utc)
		if err != nil {
			t.Fatal(err)
		}
		if _, got := when.In(loc).Zone(); got != tc.offset {
			t.Errorf("%s in Prague: offset %ds, want %ds", tc.utc, got, tc.offset)
		}
	}
}

// The schema must have no place to put a secret, which is what makes committing
// config.yaml safe. This asserts it structurally rather than by review.
func TestSchemaHasNoSecretFields(t *testing.T) {
	banned := []string{"password", "secret", "token", "apikey", "api_key", "privatekey", "key_b64"}
	var walk func(typ reflect.Type, path string)
	walk = func(typ reflect.Type, path string) {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			name := strings.ToLower(f.Name)
			for _, b := range banned {
				if !strings.Contains(name, strings.ReplaceAll(b, "_", "")) {
					continue
				}
				// The name is only half of it: a secret has to be storable. A bool
				// like PasswordProtected states a policy and cannot carry a value,
				// so gating on the type keeps this guard aimed at real leaks
				// instead of rejecting any field that mentions a password.
				if canHoldText(f.Type) {
					t.Errorf("%s.%s looks like a secret; secrets belong in internal/secrets, not config.yaml", path, f.Name)
				}
			}
			if f.Type.Kind() == reflect.Struct {
				walk(f.Type, path+"."+f.Name)
			}
		}
	}
	walk(reflect.TypeOf(Config{}), "Config")
}

// canHoldText reports whether a field could store a secret at all — a string, or
// a collection of them.
func canHoldText(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.String:
		return true
	case reflect.Slice, reflect.Array, reflect.Ptr:
		return canHoldText(t.Elem())
	case reflect.Map:
		return canHoldText(t.Key()) || canHoldText(t.Elem())
	default:
		return false
	}
}

// The type gate above is what decides whether the secret guard fires, so a bug
// here would silently disarm it.
func TestCanHoldText(t *testing.T) {
	for _, tc := range []struct {
		v    any
		want bool
	}{
		{"", true},
		{[]string{}, true},
		{map[string]string{}, true},
		{map[string]int{}, true}, // the key alone is enough to carry a value
		{new(string), true},
		{false, false},
		{0, false},
		{Duration(0), false},
		{[]int{}, false},
	} {
		if got := canHoldText(reflect.TypeOf(tc.v)); got != tc.want {
			t.Errorf("canHoldText(%T) = %v, want %v", tc.v, got, tc.want)
		}
	}
}

// A field without an explicit yaml tag gets whatever key yaml.v3 guesses, which
// silently diverges from the documented config file.
func TestEveryFieldHasAnExplicitYAMLTag(t *testing.T) {
	var walk func(typ reflect.Type, path string)
	walk = func(typ reflect.Type, path string) {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if tag, ok := f.Tag.Lookup("yaml"); !ok || tag == "" {
				t.Errorf("%s.%s has no yaml tag (yaml.v3 would guess %q)", path, f.Name, strings.ToLower(f.Name))
			}
			if f.Type.Kind() == reflect.Struct {
				walk(f.Type, path+"."+f.Name)
			}
		}
	}
	walk(reflect.TypeOf(Config{}), "Config")
}

func TestUnknownKeyIsAnError(t *testing.T) {
	_, err := Decode(strings.NewReader("version: 1\nidentity:\n  server_nmae: typo\n"))
	if err == nil {
		t.Fatal("expected an error for an unknown key, got nil")
	}
	if !strings.Contains(err.Error(), "server_nmae") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

func TestMultipleDocumentsRejected(t *testing.T) {
	_, err := Decode(strings.NewReader("version: 1\n---\nversion: 1\n"))
	if err == nil {
		t.Fatal("expected an error for a multi-document file, got nil")
	}
}

func TestDurationAcceptsBothForms(t *testing.T) {
	cases := []struct {
		yaml string
		want time.Duration
	}{
		{"1h", time.Hour},
		{"90s", 90 * time.Second},
		{"1h30m", 90 * time.Minute},
		{"3600", time.Hour}, // bare seconds, for pasting v1 *_SEC values
		{"0.5", 500 * time.Millisecond},
	}
	for _, tc := range cases {
		c, err := Decode(strings.NewReader("version: 1\nbackups:\n  interval: " + tc.yaml + "\n"))
		if err != nil {
			t.Errorf("interval: %s -> %v", tc.yaml, err)
			continue
		}
		if got := c.Backups.Interval.D(); got != tc.want {
			t.Errorf("interval: %s -> %v, want %v", tc.yaml, got, tc.want)
		}
	}

	if _, err := Decode(strings.NewReader("version: 1\nbackups:\n  interval: soon\n")); err == nil {
		t.Error("expected an error for interval: soon, got nil")
	}
}

// Omitted keys must keep their default rather than becoming a zero value; that
// asymmetry is what made the bash system's fallbacks unreliable.
func TestOmittedKeysKeepDefaults(t *testing.T) {
	c, err := Decode(strings.NewReader("version: 1\ncontroller:\n  http_port: 9000\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Controller.HTTPPort != 9000 {
		t.Errorf("http_port = %d, want 9000", c.Controller.HTTPPort)
	}
	if want := Defaults().Controller.WebhookPort; c.Controller.WebhookPort != want {
		t.Errorf("webhook_port = %d, want the default %d", c.Controller.WebhookPort, want)
	}
	if want := Defaults().Akash.BlocksPerDay; c.Akash.BlocksPerDay != want {
		t.Errorf("blocks_per_day = %d, want the default %d", c.Akash.BlocksPerDay, want)
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	c := Defaults()
	// Defaults alone are incomplete on purpose: values with no safe default
	// must be supplied.
	err := c.Validate()
	if err == nil {
		t.Fatal("bare defaults should not validate (repo_url, images and server_name are unset)")
	}
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("want *ValidationError, got %T", err)
	}
	if len(ve.Problems) < 4 {
		t.Errorf("want several problems reported together, got %d: %v", len(ve.Problems), ve.Problems)
	}
}

func TestValidateCatchesPortCollision(t *testing.T) {
	c := mustLoadReal(t)
	c.Server.RCON.Enabled = true
	c.Server.RCON.Port = c.Server.Ports.Game

	err := c.Validate()
	if err == nil {
		t.Fatal("expected a collision error when rcon.port equals ports.game")
	}
	if !strings.Contains(err.Error(), "already used by") {
		t.Errorf("error should explain the collision, got: %v", err)
	}
}

func TestValidateCatchesWebhookPortCollision(t *testing.T) {
	c := mustLoadReal(t)
	c.Controller.WebhookPort = c.Controller.HTTPPort
	if err := c.Validate(); err == nil {
		t.Fatal("expected an error when webhook_port equals http_port")
	}
}

func TestValidateRejectsCollapsedStateBranches(t *testing.T) {
	c := mustLoadReal(t)
	c.Git.AgentStateBranch = c.Git.ControllerStateBranch
	err := c.Validate()
	if err == nil {
		t.Fatal("expected an error when both state branches are the same")
	}
	if !strings.Contains(err.Error(), "single-writer") {
		t.Errorf("error should explain why it matters, got: %v", err)
	}
}

func TestValidateRejectsBadSizes(t *testing.T) {
	c := mustLoadReal(t)
	c.Server.Resources.Memory = "16GB" // Akash wants Gi, not GB
	c.Server.MemoryMax = "14336"       // JVM wants a unit suffix
	err := c.Validate()
	if err == nil {
		t.Fatal("expected errors for malformed sizes")
	}
	for _, want := range []string{"server.resources.memory", "server.memory_max"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s, got: %v", want, err)
		}
	}
}

func TestValidateRejectsUnknownCountryCode(t *testing.T) {
	c := mustLoadReal(t)
	c.Akash.Placement.Countries = []string{"PL", "Germany"}
	if err := c.Validate(); err == nil {
		t.Fatal("expected an error for a non-ISO country entry")
	}
}

func TestValidateRejectsDefaultLocaleOutsideLocales(t *testing.T) {
	c := mustLoadReal(t)
	c.Dashboard.DefaultLocale = "de"
	if err := c.Validate(); err == nil {
		t.Fatal("expected an error when default_locale is not in locales")
	}
}

// A misspelt restore_policy decides whether a start continues the world or
// replaces it, so it is rejected rather than defaulted — guessing on the
// operator's behalf is exactly the silence the key exists to remove.
func TestValidateRejectsUnknownRestorePolicy(t *testing.T) {
	c := mustLoadReal(t)
	for _, bad := range []string{"", "newest", "Latest", "auto"} {
		c.Backups.RestorePolicy = bad
		if err := c.Validate(); err == nil {
			t.Errorf("backups.restore_policy = %q was accepted", bad)
		}
	}
	for _, good := range []string{RestoreLatest, RestorePinned, RestoreNone} {
		c.Backups.RestorePolicy = good
		if err := c.Validate(); err != nil {
			t.Errorf("backups.restore_policy = %q was rejected: %v", good, err)
		}
	}
}

// The agent's filesystem layout and PZ launch details lived in the v1 Dockerfile
// and entrypoint.sh, nowhere in configuration. These assertions are the record
// that they were transcribed rather than reinvented: if a value here changes, the
// image it has to agree with changed too.
func TestRealConfigCarriesTheV1AgentLayout(t *testing.T) {
	a := mustLoadReal(t).Agent
	for _, tc := range []struct{ what, got, want string }{
		{"paths.home", a.Paths.Home, "/home/steam"},
		{"paths.game_dir", a.Paths.GameDir, "/home/steam/pz-server"},
		{"paths.data_dir", a.Paths.DataDir, "/home/steam/Zomboid"},
		{"paths.lowercase_link", a.Paths.LowercaseLink, "/home/steam/zomboid"},
		{"paths.log_file", a.Paths.LogFile, "/home/steam/server.log"},
		{"pz.ready_banner", a.PZ.ReadyBanner, "*** SERVER STARTED ***"},
		{"pz.save_command", a.PZ.SaveCommand, "save"},
		{"pz.quit_command", a.PZ.QuitCommand, "quit"},
		{"pz.players_command", a.PZ.PlayersCommand, "players"},
	} {
		if tc.got != tc.want {
			t.Errorf("agent.%s = %q, want %q", tc.what, tc.got, tc.want)
		}
	}
	if !reflect.DeepEqual(a.PZ.LaunchScripts, []string{"start-server.sh", "StartServer64.sh"}) {
		t.Errorf("agent.pz.launch_scripts = %v, want the two v1 launcher names", a.PZ.LaunchScripts)
	}
	// The lowercase link is the ext4 workaround, not a nicety: the game builds
	// some internal paths in ~/zomboid whatever -cachedir says.
	if strings.EqualFold(a.Paths.LowercaseLink, a.Paths.DataDir) && a.Paths.LowercaseLink == a.Paths.DataDir {
		t.Error("agent.paths.lowercase_link must differ from data_dir in case, or it links to itself")
	}
}

func TestValidateRejectsRelativeAgentPaths(t *testing.T) {
	c := mustLoadReal(t)
	c.Agent.Paths.DataDir = "Zomboid"
	err := c.Validate()
	if err == nil {
		t.Fatal("expected an error for a relative agent path")
	}
	if !strings.Contains(err.Error(), "agent.paths.data_dir") {
		t.Errorf("error should name the field, got: %v", err)
	}
}

// The agent empties work_dir on boot, so naming a directory that holds anything
// else is a data-loss bug rather than a style problem.
func TestValidateRejectsWorkDirOverlappingData(t *testing.T) {
	for _, tc := range []struct {
		what string
		set  func(*Config)
	}{
		{"data_dir", func(c *Config) { c.Agent.Paths.WorkDir = c.Agent.Paths.DataDir }},
		{"game_dir", func(c *Config) { c.Agent.Paths.WorkDir = c.Agent.Paths.GameDir }},
	} {
		c := mustLoadReal(t)
		tc.set(c)
		err := c.Validate()
		if err == nil {
			t.Errorf("work_dir = %s should be rejected", tc.what)
			continue
		}
		if !strings.Contains(err.Error(), "emptied on boot") {
			t.Errorf("error should explain why it matters, got: %v", err)
		}
	}
}

func TestValidateRejectsALowercaseLinkOntoItsTarget(t *testing.T) {
	c := mustLoadReal(t)
	c.Agent.Paths.LowercaseLink = c.Agent.Paths.DataDir
	if err := c.Validate(); err == nil {
		t.Fatal("expected an error when lowercase_link equals data_dir")
	}
	// Empty is the documented way to turn the link off, and must stay legal.
	c = mustLoadReal(t)
	c.Agent.Paths.LowercaseLink = ""
	if err := c.Validate(); err != nil {
		t.Errorf("an empty lowercase_link disables the link and must validate: %v", err)
	}
}

func TestValidateRejectsALauncherPath(t *testing.T) {
	c := mustLoadReal(t)
	// game_dir is searched for the name, so a path here would never match.
	c.Agent.PZ.LaunchScripts = []string{"bin/start-server.sh"}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected an error for a launcher given as a path")
	}
	if !strings.Contains(err.Error(), "not a path") {
		t.Errorf("error should say why, got: %v", err)
	}
}

// Each of these three is what stops one of the reported bugs from being possible
// to reintroduce by deleting a config line.
func TestValidateRequiresTheFieldsThatFixBugs(t *testing.T) {
	for _, tc := range []struct {
		field string
		set   func(*Config)
	}{
		{"agent.pz.ready_banner", func(c *Config) { c.Agent.PZ.ReadyBanner = "" }},
		{"agent.pz.players_command", func(c *Config) { c.Agent.PZ.PlayersCommand = "" }},
		{"agent.pz.quit_command", func(c *Config) { c.Agent.PZ.QuitCommand = "" }},
		{"agent.pz.save_command", func(c *Config) { c.Agent.PZ.SaveCommand = "" }},
	} {
		c := mustLoadReal(t)
		tc.set(c)
		err := c.Validate()
		if err == nil {
			t.Errorf("%s: empty should be rejected", tc.field)
			continue
		}
		if !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: error should name the field, got: %v", tc.field, err)
		}
	}
}

func TestMarshalRoundTrips(t *testing.T) {
	c := mustLoadReal(t)
	out, err := c.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Decode(strings.NewReader(string(out)))
	if err != nil {
		t.Fatalf("re-decoding a dumped config failed, so `config dump` output is not loadable: %v", err)
	}
	if !reflect.DeepEqual(c, back) {
		t.Error("round trip changed the config")
	}
}

func mustLoadReal(t *testing.T) *Config {
	t.Helper()
	c, err := Load(realConfigPath)
	if err != nil {
		t.Fatalf("load %s: %v", realConfigPath, err)
	}
	return c
}
