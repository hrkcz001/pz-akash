package fsm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
)

// guideHarness is a machine whose guide is mirrored into a scratch directory, with
// two locales configured and neither of them present in the repository yet.
func guideHarness(t *testing.T) (*harness, string) {
	t.Helper()
	dir := t.TempDir()
	h := newHarness(t, func(c *config.Config) {
		c.Controller.Storage.PackagesDir = dir
		c.Dashboard.GuideFile = "README.{lang}.md"
		c.Dashboard.Locales = []string{"ru", "en"}
	})
	return h, dir
}

func guideBody(t *testing.T, dir, name string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		if !os.IsNotExist(err) {
			t.Fatalf("read mirrored %s: %v", name, err)
		}
		return "", false
	}
	return string(b), true
}

// TestGuideMirrorFollowsTheRepository is the README-on-the-fly feature in one
// lifecycle: a guide that appears, changes, and is retracted, with a poll between
// each and nothing rebuilt.
//
// The retraction half is the one worth having a test for. Mirroring only presence
// would leave the image's own baked-in copy on a public page after the operator
// deleted the file, which is worse than never having mirrored at all: the page
// would be serving a sentence that was deliberately withdrawn.
func TestGuideMirrorFollowsTheRepository(t *testing.T) {
	h, dir := guideHarness(t)

	// Nothing in the repository, so nothing mirrored — and in particular no empty
	// file, which the dashboard would render as a blank guide rather than as none.
	if _, ok := guideBody(t, dir, "README.en.md"); ok {
		t.Fatal("README.en.md was mirrored before it existed in the repository")
	}

	h.push(map[string]string{"README.en.md": "# The port moves\n"})
	h.poll()
	if got, ok := guideBody(t, dir, "README.en.md"); !ok || got != "# The port moves\n" {
		t.Fatalf("after the first push: mirrored %q (present=%v), want the pushed body", got, ok)
	}

	h.push(map[string]string{"README.en.md": "# The port moves, and here is where to look\n"})
	h.poll()
	if got, _ := guideBody(t, dir, "README.en.md"); got != "# The port moves, and here is where to look\n" {
		t.Fatalf("after the edit: mirrored %q, want the corrected body", got)
	}

	h.remove("README.en.md")
	h.poll()
	if got, ok := guideBody(t, dir, "README.en.md"); ok {
		t.Fatalf("README.en.md is still mirrored as %q after being deleted from the repository", got)
	}
}

// TestGuideMirrorMirrorsEveryConfiguredLocale checks that the set of files comes
// from Dashboard.GuideFiles and not from the default locale alone: a Russian player
// reading the English guide is the failure this feature exists to fix.
func TestGuideMirrorMirrorsEveryConfiguredLocale(t *testing.T) {
	h, dir := guideHarness(t)

	h.push(map[string]string{
		"README.en.md": "en\n",
		"README.ru.md": "ru\n",
	})
	h.poll()

	for name, want := range map[string]string{"README.en.md": "en\n", "README.ru.md": "ru\n"} {
		if got, ok := guideBody(t, dir, name); !ok || got != want {
			t.Errorf("%s mirrored as %q (present=%v), want %q", name, got, ok, want)
		}
	}
}

// TestGuideMirrorLeavesUnchangedFilesAlone is the cost argument for running this on
// every poll. The guide changes a few times a year and the poll runs every few
// minutes, so all but a handful of passes must do nothing at all — and "nothing"
// has to include not rewriting identical bytes, because the dashboard's cache is
// keyed by modification time and size. A pointless write invalidates every locale's
// cached copy four times an hour.
func TestGuideMirrorLeavesUnchangedFilesAlone(t *testing.T) {
	h, dir := guideHarness(t)

	h.push(map[string]string{"README.en.md": "steady\n"})
	h.poll()
	st, err := os.Stat(filepath.Join(dir, "README.en.md"))
	if err != nil {
		t.Fatal(err)
	}

	before := h.logCount("guide README.en.md updated")
	h.poll()
	h.poll()
	if after := h.logCount("guide README.en.md updated"); after != before {
		t.Errorf("mirrored the same bytes again: %d writes, want the %d from the first poll",
			after, before)
	}

	st2, err := os.Stat(filepath.Join(dir, "README.en.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !st2.ModTime().Equal(st.ModTime()) {
		t.Errorf("modification time moved from %s to %s without a content change, which "+
			"invalidates the dashboard's cache for nothing", st.ModTime(), st2.ModTime())
	}
}

// TestGuideMirrorSaysNothingAboutFilesThatWereNeverThere pins the quiet half of
// removeGuide. GuideFiles always ends with the untranslated fallback name, and a
// deployment that translates every locale never has one — so on the live system
// that name is absent on every poll forever. A log line per pass would bury the
// boot it was printed next to.
func TestGuideMirrorSaysNothingAboutFilesThatWereNeverThere(t *testing.T) {
	h, dir := guideHarness(t)

	h.push(map[string]string{"README.en.md": "en\n"})
	h.poll()
	h.poll()

	if h.logged("README.md removed") {
		h.dumpLogs()
		t.Error("logged the removal of README.md, which never existed")
	}
	if _, ok := guideBody(t, dir, "README.md"); ok {
		t.Error("README.md was created; the fallback name is a file to look for, not one to write")
	}
}

// TestGuideMirrorIsOffWhenNothingConfiguresIt covers the two ways the feature is
// disabled — no guide_file, and no packages_dir — because both are the state of a
// deployment that has not opted in, and neither may produce a stray file or a stray
// error. This is the default configuration of every other test in this package.
func TestGuideMirrorIsOffWhenNothingConfiguresIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		tune func(*config.Config, string)
	}{
		{
			name: "no guide_file",
			tune: func(c *config.Config, dir string) { c.Controller.Storage.PackagesDir = dir },
		},
		{
			name: "no packages_dir",
			tune: func(c *config.Config, _ string) { c.Dashboard.GuideFile = "README.{lang}.md" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			h := newHarness(t, func(c *config.Config) { tc.tune(c, dir) })
			h.push(map[string]string{"README.en.md": "en\n"})
			h.poll()

			ents, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(ents) != 0 {
				t.Errorf("wrote %d file(s) into packages_dir with the mirror unconfigured", len(ents))
			}
		})
	}
}
