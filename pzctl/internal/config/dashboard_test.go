package config

// The dashboard's two derived names: what a browser saves the torrent as, and which
// files make up the guide. Both are computed rather than configured because both are
// read by two packages that must not disagree — httpapi and the page for the first,
// the fsm mirror and the webhook filter for the second.

import (
	"reflect"
	"strings"
	"testing"
)

func TestTorrentName(t *testing.T) {
	for _, tc := range []struct {
		name     string
		file     string
		download string
		version  string
		want     string
	}{
		{
			// The case this exists for. The repository wants a stable name it can
			// overwrite on every game update; a player wants the name the torrent is
			// known by, because it is what tells them which build they are about to
			// download.
			name: "version substituted", file: "game.torrent",
			download: "ProjectZomboid{version}Portable.torrent", version: "42.20.3",
			want: "ProjectZomboid42.20.3Portable.torrent",
		},
		{
			name: "no download name configured", file: "game.torrent",
			want: "game.torrent",
		},
		{
			// A literal name is allowed: the placeholder is a convenience, not a
			// requirement.
			name: "no placeholder", file: "game.torrent",
			download: "ProjectZomboidPortable.torrent", version: "42.20.3",
			want: "ProjectZomboidPortable.torrent",
		},
		{
			// The one that matters. An unset game_version must not reach a player as
			// the literal word "{version}" in a filename, so the whole rename is
			// abandoned rather than half-applied.
			name: "placeholder with no version", file: "game.torrent",
			download: "ProjectZomboid{version}Portable.torrent",
			want:     "game.torrent",
		},
		{
			// Nothing to name. The route is not mounted at all in this case, so the
			// value is unused — but returning the download name here would make a
			// caller believe there was a file.
			name: "no torrent at all", download: "ProjectZomboid{version}Portable.torrent",
			version: "42.20.3", want: "",
		},
	} {
		d := Dashboard{TorrentFile: tc.file, TorrentDownloadName: tc.download, GameVersion: tc.version}
		if got := d.TorrentName(); got != tc.want {
			t.Errorf("%s: TorrentName() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestGuideFiles(t *testing.T) {
	for _, tc := range []struct {
		name    string
		guide   string
		locales []string
		want    []string
	}{
		{
			// Every configured locale, then the untranslated name. The fallback is
			// last because it is the least specific, and it is present because a
			// deployment may serve one guide to everyone.
			name:  "one file per locale plus the fallback",
			guide: "README.{lang}.md", locales: []string{"ru", "en"},
			want: []string{"README.ru.md", "README.en.md", "README.md"},
		},
		{
			// The mirror writes each name and the webhook watches each name, so a
			// duplicate would mean a doubled write and a doubled rule.
			name:  "duplicate locales collapse",
			guide: "README.{lang}.md", locales: []string{"en", "en"},
			want: []string{"README.en.md", "README.md"},
		},
		{
			// A guide with no placeholder is the same file for every locale, and the
			// fallback removal leaves it unchanged — so one name, not three.
			name:  "no placeholder",
			guide: "GUIDE.md", locales: []string{"ru", "en"},
			want: []string{"GUIDE.md"},
		},
		{
			// The feature turned off. Returning the fallback name here would have the
			// controller mirror, and the webhook watch, a file nobody configured.
			name: "no guide configured", locales: []string{"en"}, want: nil,
		},
		{
			// No locales is not a reason to serve nothing: the untranslated name is
			// still a file the operator may have written.
			name: "no locales", guide: "README.{lang}.md", want: []string{"README.md"},
		},
	} {
		d := Dashboard{GuideFile: tc.guide, Locales: tc.locales}
		if got := d.GuideFiles(); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: GuideFiles() = %#v, want %#v", tc.name, got, tc.want)
		}
	}
}

// TestValidateTorrentDownloadName covers the rules on a config value that becomes a
// Content-Disposition header. Two of them are about the header and one is about the
// operator having asked for something that cannot happen.
func TestValidateTorrentDownloadName(t *testing.T) {
	for _, tc := range []struct {
		name     string
		file     string
		download string
		want     string // a substring of the error, or "" for "must be accepted"
	}{
		{
			name: "the shipped value", file: "game.torrent",
			download: "ProjectZomboid{version}Portable.torrent",
		},
		{
			// It names a file the browser saves, not a place it saves it. A path here
			// would be a filename with a slash in it as far as the header is
			// concerned, and browsers disagree about what to do with one.
			name: "a path", file: "game.torrent",
			download: "downloads/game.torrent",
			want:     "torrent_download_name",
		},
		{
			// The value is interpolated into a quoted header value, so a quote or a
			// newline in it is header injection with extra steps.
			name: "a quote", file: "game.torrent",
			download: `game".torrent`,
			want:     "quoted header value",
		},
		{
			name: "a newline", file: "game.torrent",
			download: "game\n.torrent",
			want:     "quoted header value",
		},
		{
			// Nothing to rename. Silently ignoring this would leave an operator who
			// meant to publish a torrent believing they had.
			name: "no torrent to name", download: "ProjectZomboidPortable.torrent",
			want: "there is no torrent to name",
		},
	} {
		c := mustLoadReal(t)
		c.Dashboard.TorrentFile = tc.file
		c.Dashboard.TorrentDownloadName = tc.download
		err := c.Validate()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: rejected a value that should be accepted: %v", tc.name, err)
		case tc.want == "":
		case err == nil:
			t.Errorf("%s: accepted %q", tc.name, tc.download)
		case !strings.Contains(err.Error(), tc.want):
			t.Errorf("%s: error does not mention %q:\n%v", tc.name, tc.want, err)
		}
	}
}
