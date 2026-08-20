package gitbus

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The tests in this package use the real git binary to build and inspect the
// remote. That is deliberate on two counts: go-git's file transport shells out to
// git-upload-pack anyway, so git is already a hard dependency here; and it makes
// git an independent oracle. Our tree writer has to produce objects git itself
// accepts — `git fsck --strict` catches an unsorted tree, which go-git would
// happily read back.

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

// git runs a git command in dir and fails the test on error.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid",
		"GIT_AUTHOR_DATE=2026-08-19T01:36:23+02:00",
		"GIT_COMMITTER_DATE=2026-08-19T01:36:23+02:00",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitOut is git for commands expected to fail sometimes.
func tryGit(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// newBareRemote creates an empty bare repository and returns its path. The path
// is used verbatim as the remote URL: go-git's endpoint parser routes a native
// filesystem path to the file transport, whereas a forward-slashed one on Windows
// looks like an scp-style `host:path` and would be treated as SSH.
func newBareRemote(t *testing.T) string {
	t.Helper()
	requireGit(t)
	path := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, "", "init", "--bare", "--initial-branch=main", path)
	return path
}

// seedRemote creates a bare remote whose main branch holds files.
func seedRemote(t *testing.T, files map[string]string) string {
	t.Helper()
	remote := newBareRemote(t)
	work := t.TempDir()
	runGit(t, work, "init", "--initial-branch=main")
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
	runGit(t, work, "push", "origin", "main")
	return remote
}

// liveish is the shape of the real pz-saves main branch: a config file and a
// couple of pending triggers.
func liveish() map[string]string {
	return map[string]string{
		"config.yaml":            "version: 1\nidentity:\n  timezone: Europe/Prague\n",
		"triggers/start":         "",
		"triggers/backup-please": "reason: before the update\n",
		"README.md":              "operator notes\n",
	}
}

func prague(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		t.Fatalf("Europe/Prague: %v", err)
	}
	return loc
}

// openMirror opens a Repo against remote and fetches it once.
func openMirror(t *testing.T, remote string) *Repo {
	t.Helper()
	r, err := Open(Options{
		RepoURL:   remote,
		CacheDir:  filepath.Join(t.TempDir(), "mirror.git"),
		UserName:  "pzctl",
		UserEmail: "pzctl@example.invalid",
		Location:  prague(t),
		Logf:      func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := r.Fetch(t.Context()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	return r
}

func testBranches() Branches {
	return Branches{
		Main:        "main",
		Controller:  "state/controller",
		Agent:       "state/agent",
		TriggersDir: "triggers",
	}
}

// remoteFile reads a path from a branch of the bare remote, using git rather than
// our own reader, so a test cannot pass by being wrong in both directions.
func remoteFile(t *testing.T, remote, branch, path string) (string, bool) {
	t.Helper()
	out, err := tryGit(t, remote, "cat-file", "-p", branch+":"+path)
	if err != nil {
		return "", false
	}
	return out, true
}

// fsck asserts every object we wrote is one git considers well-formed. This is
// the check that catches tree entries in the wrong order: go-git reads such a
// tree back happily, and only git notices.
func fsck(t *testing.T, remote string) {
	t.Helper()
	out, err := tryGit(t, remote, "fsck", "--strict", "--no-progress")
	if err != nil {
		t.Fatalf("git fsck rejected our objects: %v\n%s", err, out)
	}
	// fsck exits 0 for dangling objects, which are expected: an orphan commit
	// replaced by the next publish leaves its predecessor unreferenced.
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "dangling ") ||
			strings.HasPrefix(line, "Checking ") || strings.HasPrefix(line, "notice:") {
			continue
		}
		t.Fatalf("git fsck complained: %s", line)
	}
}
