package gitbus

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
)

func TestFetchAndReadAgainstALocalRemote(t *testing.T) {
	t.Parallel()
	remote := seedRemote(t, liveish())
	r := openMirror(t, remote)

	head, err := r.Head("main")
	if err != nil {
		t.Fatalf("Head(main): %v", err)
	}
	if want := runGit(t, remote, "rev-parse", "main"); head != want {
		t.Fatalf("Head(main) = %s, git says %s", head, want)
	}

	body, err := r.ReadFile("main", "config.yaml")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got, want := string(body), liveish()["config.yaml"]; got != want {
		t.Fatalf("config.yaml = %q, want %q", got, want)
	}

	// A missing file and a missing branch are both fs.ErrNotExist, so callers can
	// use one check for "not written yet" whether state lives in git or on disk.
	for _, tc := range []struct{ branch, path string }{
		{"main", "no-such-file"},
		{"state/controller", "controller.json"},
	} {
		if _, err := r.ReadFile(tc.branch, tc.path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("ReadFile(%s:%s) error = %v, want fs.ErrNotExist", tc.branch, tc.path, err)
		}
	}
}

func TestListDirOnTheTriggersDirectory(t *testing.T) {
	t.Parallel()
	r := openMirror(t, seedRemote(t, liveish()))

	got, err := r.ListDir("main", "triggers")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	want := []string{"backup-please", "start"}
	if len(got) != len(want) {
		t.Fatalf("ListDir = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ListDir = %v, want %v (sorted)", got, want)
		}
	}

	// No triggers and no triggers directory mean the same thing to the FSM, so
	// neither is an error.
	for _, tc := range []struct{ branch, dir string }{
		{"main", "no-such-dir"},
		{"state/agent", "triggers"},
	} {
		got, err := r.ListDir(tc.branch, tc.dir)
		if err != nil || len(got) != 0 {
			t.Fatalf("ListDir(%s:%s) = %v, %v; want empty and no error", tc.branch, tc.dir, got, err)
		}
	}
}

func TestFetchOfAnEmptyRemoteIsNotAFailure(t *testing.T) {
	t.Parallel()
	// A repository whose state branches have never been published is the normal
	// first-boot condition. v1 treated the resulting git error as fatal and
	// exited, so the very first deploy needed a hand-seeded state file.
	r := openMirror(t, newBareRemote(t))
	if err := r.Fetch(t.Context()); err != nil {
		t.Fatalf("Fetch of an empty remote: %v", err)
	}
	if _, err := r.Head("main"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Head of a nonexistent branch = %v, want fs.ErrNotExist", err)
	}
}

func TestOpenRepointsAStaleMirror(t *testing.T) {
	t.Parallel()
	// A cache directory left over from a previous config must not keep talking to
	// the old remote. That failure is silent — state appears to save, to nobody.
	first := seedRemote(t, liveish())
	second := seedRemote(t, map[string]string{"config.yaml": "version: 2\n"})
	cache := t.TempDir()

	for _, remote := range []string{first, second} {
		r, err := Open(Options{
			RepoURL:  remote,
			CacheDir: cache,
			Location: prague(t),
		})
		if err != nil {
			t.Fatalf("Open(%s): %v", remote, err)
		}
		if err := r.Fetch(t.Context()); err != nil {
			t.Fatalf("Fetch(%s): %v", remote, err)
		}
		body, err := r.ReadFile("main", "config.yaml")
		if err != nil {
			t.Fatalf("ReadFile after repoint: %v", err)
		}
		want, _ := remoteFile(t, remote, "main", "config.yaml")
		if strings.TrimSpace(string(body)) != want {
			t.Fatalf("mirror served %q, remote %s holds %q", body, remote, want)
		}
	}
}
