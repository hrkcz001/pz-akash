// Package bootstrap makes config.yaml exist.
//
// A container starts knowing four things: which repository holds the
// configuration, which branch, which file, and where to keep the mirror. That is
// deliberately the whole of it — every other value, including the timezone, the
// branch layout, the ports and the pinned host keys, lives in the file this
// package goes and fetches. It is what keeps the Akash SDL stable across
// configuration changes: editing config.yaml and pushing is a restart, not a
// re-render and a redeploy.
//
// v1 had the opposite arrangement. Its deployment.yaml carried around forty
// environment variables, so changing a port or a schedule meant editing a
// manifest, re-rendering an SDL and updating a live lease — and the manifest was
// also where the passwords were, which is why they are in the git history.
//
// # The trust anchor
//
// The first fetch cannot verify the remote using host keys that live in the file
// it is fetching. github.com's public host keys are therefore compiled in here,
// and TestEmbeddedKnownHostsMatchTheConfig pins them against config.yaml's copy
// so the two cannot drift. They are public keys: publishing them is their
// purpose. The private deploy key still comes from the environment.
package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/gitbus"
	"github.com/hrkcz001/pz-akash/pzctl/internal/secrets"
)

// The environment a container is started with. internal/sdl renders all four.
const (
	EnvRepoURL   = "PZ_REPO_URL"
	EnvBranch    = "PZ_GIT_BRANCH"
	EnvPath      = "PZ_CONFIG_PATH"
	EnvMirrorDir = "PZ_BOOTSTRAP_DIR"
)

// GitHubKnownHosts pins github.com, from https://api.github.com/meta.
//
// Refresh with:
//
//	curl -s https://api.github.com/meta | jq -r '.ssh_keys[] | "github.com " + .'
//
// The alternative is StrictHostKeyChecking=no, which is what v1 used: it hands a
// repository write key to whatever answers on port 22.
const GitHubKnownHosts = `github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl
github.com ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBEmKSENjQEezOmxkZMy7opKgwFB9nkt5YRrYMjNuG5N87uRgg6CLrbo5wAdT/y6v0mKV0U2w0WZ2YB/++Tpockg=
github.com ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQCj7ndNxQowgcQnjshcLrqPEiiphnt+VTTvDP6mHBL9j1aNUkY4Ue1gvwnGLVlOhGeYrnZaMgRK6+PKCUXaDbC7qtbW8gIkhL7aGCsOr/C56SJMy/BCZfxd1nWzAOxSDPgVsmerOBYfNqltV9/hWCqBywINIR+5dIg6JTJ72pcEpEjcYgXkE2YEFXV1JHnsKgbLWNlhScqb2UmyRkQyytRLtL+38TGxkxCflmO+5Z8CSSNY7GidjMIZ7Q4zMjA2n1nGrlTDkzwDCsw+wqFPGQA179cnfGWOWRVruj16z6XyvxvjJwbz0wQZ75XK5tKSb7FNyeIEs4TT4jk+S4dhPeAUC5y+bDYirYgM4GC7uEnztnZyaVWQ7B381AK4Qdrwt51ZqExKbQpTUNn+EjqoTwvqNj4kqx5QUCI0ThS/YkOxJCXmPUWZbhjpCg56i+2aB6CmK2JGhn57K5mj0MNdBXA4/WnwH6XoPWJzK5Nyu2zB3nAZp+S5hpQs+p1vN1/wsjk=
`

// DefaultTimeout bounds the fetch. A container that cannot reach GitHub should
// fail visibly at start rather than hang where an operator reads it as a slow
// image pull.
const DefaultTimeout = 90 * time.Second

// Options describes one fetch. Zero fields fall back to the environment, so
// FromEnv followed by field overrides is the normal way to build one.
type Options struct {
	RepoURL string
	Branch  string
	// Path is the config file's path inside the repository.
	Path string
	// MirrorDir is the bare mirror. It is deliberately the same directory the
	// controller's or agent's bus will open afterwards, so the boot fetch warms
	// the mirror instead of being thrown away.
	MirrorDir string
	// Out is where the fetched bytes are written. The caller then loads that
	// path like any other config file, which keeps one code path and lets an
	// operator run `pzctl config validate -c` on the very bytes in play.
	Out string

	// DeployKeyB64 is the value of PZ_DEPLOY_KEY_B64, in any of the forms
	// secrets accepts. Empty is valid for a local path remote, and only for one.
	DeployKeyB64 string

	KnownHosts string
	Timeout    time.Duration
	Logf       func(format string, args ...any)
}

// Configured reports whether the environment names a repository to bootstrap
// from. A local run with a config file on disk does not, and must not: fetching
// would silently override the file the operator is editing.
func Configured() bool { return strings.TrimSpace(os.Getenv(EnvRepoURL)) != "" }

// FromEnv reads the four bootstrap variables and the deploy key.
func FromEnv() Options {
	return Options{
		RepoURL:      strings.TrimSpace(os.Getenv(EnvRepoURL)),
		Branch:       strings.TrimSpace(os.Getenv(EnvBranch)),
		Path:         strings.TrimSpace(os.Getenv(EnvPath)),
		MirrorDir:    strings.TrimSpace(os.Getenv(EnvMirrorDir)),
		DeployKeyB64: os.Getenv(secrets.DeployKeyEnv),
	}
}

// applyDefaults fills in what the environment did not say. Only MirrorDir has no
// safe default: it is where a bare repository is created, and guessing a path to
// create in someone's filesystem is worse than saying so.
func (o *Options) applyDefaults() error {
	if o.RepoURL == "" {
		return fmt.Errorf("bootstrap: %s is required", EnvRepoURL)
	}
	if o.Branch == "" {
		o.Branch = config.Defaults().Git.Branch
	}
	if o.Path == "" {
		o.Path = config.DefaultFileName
	}
	if o.Out == "" {
		o.Out = config.DefaultFileName
	}
	if o.MirrorDir == "" {
		return fmt.Errorf("bootstrap: %s is required (the bare mirror to fetch into)", EnvMirrorDir)
	}
	if o.KnownHosts == "" {
		o.KnownHosts = GitHubKnownHosts
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	return nil
}

// Fetch reads the config file out of the repository and writes it to o.Out,
// returning that path.
//
// The write is atomic, because the alternative is a truncated config file on
// disk after a crash mid-write — and the next start would then fail with a YAML
// error naming a line number, which is a long way from the actual cause.
func Fetch(ctx context.Context, o Options) (string, error) {
	if err := o.applyDefaults(); err != nil {
		return "", err
	}

	key, err := (&secrets.Set{DeployKeyB64: o.DeployKeyB64}).DeployKeyPEM()
	if err != nil {
		return "", fmt.Errorf("bootstrap: %w", err)
	}
	if len(key) == 0 && isRemoteURL(o.RepoURL) {
		return "", fmt.Errorf("bootstrap: %s is required to read %s from %s",
			secrets.DeployKeyEnv, o.Path, o.RepoURL)
	}

	repo, err := gitbus.Open(gitbus.Options{
		RepoURL:      o.RepoURL,
		CacheDir:     o.MirrorDir,
		KnownHosts:   o.KnownHosts,
		DeployKeyPEM: key,
		NetTimeout:   o.Timeout,
		Logf:         o.Logf,
	})
	if err != nil {
		return "", fmt.Errorf("bootstrap: %w", err)
	}

	fetchCtx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	if err := repo.Fetch(fetchCtx); err != nil {
		return "", fmt.Errorf("bootstrap: %w", err)
	}

	data, err := repo.ReadFile(o.Branch, o.Path)
	if err != nil {
		if errors.Is(err, gitbus.ErrNotFound) {
			// Named precisely, because there are two ways to get here and they
			// have different fixes: the branch has not been merged yet, or the
			// file is somewhere else in it.
			return "", fmt.Errorf("bootstrap: %s does not contain %s on branch %s",
				o.RepoURL, o.Path, o.Branch)
		}
		return "", fmt.Errorf("bootstrap: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return "", fmt.Errorf("bootstrap: %s on branch %s is empty", o.Path, o.Branch)
	}

	if err := writeAtomic(o.Out, data); err != nil {
		return "", fmt.Errorf("bootstrap: %w", err)
	}
	o.Logf("bootstrap: %s from %s@%s, %d bytes", o.Out, o.Path, o.Branch, len(data))
	return o.Out, nil
}

// isRemoteURL reports whether reaching the remote needs credentials. A path or a
// file:// URL is a local clone, which the tests use and which needs neither a
// deploy key nor a host key.
func isRemoteURL(u string) bool {
	switch {
	case strings.HasPrefix(u, "file://"):
		return false
	case strings.HasPrefix(u, "ssh://"), strings.HasPrefix(u, "git@"),
		strings.HasPrefix(u, "https://"), strings.HasPrefix(u, "http://"):
		return true
	}
	return false
}

func writeAtomic(path string, data []byte) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.yaml")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
