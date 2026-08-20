// Package secrets loads the values that deliberately have no home in
// config.yaml. Everything here comes from the process environment, which is how
// it can stay out of git while config.yaml stays committed.
//
// The naming rule is that every variable is PZ_-prefixed, so an operator
// reading an Akash SDL can tell at a glance which entries are sensitive.
package secrets

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Role selects which secrets a process requires.
type Role string

const (
	RoleController Role = "controller"
	RoleAgent      Role = "agent"
)

// Redacted replaces every non-empty secret when rendering for review.
const Redacted = "__REDACTED__"

// Set holds every secret the system uses. A zero value is a valid "nothing
// loaded" state; Load reports which required entries were missing.
type Set struct {
	// DeployKeyB64 is the base64-encoded OpenSSH private key granting write
	// access to the pz-saves repository. Both roles need it while git is the
	// message bus.
	DeployKeyB64 string

	// Controller-only.
	AkashAPIKey        string
	WebhookSecret      string
	CloudflareAPIToken string

	// Shared HTTP credentials.
	StoragePassword     string // uploads and protected storage
	ServerFilesPassword string // /server.zip
	BackupsPassword     string // /backups/<file>

	// Game secrets. These never reach an Akash SDL or an image layer: the
	// controller substitutes them into server.zip as it serves it.
	RCONPassword  string
	AdminPassword string
	JoinPassword  string
}

// Requirements narrows which conditional secrets are mandatory. It mirrors the
// relevant config switches without this package importing config.
type Requirements struct {
	RCON bool // server.rcon.enabled
	DNS  bool // dns.enabled
	// JoinPassword mirrors game.password_protected. Requiring the join password
	// on a server configured to have none would refuse to start over a value
	// nothing reads; not requiring it on a protected server hands out a world
	// anyone can walk into. Note this is not game.open — PZ enforces Password=
	// independently of whether accounts are open.
	JoinPassword bool
}

type spec struct {
	env   string
	field func(*Set) *string
	// need reports whether this secret is mandatory for the given role.
	need func(Role, Requirements) bool
}

func always(roles ...Role) func(Role, Requirements) bool {
	return func(r Role, _ Requirements) bool {
		for _, want := range roles {
			if r == want {
				return true
			}
		}
		return false
	}
}

// DeployKeyEnv holds the git write credential. It is named separately because
// read-only tools ask for it on its own, without requiring the rest of a role's
// secrets to be present.
const DeployKeyEnv = "PZ_DEPLOY_KEY_B64"

// specs is the authoritative registry: the env var name, where it lands, and
// who must have it.
var specs = []spec{
	{DeployKeyEnv, func(s *Set) *string { return &s.DeployKeyB64 }, always(RoleController, RoleAgent)},

	{"PZ_AKASH_API_KEY", func(s *Set) *string { return &s.AkashAPIKey }, always(RoleController)},
	{"PZ_WEBHOOK_SECRET", func(s *Set) *string { return &s.WebhookSecret }, always(RoleController)},
	{"PZ_CLOUDFLARE_API_TOKEN", func(s *Set) *string { return &s.CloudflareAPIToken },
		func(r Role, q Requirements) bool { return r == RoleController && q.DNS }},

	{"PZ_STORAGE_PASSWORD", func(s *Set) *string { return &s.StoragePassword }, always(RoleController)},
	{"PZ_SERVER_FILES_PASSWORD", func(s *Set) *string { return &s.ServerFilesPassword }, always(RoleController, RoleAgent)},
	{"PZ_BACKUPS_PASSWORD", func(s *Set) *string { return &s.BackupsPassword }, always(RoleController, RoleAgent)},

	{"PZ_RCON_PASSWORD", func(s *Set) *string { return &s.RCONPassword },
		func(r Role, q Requirements) bool { return r == RoleController && q.RCON }},
	{"PZ_ADMIN_PASSWORD", func(s *Set) *string { return &s.AdminPassword }, always(RoleController)},
	{"PZ_JOIN_PASSWORD", func(s *Set) *string { return &s.JoinPassword },
		// The ini Password= field, substituted into server.zip alongside the
		// other game secrets rather than shipped in an SDL.
		func(r Role, q Requirements) bool { return r == RoleController && q.JoinPassword }},
}

// EnvNames lists every recognised variable, sorted, for help text and docs.
func EnvNames() []string {
	out := make([]string, 0, len(specs))
	for _, sp := range specs {
		out = append(out, sp.env)
	}
	sort.Strings(out)
	return out
}

// Load reads the environment. Missing optional secrets stay empty; missing
// required ones are reported together so the operator fixes the SDL once.
func Load(role Role, req Requirements) (*Set, error) {
	s := &Set{}
	var missing []string
	for _, sp := range specs {
		v := strings.TrimSpace(os.Getenv(sp.env))
		*sp.field(s) = v
		if v == "" && sp.need(role, req) {
			missing = append(missing, sp.env)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required secret(s) for role %s: %s", role, strings.Join(missing, ", "))
	}
	return s, nil
}

// LoadOptional reads the environment and requires nothing. Read-only tools use
// it so a diagnostic stays available on a machine that holds no secrets — and,
// more to the point, so `pzctl state show` still runs when the reason you are
// running it is that git auth is broken.
func LoadOptional() *Set {
	s := &Set{}
	for _, sp := range specs {
		*sp.field(s) = strings.TrimSpace(os.Getenv(sp.env))
	}
	return s
}

// DeployKeyPEM decodes DeployKeyB64 into the PEM bytes an SSH client wants. An
// empty value yields nil and no error, which is correct for a local path remote.
//
// Two tolerances are deliberate. Newlines inside the base64 are ignored, because
// a key pasted into a GitHub secret or a shell heredoc often acquires them. And a
// value that is already PEM is passed through, because writing the key straight
// into the variable is the obvious mistake to make and an unreadable
// "illegal base64 data" is a poor way to learn about it.
func (s *Set) DeployKeyPEM() ([]byte, error) {
	v := strings.TrimSpace(s.DeployKeyB64)
	if v == "" {
		return nil, nil
	}
	if strings.HasPrefix(v, "-----BEGIN") {
		return []byte(v + "\n"), nil
	}
	packed := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, v)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding} {
		if out, err := enc.DecodeString(packed); err == nil {
			return out, nil
		}
	}
	return nil, fmt.Errorf("%s is neither base64 nor PEM", DeployKeyEnv)
}

// ErrNoDeployKey is returned by RequireDeployKey when the variable is unset.
var ErrNoDeployKey = errors.New("no deploy key in the environment")

// RequireDeployKey is DeployKeyPEM for the paths that cannot proceed without a
// credential, so the failure names the variable to set rather than surfacing as a
// permission denied from the far end of an SSH handshake.
func (s *Set) RequireDeployKey() ([]byte, error) {
	pem, err := s.DeployKeyPEM()
	if err != nil {
		return nil, err
	}
	if len(pem) == 0 {
		return nil, fmt.Errorf("%w: set %s", ErrNoDeployKey, DeployKeyEnv)
	}
	return pem, nil
}

// Redact returns a copy in which every non-empty value is replaced by the
// Redacted placeholder. Rendering an SDL for review uses this so the output is
// safe to paste into a diff, a chat message or a ticket.
func (s *Set) Redact() *Set {
	out := &Set{}
	for _, sp := range specs {
		if *sp.field(s) != "" {
			*sp.field(out) = Redacted
		}
	}
	return out
}

// Placeholders returns a Set where every value is the Redacted placeholder,
// including ones absent from the environment. This is what `sdl render` uses
// without --with-secrets, so the rendered shape is complete and reviewable even
// on a machine that holds no secrets at all.
func Placeholders() *Set {
	out := &Set{}
	for _, sp := range specs {
		*sp.field(out) = Redacted
	}
	return out
}

// Env renders the set as SDL-ready KEY=value pairs, skipping empty values and
// preserving the registry order so diffs between renders stay stable.
func (s *Set) Env(role Role, req Requirements) [][2]string {
	var out [][2]string
	for _, sp := range specs {
		v := *sp.field(s)
		if v == "" {
			continue
		}
		if !sp.need(role, req) && !relevant(sp, role) {
			continue
		}
		out = append(out, [2]string{sp.env, v})
	}
	return out
}

// relevant reports whether a secret belongs in this role's environment even
// when it is not strictly mandatory — for example RCON_PASSWORD on a controller
// that has RCON disabled today but may enable it without a redeploy.
func relevant(sp spec, role Role) bool {
	switch role {
	case RoleAgent:
		// The agent's environment is kept deliberately minimal: it gets git
		// access and the two HTTP credentials it needs to fetch files. Game
		// secrets arrive inside server.zip instead, so they never appear in an
		// Akash manifest that a provider can read.
		switch sp.env {
		case "PZ_DEPLOY_KEY_B64", "PZ_SERVER_FILES_PASSWORD", "PZ_BACKUPS_PASSWORD":
			return true
		}
		return false
	case RoleController:
		// The controller holds everything.
		return true
	}
	return false
}
