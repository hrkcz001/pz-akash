package bootstrap

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/gitbus"
	"github.com/hrkcz001/pz-akash/pzctl/internal/secrets"
)

// realConfigPath is the config this repository ships. The bootstrap tests fetch
// that exact file through a local git remote, so the round trip is over the real
// bytes an operator edits rather than a two-line fixture.
const realConfigPath = "../../config.yaml"

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid",
		"GIT_AUTHOR_DATE=2026-08-20T21:14:07+02:00",
		"GIT_COMMITTER_DATE=2026-08-20T21:14:07+02:00",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

// seedRemote builds a bare repository whose branch holds files, and returns its
// path. The path is used verbatim as the remote URL: go-git routes a native
// filesystem path to the file transport, whereas a forward-slashed one on Windows
// reads as an scp-style `host:path` and would be attempted over SSH.
func seedRemote(t *testing.T, branch string, files map[string]string) string {
	t.Helper()
	requireGit(t)

	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, "", "init", "--bare", "--initial-branch="+branch, remote)

	work := t.TempDir()
	runGit(t, work, "init", "--initial-branch="+branch)
	for path, body := range files {
		full := filepath.Join(work, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, work, "add", "-A")
	runGit(t, work, "-c", "user.name=test", "-c", "user.email=test@example.invalid",
		"commit", "-m", "seed")
	runGit(t, work, "remote", "add", "origin", remote)
	runGit(t, work, "push", "origin", branch)
	return remote
}

func realConfigBytes(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(realConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// opts builds an Options for a local remote, with the two paths under the test's
// own directory. Everything else is left zero so the defaults are what run.
func opts(t *testing.T, remote string) Options {
	t.Helper()
	dir := t.TempDir()
	return Options{
		RepoURL:   remote,
		MirrorDir: filepath.Join(dir, "mirror.git"),
		Out:       filepath.Join(dir, "config.yaml"),
		Logf:      func(string, ...any) {},
	}
}

// TestFetchWritesAConfigTheLoaderAccepts is the whole point of the package: a
// container that starts with four environment variables and nothing on disk ends
// up with a config file its own loader validates.
func TestFetchWritesAConfigTheLoaderAccepts(t *testing.T) {
	want := realConfigBytes(t)
	remote := seedRemote(t, "main", map[string]string{
		"config.yaml": want,
		"README.md":   "operator notes\n",
	})

	o := opts(t, remote)
	out, err := Fetch(t.Context(), o)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if out != o.Out {
		t.Errorf("Fetch returned %q, wrote %q", out, o.Out)
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("fetched config differs from the remote's copy (%d vs %d bytes)",
			len(got), len(want))
	}

	// Not just "a file appeared": the bytes have to satisfy Validate, because a
	// container that writes an unloadable config has not bootstrapped.
	c, err := config.Load(out)
	if err != nil {
		t.Fatalf("config.Load on the fetched file: %v", err)
	}
	if c.Version != config.SchemaVersion {
		t.Errorf("version = %d, want %d", c.Version, config.SchemaVersion)
	}
}

// TestFetchDefaultsBranchAndPath covers the case the SDL will actually produce
// for a default deployment: PZ_GIT_BRANCH and PZ_CONFIG_PATH unset, so the
// defaults have to name main and config.yaml.
func TestFetchDefaultsBranchAndPath(t *testing.T) {
	if got := config.Defaults().Git.Branch; got != "main" {
		t.Fatalf("default branch is %q; this test assumes main", got)
	}
	remote := seedRemote(t, "main", map[string]string{"config.yaml": realConfigBytes(t)})

	o := opts(t, remote)
	o.Branch = ""
	o.Path = ""
	if _, err := Fetch(t.Context(), o); err != nil {
		t.Fatalf("Fetch with defaults: %v", err)
	}
}

// TestFetchWarmsTheMirrorTheBusReuses pins the claim in Options.MirrorDir: the
// boot fetch is not thrown away. Opening a bus on the same directory afterwards
// reads the branch with no network operation of its own.
func TestFetchWarmsTheMirrorTheBusReuses(t *testing.T) {
	remote := seedRemote(t, "main", map[string]string{"config.yaml": realConfigBytes(t)})

	o := opts(t, remote)
	if _, err := Fetch(t.Context(), o); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	repo, err := gitbus.Open(gitbus.Options{
		RepoURL:  remote,
		CacheDir: o.MirrorDir,
		Logf:     func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("reopen the mirror: %v", err)
	}
	// No Fetch call here — if the mirror were cold this would be ErrNotFound.
	if _, err := repo.ReadFile("main", "config.yaml"); err != nil {
		t.Fatalf("read from the warmed mirror: %v", err)
	}
}

// TestFetchCreatesTheParentAndLeavesNoTemporary. The atomic write is the reason
// this is worth a test: a crash mid-write must not leave a half-written config
// behind, and the rename must not leave the temporary file behind either.
func TestFetchCreatesTheParentAndLeavesNoTemporary(t *testing.T) {
	remote := seedRemote(t, "main", map[string]string{"config.yaml": realConfigBytes(t)})

	o := opts(t, remote)
	dir := filepath.Join(filepath.Dir(o.Out), "etc", "pzctl")
	o.Out = filepath.Join(dir, "config.yaml")

	if _, err := Fetch(t.Context(), o); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "config.yaml" {
		t.Errorf("%s holds %v, want just config.yaml", dir, names)
	}
}

// TestFetchOverwritesAStaleConfig. A restarted container has last boot's file on
// disk, and the config may have changed since. Leaving the old one in place would
// make the restart a no-op, which is the opposite of how config is meant to ship.
func TestFetchOverwritesAStaleConfig(t *testing.T) {
	want := realConfigBytes(t)
	remote := seedRemote(t, "main", map[string]string{"config.yaml": want})

	o := opts(t, remote)
	if err := os.WriteFile(o.Out, []byte("version: 1 # last boot's copy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Fetch(t.Context(), o); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got, err := os.ReadFile(o.Out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Error("the stale config was left in place")
	}
}

func TestFetchErrors(t *testing.T) {
	missing := seedRemote(t, "main", map[string]string{"README.md": "no config here\n"})
	empty := seedRemote(t, "main", map[string]string{"config.yaml": "  \n\n"})
	good := seedRemote(t, "main", map[string]string{"config.yaml": realConfigBytes(t)})

	cases := []struct {
		name string
		// mutate is applied to a valid local-remote Options.
		mutate func(*Options)
		// want is a substring of the error. Errors here are read by whoever is
		// looking at a container's first ten log lines, so each one has to name
		// the thing to fix.
		want []string
	}{
		{
			name:   "no repository",
			mutate: func(o *Options) { o.RepoURL = "" },
			want:   []string{EnvRepoURL},
		},
		{
			name:   "no mirror directory",
			mutate: func(o *Options) { o.MirrorDir = "" },
			want:   []string{EnvMirrorDir},
		},
		{
			// The one that matters in production: a real remote with no key
			// fails here, before any connection, naming the variable to set.
			name: "remote with no deploy key",
			mutate: func(o *Options) {
				o.RepoURL = "git@github.com:hrkcz001/pz-saves.git"
			},
			want: []string{secrets.DeployKeyEnv, "config.yaml", "pz-saves"},
		},
		{
			name:   "unparseable deploy key",
			mutate: func(o *Options) { o.DeployKeyB64 = "not a key" },
			want:   []string{"bootstrap:"},
		},
		{
			name:   "file is not in the repository",
			mutate: func(o *Options) { o.RepoURL = missing },
			want:   []string{"does not contain config.yaml", "main"},
		},
		{
			name:   "branch does not exist",
			mutate: func(o *Options) { o.Branch = "v2" },
			want:   []string{"v2"},
		},
		{
			name:   "config is empty",
			mutate: func(o *Options) { o.RepoURL = empty },
			want:   []string{"empty"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := opts(t, good)
			tc.mutate(&o)

			_, err := Fetch(t.Context(), o)
			if err == nil {
				t.Fatal("Fetch succeeded")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
			if _, statErr := os.Stat(o.Out); statErr == nil {
				t.Error("a failed Fetch wrote a config file anyway")
			}
		})
	}
}

// TestEmbeddedKnownHostsMatchTheConfig is the drift pin promised in the package
// doc. The embedded copy is the trust anchor for the first fetch and config's is
// what every fetch after it uses; two copies of the same keys that are allowed to
// disagree would mean a rotation fixes one path and breaks the other.
func TestEmbeddedKnownHostsMatchTheConfig(t *testing.T) {
	c, err := config.Load(realConfigPath)
	if err != nil {
		t.Fatalf("load %s: %v", realConfigPath, err)
	}
	got := strings.TrimSpace(GitHubKnownHosts)
	want := strings.TrimSpace(c.Git.KnownHosts)
	if got != want {
		t.Errorf("GitHubKnownHosts and %s's git.known_hosts have drifted.\nembedded:\n%s\nconfig:\n%s",
			realConfigPath, got, want)
	}
	if lines := strings.Count(want, "\n") + 1; lines != 3 {
		t.Errorf("%d host key lines, want 3 (ed25519, ecdsa, rsa)", lines)
	}
}

// TestConfiguredFollowsTheRepositoryURL. Configured is what keeps a local run
// safe: `pzctl controller` on a workstation with an edited config.yaml must not
// have that file replaced by whatever is on the branch.
func TestConfiguredFollowsTheRepositoryURL(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false},
		{"   ", false},
		{"git@github.com:hrkcz001/pz-saves.git", true},
	} {
		t.Setenv(EnvRepoURL, tc.value)
		if got := Configured(); got != tc.want {
			t.Errorf("Configured() with %s=%q = %v, want %v", EnvRepoURL, tc.value, got, tc.want)
		}
	}
}

func TestFromEnvReadsTheBootstrapVariables(t *testing.T) {
	t.Setenv(EnvRepoURL, "  git@github.com:hrkcz001/pz-saves.git  ")
	t.Setenv(EnvBranch, "v2")
	t.Setenv(EnvPath, "deploy/config.yaml")
	t.Setenv(EnvMirrorDir, "/data/repo")
	t.Setenv(secrets.DeployKeyEnv, "a2V5")

	got := FromEnv()
	// Field by field rather than a struct compare: Options carries a Logf, which
	// makes it uncomparable.
	for _, f := range []struct {
		name, got, want string
	}{
		{EnvRepoURL, got.RepoURL, "git@github.com:hrkcz001/pz-saves.git"},
		{EnvBranch, got.Branch, "v2"},
		{EnvPath, got.Path, "deploy/config.yaml"},
		{EnvMirrorDir, got.MirrorDir, "/data/repo"},
		{secrets.DeployKeyEnv, got.DeployKeyB64, "a2V5"},
	} {
		if f.got != f.want {
			t.Errorf("FromEnv() %s = %q, want %q", f.name, f.got, f.want)
		}
	}
}
