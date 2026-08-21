package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hrkcz001/pz-akash/pzctl/internal/bootstrap"
)

// clearBootEnv makes the environment say "workstation", so each case sets only
// the variable it is about. t.Setenv restores the previous value at the end.
func clearBootEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"PZ_CONFIG", bootstrap.EnvRepoURL, bootstrap.EnvBranch,
		bootstrap.EnvPath, bootstrap.EnvMirrorDir,
	} {
		t.Setenv(name, "")
	}
	// Not the package directory: config.Find looks for ./config.yaml, and a case
	// that is meant to find nothing must actually find nothing.
	t.Chdir(t.TempDir())
}

func writeConfig(t *testing.T, name, body string) string {
	t.Helper()
	if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(name)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// TestBootConfigNamedFileWinsOverTheRepository. -c and $PZ_CONFIG are an operator
// saying "this file". A container's environment variables are still set when a
// human runs a command inside it to debug something, and having their file
// replaced under them at that moment is exactly when it would hurt most.
func TestBootConfigNamedFileWinsOverTheRepository(t *testing.T) {
	for _, tc := range []struct {
		name     string
		explicit bool
	}{
		{"-c flag", true},
		{"PZ_CONFIG", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearBootEnv(t)
			writeConfig(t, "mine.yaml", "version: 1\n")
			// A repository that could not possibly be fetched: reaching it would
			// need a deploy key, so a fetch here fails loudly rather than quietly
			// succeeding and hiding the bug.
			t.Setenv(bootstrap.EnvRepoURL, "git@github.com:hrkcz001/pz-saves.git")

			explicit := ""
			if tc.explicit {
				explicit = "mine.yaml"
			} else {
				t.Setenv("PZ_CONFIG", "mine.yaml")
			}

			got, err := bootConfig(explicit)
			if err != nil {
				t.Fatalf("bootConfig: %v", err)
			}
			if got != "mine.yaml" {
				t.Errorf("bootConfig = %q, want mine.yaml", got)
			}
		})
	}
}

// TestBootConfigOnAWorkstationIsJustFind: no repository in the environment, so
// nothing about the boot path may change how a local run resolves its config.
func TestBootConfigOnAWorkstationIsJustFind(t *testing.T) {
	clearBootEnv(t)
	writeConfig(t, "config.yaml", "version: 1\n")

	got, err := bootConfig("")
	if err != nil {
		t.Fatalf("bootConfig: %v", err)
	}
	if got != "config.yaml" {
		t.Errorf("bootConfig = %q, want config.yaml", got)
	}
}

func TestBootConfigOnAWorkstationWithNoFileSaysSo(t *testing.T) {
	clearBootEnv(t)

	_, err := bootConfig("")
	if err == nil {
		t.Fatal("bootConfig succeeded with no config anywhere")
	}
	if !strings.Contains(err.Error(), "no config file found") {
		t.Errorf("error = %v, want config.Find's message", err)
	}
}

// TestBootConfigFetchFailureIsReportedNotMasked. With a repository named and
// nothing on disk, the error has to be the fetch's — "no config file found" would
// send whoever reads it looking for a missing file when the actual fault is a
// missing variable or an unreachable remote.
func TestBootConfigFetchFailureIsReportedNotMasked(t *testing.T) {
	clearBootEnv(t)
	t.Setenv(bootstrap.EnvRepoURL, "git@github.com:hrkcz001/pz-saves.git")

	_, err := bootConfig("")
	if err == nil {
		t.Fatal("bootConfig succeeded without a repository it could read")
	}
	if !strings.Contains(err.Error(), "bootstrap:") {
		t.Errorf("error = %v, want bootstrap's", err)
	}
	if strings.Contains(err.Error(), "no config file found") {
		t.Errorf("error = %v; the fetch failure was masked by config.Find's", err)
	}
}

// TestBootConfigFallsBackToTheLastBootsFile. A controller that cannot reach GitHub
// should come up with the configuration it already has: it can still serve the
// dashboard, and the next tick fetches again. Exiting instead leaves a funded
// lease with nothing answering on the domain.
func TestBootConfigFallsBackToTheLastBootsFile(t *testing.T) {
	clearBootEnv(t)
	writeConfig(t, "config.yaml", "version: 1 # last boot\n")
	t.Setenv(bootstrap.EnvRepoURL, "git@github.com:hrkcz001/pz-saves.git")

	got, err := bootConfig("")
	if err != nil {
		t.Fatalf("bootConfig: %v", err)
	}
	if got != "config.yaml" {
		t.Errorf("bootConfig = %q, want the fallback config.yaml", got)
	}
}
