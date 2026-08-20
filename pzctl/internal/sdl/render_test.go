package sdl

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/secrets"
)

// sdlDoc is a decoding target strict enough to prove the rendered document has
// the shape Akash expects, rather than merely being parseable YAML.
type sdlDoc struct {
	Version  string `yaml:"version"`
	Services map[string]struct {
		Image  string `yaml:"image"`
		Expose []struct {
			Port  int    `yaml:"port"`
			As    int    `yaml:"as"`
			Proto string `yaml:"proto"`
			To    []struct {
				Global bool   `yaml:"global"`
				IP     string `yaml:"ip"`
			} `yaml:"to"`
		} `yaml:"expose"`
		Env []string `yaml:"env"`
	} `yaml:"services"`
	Profiles struct {
		Compute map[string]struct {
			Resources struct {
				CPU struct {
					Units any `yaml:"units"`
				} `yaml:"cpu"`
				Memory struct {
					Size string `yaml:"size"`
				} `yaml:"memory"`
				Storage []struct {
					Size string `yaml:"size"`
				} `yaml:"storage"`
			} `yaml:"resources"`
		} `yaml:"compute"`
		Placement map[string]struct {
			Pricing map[string]struct {
				Denom  string `yaml:"denom"`
				Amount int    `yaml:"amount"`
			} `yaml:"pricing"`
		} `yaml:"placement"`
	} `yaml:"profiles"`
	Deployment map[string]map[string]struct {
		Profile string `yaml:"profile"`
		Count   int    `yaml:"count"`
	} `yaml:"deployment"`
	Endpoints map[string]struct {
		Kind string `yaml:"kind"`
	} `yaml:"endpoints"`
}

func loadCfg(t *testing.T) *config.Config {
	t.Helper()
	c, err := config.Load("../../config.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return c
}

func parse(t *testing.T, raw []byte) sdlDoc {
	t.Helper()
	var doc sdlDoc
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("rendered SDL is not valid or has unexpected keys: %v\n---\n%s", err, raw)
	}
	if doc.Version != "2.0" {
		t.Errorf("version = %q, want \"2.0\"", doc.Version)
	}
	return doc
}

func TestRenderControllerShape(t *testing.T) {
	cfg := loadCfg(t)
	raw, err := RenderController(Input{Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	doc := parse(t, raw)

	svc, ok := doc.Services[ControllerService]
	if !ok {
		t.Fatalf("no %q service in rendered SDL", ControllerService)
	}
	if svc.Image != cfg.ControllerImageRef() {
		t.Errorf("image = %q, want %q", svc.Image, cfg.ControllerImageRef())
	}
	// http_port and webhook_port, both shared endpoints: the controller has no
	// IP lease, so no expose entry may carry an ip.
	if len(svc.Expose) != 2 {
		t.Fatalf("want 2 exposed ports, got %d", len(svc.Expose))
	}
	for _, e := range svc.Expose {
		if e.To[0].IP != "" {
			t.Errorf("port %d has ip %q; the controller uses shared endpoints", e.Port, e.To[0].IP)
		}
	}
	if len(doc.Endpoints) != 0 {
		t.Errorf("controller SDL must not declare endpoints, got %v", doc.Endpoints)
	}
	if got := doc.Profiles.Placement[PlacementName].Pricing[ControllerService].Amount; got != cfg.Controller.PricingUAKT {
		t.Errorf("pricing amount = %d, want %d", got, cfg.Controller.PricingUAKT)
	}
	if got := doc.Deployment[ControllerService][PlacementName].Count; got != 1 {
		t.Errorf("count = %d, want 1", got)
	}
}

func TestRenderControllerFoldsWebhookOntoHTTPPort(t *testing.T) {
	cfg := loadCfg(t)
	cfg.Controller.WebhookPort = 0
	raw, err := RenderController(Input{Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	doc := parse(t, raw)
	if got := len(doc.Services[ControllerService].Expose); got != 1 {
		t.Errorf("want 1 exposed port when the webhook shares http_port, got %d", got)
	}
}

func TestRenderServerShape(t *testing.T) {
	cfg := loadCfg(t)
	raw, err := RenderServer(Input{Cfg: cfg, MaxUAKT: 69})
	if err != nil {
		t.Fatal(err)
	}
	doc := parse(t, raw)

	svc := doc.Services[ServerService]
	// With RCON and SSH off, only the two game ports are exposed. This is the
	// v2 reduction: v1 exposed four.
	if len(svc.Expose) != 2 {
		t.Fatalf("want 2 exposed ports with rcon and ssh disabled, got %d", len(svc.Expose))
	}
	for _, e := range svc.Expose {
		if e.Proto != "udp" {
			t.Errorf("port %d proto = %q, want udp", e.Port, e.Proto)
		}
		if e.To[0].IP != cfg.Server.IPName {
			t.Errorf("port %d ip = %q, want %q", e.Port, e.To[0].IP, cfg.Server.IPName)
		}
	}
	if doc.Endpoints[cfg.Server.IPName].Kind != "ip" {
		t.Errorf("endpoints.%s.kind = %q, want ip", cfg.Server.IPName, doc.Endpoints[cfg.Server.IPName].Kind)
	}
	if got := doc.Profiles.Placement[PlacementName].Pricing[ServerService].Amount; got != 69 {
		t.Errorf("pricing amount = %d, want the computed ceiling 69", got)
	}
}

func TestRenderServerFallsBackToPlaceholderPricing(t *testing.T) {
	cfg := loadCfg(t)
	raw, err := RenderServer(Input{Cfg: cfg}) // MaxUAKT unset
	if err != nil {
		t.Fatal(err)
	}
	doc := parse(t, raw)
	if got := doc.Profiles.Placement[PlacementName].Pricing[ServerService].Amount; got != cfg.Server.PricingUAKT {
		t.Errorf("pricing amount = %d, want the placeholder %d", got, cfg.Server.PricingUAKT)
	}
}

func TestRenderServerWithRCONAndSSHEnabled(t *testing.T) {
	cfg := loadCfg(t)
	cfg.Server.RCON.Enabled = true
	cfg.Server.SSH.Enabled = true
	raw, err := RenderServer(Input{Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	doc := parse(t, raw)
	svc := doc.Services[ServerService]
	if len(svc.Expose) != 4 {
		t.Fatalf("want 4 exposed ports, got %d", len(svc.Expose))
	}
	byPort := map[int]string{}
	for _, e := range svc.Expose {
		byPort[e.Port] = e.Proto
	}
	for port, proto := range map[int]string{
		cfg.Server.Ports.Game: "udp",
		cfg.Server.Ports.UDP:  "udp",
		cfg.Server.RCON.Port:  "tcp",
		cfg.Server.SSH.Port:   "tcp",
	} {
		if byPort[port] != proto {
			t.Errorf("port %d proto = %q, want %q", port, byPort[port], proto)
		}
	}
}

func TestRenderWithoutSecretsEmitsPlaceholdersOnly(t *testing.T) {
	cfg := loadCfg(t)
	for _, tc := range []struct {
		name   string
		render func(Input) ([]byte, error)
	}{
		{"controller", RenderController},
		{"server", RenderServer},
	} {
		raw, err := tc.render(Input{Cfg: cfg})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		doc := parse(t, raw)
		svcName := ControllerService
		if tc.name == "server" {
			svcName = ServerService
		}
		sawSecret := false
		for _, e := range doc.Services[svcName].Env {
			key, val, _ := strings.Cut(e, "=")
			if !isSecretEnv(key) {
				continue
			}
			sawSecret = true
			if val != secrets.Redacted {
				t.Errorf("%s: %s = %q, want the placeholder %q", tc.name, key, val, secrets.Redacted)
			}
		}
		if !sawSecret {
			t.Errorf("%s: no secret entries rendered at all; the SDL shape is incomplete", tc.name)
		}
	}
}

// The agent's environment is deliberately minimal. Game passwords arrive inside
// server.zip, and the Akash API key has no business on a provider's machine.
func TestServerEnvExcludesControllerOnlySecrets(t *testing.T) {
	cfg := loadCfg(t)
	cfg.Server.RCON.Enabled = true
	cfg.DNS.Enabled = true
	raw, err := RenderServer(Input{Cfg: cfg, Secrets: secrets.Placeholders()})
	if err != nil {
		t.Fatal(err)
	}
	doc := parse(t, raw)

	got := map[string]bool{}
	for _, e := range doc.Services[ServerService].Env {
		key, _, _ := strings.Cut(e, "=")
		got[key] = true
	}
	for _, forbidden := range []string{
		"PZ_AKASH_API_KEY",
		"PZ_WEBHOOK_SECRET",
		"PZ_CLOUDFLARE_API_TOKEN",
		"PZ_STORAGE_PASSWORD",
		"PZ_RCON_PASSWORD",
		"PZ_ADMIN_PASSWORD",
		"PZ_JOIN_PASSWORD",
	} {
		if got[forbidden] {
			t.Errorf("%s must not appear in the server SDL", forbidden)
		}
	}
	for _, required := range []string{
		"PZ_DEPLOY_KEY_B64",
		"PZ_SERVER_FILES_PASSWORD",
		"PZ_BACKUPS_PASSWORD",
	} {
		if !got[required] {
			t.Errorf("%s is missing from the server SDL", required)
		}
	}
}

func TestBootstrapEnvIsPresent(t *testing.T) {
	cfg := loadCfg(t)
	raw, err := RenderServer(Input{Cfg: cfg, ControllerURL: "https://vsrania.online"})
	if err != nil {
		t.Fatal(err)
	}
	doc := parse(t, raw)
	want := map[string]string{
		"PZ_ROLE":           string(secrets.RoleAgent),
		"PZ_REPO_URL":       cfg.Git.RepoURL,
		"PZ_GIT_BRANCH":     cfg.Git.Branch,
		"PZ_CONFIG_PATH":    config.DefaultFileName,
		"PZ_CONTROLLER_URL": "https://vsrania.online",
	}
	got := map[string]string{}
	for _, e := range doc.Services[ServerService].Env {
		k, v, _ := strings.Cut(e, "=")
		got[k] = v
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// Awkward secret values must survive the YAML round trip byte for byte; a
// password mangled by quoting rules is a failure that only shows up at runtime.
//
// Every value here is invented. A fixture copied from a live deployment is a
// committed secret no matter how ordinary the test around it looks.
func TestEnvScalarSurvivesYAMLRoundTrip(t *testing.T) {
	values := []string{
		"ExampleP4ss**",
		"plain-token-123",
		"has # hash",
		"has: colon space",
		`has "quotes" and \backslash`,
		"trailing-space ",
		"0123456789",
		"LS0tLS1CRUdJTiBPUEVOU1NIIFBSSVZBVEUgS0VZLS0tLS0K+/=",
	}
	for _, v := range values {
		doc := "env:\n  - " + envScalar("PZ_TEST", v) + "\n"
		var out struct {
			Env []string `yaml:"env"`
		}
		if err := yaml.Unmarshal([]byte(doc), &out); err != nil {
			t.Errorf("value %q produced invalid YAML: %v\n%s", v, err, doc)
			continue
		}
		if len(out.Env) != 1 {
			t.Errorf("value %q decoded to %d entries", v, len(out.Env))
			continue
		}
		if want := "PZ_TEST=" + v; out.Env[0] != want {
			t.Errorf("value %q round-tripped to %q, want %q", v, out.Env[0], want)
		}
	}
}

func TestMaxUAKTPerBlock(t *testing.T) {
	// 3 USD/day at 3 USD/AKT over 14400 blocks: 3e6 / (3 * 14400) = 69.44 -> 69.
	// Truncation matches the bash implementation's int().
	got, err := MaxUAKTPerBlock(3.0, 3.0, 14400)
	if err != nil {
		t.Fatal(err)
	}
	if got != 69 {
		t.Errorf("MaxUAKTPerBlock(3, 3, 14400) = %d, want 69", got)
	}

	for _, tc := range []struct {
		name      string
		usd, rate float64
		bpd       int
	}{
		{"zero usd", 0, 3.0, 14400},
		{"zero rate", 3.0, 0, 14400},
		{"zero blocks", 3.0, 3.0, 0},
		{"ceiling rounds to zero", 0.0001, 1000, 14400},
	} {
		if _, err := MaxUAKTPerBlock(tc.usd, tc.rate, tc.bpd); err == nil {
			t.Errorf("%s: want an error, got nil", tc.name)
		}
	}
}

func TestInitialDepositUSD(t *testing.T) {
	if got := InitialDepositUSD(3.0, 1, 1.2, 0.5); got != 3.6 {
		t.Errorf("InitialDepositUSD(3, 1, 1.2, 0.5) = %v, want 3.6", got)
	}
	// The floor protects against a deposit too small for Akash to accept.
	if got := InitialDepositUSD(0.1, 1, 1.2, 0.5); got != 0.5 {
		t.Errorf("InitialDepositUSD(0.1, 1, 1.2, 0.5) = %v, want the 0.5 floor", got)
	}
}

// The header tells the next reader how to reproduce the document. If it names a
// command that does not exist, the file looks hand-editable again.
func TestHeaderNamesTheReproducingCommand(t *testing.T) {
	cfg := loadCfg(t)
	for _, tc := range []struct {
		role   string
		render func(Input) ([]byte, error)
	}{
		{"controller", RenderController},
		{"server", RenderServer},
	} {
		raw, err := tc.render(Input{Cfg: cfg})
		if err != nil {
			t.Fatalf("%s: %v", tc.role, err)
		}
		want := "`pzctl sdl render " + tc.role + "`"
		if !strings.Contains(string(raw), want) {
			t.Errorf("%s SDL header does not contain %s", tc.role, want)
		}
	}
}

func isSecretEnv(key string) bool {
	for _, n := range secrets.EnvNames() {
		if n == key {
			return true
		}
	}
	return false
}
