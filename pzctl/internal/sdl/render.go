// Package sdl renders Akash SDL documents from config.yaml plus environment
// secrets.
//
// The bash system took the opposite approach: a checked-in deployment.yaml was
// mutated in place by token substitution and a regex over `amount:`. That is why
// secrets ended up committed and why a stale token could survive into a deploy.
// Here the SDL is a pure function of (config, secrets) and is never written to
// the repository at all.
package sdl

import (
	"bytes"
	"embed"
	"fmt"
	"strconv"
	"strings"
	"text/template"

	"github.com/hrkcz001/pz-akash/pzctl/internal/bootstrap"
	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/secrets"
)

//go:embed templates/*.tmpl
var templatesFS embed.FS

// PlacementName is the SDL placement key. Both deployments use the same name;
// it is local to each SDL document.
const PlacementName = "pz-placement"

// Service names as they appear in the SDL and therefore in Akash lease status.
const (
	ControllerService = "controller"
	ServerService     = "pz-server"
)

// Input is everything needed to render either SDL.
type Input struct {
	Cfg     *config.Config
	Secrets *secrets.Set

	// ControllerURL is the public base URL the agent should use. Empty means
	// "discover it from the controller state branch at boot", which is the
	// normal case: the URL is not known until the controller's own lease is up.
	ControllerURL string

	// MaxPricePerBlock is the bid ceiling for the server SDL, in
	// Cfg.Akash.Price.Denom, computed from Akash.Price.MaxUSDPerDay (and the live
	// AKT/USD rate, if that denomination needs one). Zero falls back to
	// Server.PricingAmount, the hand-deploy placeholder.
	MaxPricePerBlock int
}

type expose struct {
	Port  int
	As    int
	Proto string
	IP    string
}

type view struct {
	// Role is the `pzctl sdl render <role>` argument that reproduces this
	// document, quoted in the header. It is the CLI role ("controller",
	// "server"), not the secrets role — the agent's SDL is rendered by
	// `sdl render server`.
	Role          string
	Note          string
	ServiceName   string
	PlacementName string
	Image         string
	Exposes       []expose
	Env           []string
	CPU           string
	Memory        string
	Storage       string
	PricingAmount int
	PricingDenom  string
	IPLease       bool
	IPName        string
}

var tmpl = template.Must(template.ParseFS(templatesFS, "templates/*.tmpl"))

// RenderController produces the SDL for the controller deployment. The
// controller is deployed by hand, so its pricing comes straight from config.
func RenderController(in Input) ([]byte, error) {
	c := in.Cfg
	v := view{
		Role:          "controller",
		Note:          "SDL for the PZ CONTROLLER deployment.",
		ServiceName:   ControllerService,
		PlacementName: PlacementName,
		Image:         c.ControllerImageRef(),
		CPU:           c.Controller.Resources.CPU,
		Memory:        c.Controller.Resources.Memory,
		Storage:       c.Controller.Resources.Storage,
		PricingAmount: c.Controller.PricingAmount,
		PricingDenom:  c.Akash.Price.Denom,
	}

	// The controller uses shared endpoints: the provider assigns the external
	// ports, and the resulting public URL is discovered after the lease is up.
	v.Exposes = append(v.Exposes, expose{
		Port: c.Controller.HTTPPort, As: c.Controller.HTTPPort, Proto: "tcp",
	})
	if !c.WebhookOnHTTPPort() {
		v.Exposes = append(v.Exposes, expose{
			Port: c.Controller.WebhookPort, As: c.Controller.WebhookPort, Proto: "tcp",
		})
	}

	env := bootstrapEnv(c, secrets.RoleController)
	v.Env = append(env, secretEnv(in, secrets.RoleController)...)

	return execute(v)
}

// RenderServer produces the SDL for the PZ server deployment. The controller
// renders this at deploy time and POSTs it to the Akash API; it is never
// written to disk in the repository.
func RenderServer(in Input) ([]byte, error) {
	c := in.Cfg
	pricing := in.MaxPricePerBlock
	if pricing <= 0 {
		pricing = c.Server.PricingAmount
	}

	v := view{
		Role:          "server",
		Note:          "SDL for the PZ GAME SERVER deployment.",
		ServiceName:   ServerService,
		PlacementName: PlacementName,
		Image:         c.ServerImageRef(),
		CPU:           c.Server.Resources.CPU,
		Memory:        c.Server.Resources.Memory,
		Storage:       c.Server.Resources.Storage,
		PricingAmount: pricing,
		PricingDenom:  c.Akash.Price.Denom,
		IPLease:       c.Server.IPLease,
		IPName:        c.Server.IPName,
	}

	ip := ""
	if c.Server.IPLease {
		ip = c.Server.IPName
	}
	// Only the game port is unconditional. The second UDP socket is exposed only
	// when one is configured — server.ports.udp: 0 means PZ binds one, and on a
	// shared endpoint an expose nothing listens on still costs an arbitrary
	// external port that a player would have to be told about. RCON and SSH are
	// opt-in in v2: the agent drives saves through the PZ process it owns and
	// uploads its own backups, so neither port is needed for normal operation.
	v.Exposes = append(v.Exposes,
		expose{Port: c.Server.Ports.Game, As: c.Server.Ports.Game, Proto: "udp", IP: ip},
	)
	if c.Server.Ports.UDP > 0 {
		v.Exposes = append(v.Exposes, expose{
			Port: c.Server.Ports.UDP, As: c.Server.Ports.UDP, Proto: "udp", IP: ip,
		})
	}
	if c.Server.RCON.Enabled {
		v.Exposes = append(v.Exposes, expose{
			Port: c.Server.RCON.Port, As: c.Server.RCON.Port, Proto: "tcp", IP: ip,
		})
	}
	if c.Server.SSH.Enabled {
		v.Exposes = append(v.Exposes, expose{
			Port: c.Server.SSH.Port, As: c.Server.SSH.Port, Proto: "tcp", IP: ip,
		})
	}

	env := bootstrapEnv(c, secrets.RoleAgent)
	if in.ControllerURL != "" {
		env = append(env, envScalar("PZ_CONTROLLER_URL", in.ControllerURL))
	}
	v.Env = append(env, secretEnv(in, secrets.RoleAgent)...)

	return execute(v)
}

// bootstrapEnv is the minimum a container needs before it can read config.yaml:
// which repository to clone, which branch, which file, and where to keep the
// mirror. Everything else is read from the config once the clone succeeds, which
// is what keeps the SDL stable across configuration changes.
//
// The names come from internal/bootstrap, the package that reads them, so the two
// halves of the contract cannot drift apart without a compile error. PZ_ROLE is
// the exception: nothing reads it. Each image names its role in its own CMD
// (`pzctl controller`, `pzctl agent`) rather than dispatching on a variable, so
// this is here to say what a container is when someone reads the manifest or a
// provider's process list.
//
// The mirror directory is role-dependent because the two containers keep their
// mirrors in different places — and must, since each force-pushes its own state
// branch through it. Handing both the same path would be a local-run bug that
// only appears when a laptop runs the pair.
func bootstrapEnv(c *config.Config, role secrets.Role) []string {
	mirror := c.Git.CacheDir
	if role == secrets.RoleAgent {
		mirror = c.Agent.Paths.RepoCache
	}
	return []string{
		envScalar("PZ_ROLE", string(role)),
		envScalar(bootstrap.EnvRepoURL, c.Git.RepoURL),
		envScalar(bootstrap.EnvBranch, c.Git.Branch),
		envScalar(bootstrap.EnvPath, config.DefaultFileName),
		envScalar(bootstrap.EnvMirrorDir, mirror),
	}
}

func secretEnv(in Input, role secrets.Role) []string {
	set := in.Secrets
	if set == nil {
		set = secrets.Placeholders()
	}
	req := secrets.Requirements{
		RCON:         in.Cfg.Server.RCON.Enabled,
		DNS:          in.Cfg.DNS.Enabled,
		JoinPassword: in.Cfg.Game.PasswordProtected,
	}
	var out []string
	for _, kv := range set.Env(role, req) {
		out = append(out, envScalar(kv[0], kv[1]))
	}
	return out
}

func execute(v view) ([]byte, error) {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "service.sdl.tmpl", v); err != nil {
		return nil, fmt.Errorf("render %s SDL: %w", v.ServiceName, err)
	}
	return buf.Bytes(), nil
}

// envScalar formats one `KEY=value` entry as a YAML scalar, quoting only when
// the value would otherwise change meaning. Quoting selectively keeps rendered
// SDLs diffable against the hand-written ones they replace.
func envScalar(key, value string) string {
	s := key + "=" + value
	if needsQuoting(s) {
		// Go's quoting rules are a subset of YAML's double-quoted style for
		// every escape it emits (\" \\ \n \t \xNN \uNNNN), so this is safe.
		return strconv.Quote(s)
	}
	return s
}

func needsQuoting(s string) bool {
	if s == "" {
		return true
	}
	if s != strings.TrimSpace(s) {
		return true
	}
	// '#' starts a comment, ': ' ends an implicit key, and quotes or
	// backslashes would need escaping regardless.
	if strings.ContainsAny(s, "#\"'\\\n\r\t") {
		return true
	}
	if strings.Contains(s, ": ") || strings.HasSuffix(s, ":") {
		return true
	}
	return false
}
