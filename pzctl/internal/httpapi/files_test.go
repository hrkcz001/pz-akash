package httpapi

import (
	"bytes"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/hrkcz001/pz-akash/pzctl/internal/secrets"
)

// The fixture stands in for server.zip: a little .ini that needs substituting,
// surrounded by the mods that make up the bulk of the real archive.
func serverZipFixture(t *testing.T) []byte {
	t.Helper()
	return makeZip(t, "", []zipEntry{
		{name: "Server/", dir: true},
		{name: "Server/vsrania.ini", body: strings.Join([]string{
			"Password=" + PlaceholderJoinPassword,
			"RCONPassword=" + PlaceholderRCONPassword,
			"PublicName=vsrania",
		}, "\r\n")},
		{name: "Server/vsrania_SandboxVars.lua", body: "SandboxVars = { Zombies = 3 }"},
		{name: "mods/big.pak", body: strings.Repeat("mod payload ", 4096)},
	})
}

// This is what makes per-request substitution affordable on a few hundred
// megabytes: every entry that does not match a pattern is copied with its
// compressed bytes untouched — no inflate, no deflate, no CRC recomputation.
// Byte-identical raw members is the observable form of that claim.
func TestRewriteCopiesNonMatchingEntriesByteForByte(t *testing.T) {
	src := serverZipFixture(t)
	sub := NewSubstituter([]string{"Server/*.ini"}, testSecs, 4<<20, nil)

	var out bytes.Buffer
	zr, err := zipReaderFor(bytes.NewReader(src), int64(len(src)))
	if err != nil {
		t.Fatal(err)
	}
	if err := sub.Rewrite(&out, zr); err != nil {
		t.Fatal(err)
	}

	before, after := rawBytes(t, src), rawBytes(t, out.Bytes())
	for name, want := range before {
		got, ok := after[name]
		if !ok {
			t.Fatalf("%s is missing from the rewritten archive", name)
		}
		if name == "Server/vsrania.ini" {
			if bytes.Equal(got, want) {
				t.Fatal("the .ini came through unchanged; nothing was substituted")
			}
			continue
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s was recompressed (%d raw bytes in, %d out); the raw-copy path "+
				"is not being taken and every boot now pays to deflate the mods",
				name, len(want), len(got))
		}
	}
	if len(after) != len(before) {
		t.Fatalf("entry count changed: %d -> %d", len(before), len(after))
	}
}

func TestRewriteSubstitutesEveryPlaceholderItKnows(t *testing.T) {
	src := serverZipFixture(t)
	sub := NewSubstituter([]string{"Server/*.ini"}, testSecs, 4<<20, nil)

	var out bytes.Buffer
	zr, err := zipReaderFor(bytes.NewReader(src), int64(len(src)))
	if err != nil {
		t.Fatal(err)
	}
	if err := sub.Rewrite(&out, zr); err != nil {
		t.Fatal(err)
	}

	ini := readZip(t, out.Bytes())["Server/vsrania.ini"]
	if !strings.Contains(ini, "Password="+testSecs.JoinPassword) {
		t.Fatalf("the join password did not land:\n%s", ini)
	}
	if !strings.Contains(ini, "RCONPassword="+testSecs.RCONPassword) {
		t.Fatalf("the RCON password did not land:\n%s", ini)
	}
	// A placeholder left in a file PZ is about to read is a password nobody can
	// guess and nobody chose. Any of the three surviving is a failure, including
	// the admin one this fixture never used.
	for _, ph := range []string{PlaceholderJoinPassword, PlaceholderRCONPassword, PlaceholderAdminPassword} {
		if strings.Contains(ini, ph) {
			t.Fatalf("%s survived the rewrite", ph)
		}
	}
	// The rest of the file is untouched — a substituter that reformats the .ini is
	// a substituter that silently changes server settings.
	if !strings.Contains(ini, "PublicName=vsrania") {
		t.Fatalf("an unrelated line was altered:\n%s", ini)
	}
	if !strings.Contains(ini, "\r\n") {
		t.Fatal("CRLF line endings were rewritten; PZ wrote this file and expects them back")
	}
}

// An unset secret must become an empty value, not a skipped substitution.
// Skipping leaves the literal `__JOIN_PASSWORD__` in the .ini and PZ enforces
// that string as the join password: a server nobody can enter, configured by an
// omission.
func TestRewriteMapsAnEmptySecretToAnEmptyValue(t *testing.T) {
	src := serverZipFixture(t)
	sub := NewSubstituter([]string{"Server/*.ini"}, &secrets.Set{}, 4<<20, nil)

	var out bytes.Buffer
	zr, err := zipReaderFor(bytes.NewReader(src), int64(len(src)))
	if err != nil {
		t.Fatal(err)
	}
	if err := sub.Rewrite(&out, zr); err != nil {
		t.Fatal(err)
	}

	ini := readZip(t, out.Bytes())["Server/vsrania.ini"]
	for _, want := range []string{"Password=\r\n", "RCONPassword=\r\n"} {
		if !strings.Contains(ini, want) {
			t.Fatalf("want a bare %q, got:\n%s", strings.TrimSuffix(want, "\r\n"), ini)
		}
	}
	if strings.Contains(ini, "__") {
		t.Fatalf("a placeholder survived an empty secret:\n%s", ini)
	}
}

// A nil secrets.Set is the shape a controller takes when its env vars did not
// arrive. It must still produce a well-formed archive — the alternative is a
// server.zip that will not unzip, which is a harder failure to diagnose than a
// server with no passwords.
func TestRewriteWithNoSecretsStillProducesAValidArchive(t *testing.T) {
	src := serverZipFixture(t)
	sub := NewSubstituter([]string{"Server/*.ini"}, nil, 4<<20, nil)

	var out bytes.Buffer
	zr, err := zipReaderFor(bytes.NewReader(src), int64(len(src)))
	if err != nil {
		t.Fatal(err)
	}
	if err := sub.Rewrite(&out, zr); err != nil {
		t.Fatal(err)
	}
	got := readZip(t, out.Bytes())
	if len(got) != 4 {
		t.Fatalf("entries = %v, want all four", keys(got))
	}
	// Nothing to substitute with, so the placeholders stay. That is visible and
	// diagnosable; a truncated archive is not.
	if !strings.Contains(got["Server/vsrania.ini"], PlaceholderJoinPassword) {
		t.Fatal("with no secrets loaded the placeholder should be left alone")
	}
}

// A matching entry too large to hold in memory is passed through with its
// placeholders intact and logged. Refusing would make server.zip unservable, and
// the server cannot boot without it — but the log line is the only way anyone
// finds out, so its absence is the real defect.
func TestRewritePassesAnOversizedMatchThroughAndSaysSo(t *testing.T) {
	src := makeZip(t, "", []zipEntry{
		{name: "Server/huge.ini", body: "Password=" + PlaceholderJoinPassword +
			"\n" + strings.Repeat("filler\n", 2048)},
	})
	var logs []string
	sub := NewSubstituter([]string{"Server/*.ini"}, testSecs, 64,
		func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) })

	var out bytes.Buffer
	zr, err := zipReaderFor(bytes.NewReader(src), int64(len(src)))
	if err != nil {
		t.Fatal(err)
	}
	if err := sub.Rewrite(&out, zr); err != nil {
		t.Fatalf("an oversized match made the archive unservable: %v", err)
	}
	if !strings.Contains(readZip(t, out.Bytes())["Server/huge.ini"], PlaceholderJoinPassword) {
		t.Fatal("the oversized entry was rewritten after all")
	}
	found := false
	for _, l := range logs {
		if strings.Contains(l, "placeholders intact") {
			found = true
		}
	}
	if !found {
		t.Fatalf("nothing was logged about the skipped substitution; logs = %v", logs)
	}
}

func TestSubstituterMatchesOnlyTheConfiguredPatterns(t *testing.T) {
	sub := NewSubstituter([]string{"Server/*.ini"}, testSecs, 4<<20, nil)
	cases := map[string]bool{
		"Server/vsrania.ini":         true,
		"Server/vsrania_zombies.ini": true,
		// path.Match's `*` does not cross a separator, which is what keeps a mod
		// shipping its own .ini out of scope.
		"Server/mods/thing.ini": false,
		// client.zip carries .ini files too, and those are public. A pattern of
		// "every .ini" would substitute real passwords into the archive players
		// download.
		"Client/options.ini":             false,
		"Server/vsrania_SandboxVars.lua": false,
		"mods/big.pak":                   false,
	}
	for name, want := range cases {
		if got := sub.matches(name); got != want {
			t.Errorf("matches(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestSubstituterWithNoPatternsIsInactive(t *testing.T) {
	// Inactive means the handler takes the ServeContent path instead: a real
	// Content-Length, Range support, and no per-request work. An "active"
	// substituter with nothing to do would cost every download a chunked re-zip.
	if NewSubstituter(nil, testSecs, 4<<20, nil).Active() {
		t.Fatal("a substituter with no patterns reports itself active")
	}
	var nilSub *Substituter
	if nilSub.Active() {
		t.Fatal("a nil substituter reports itself active")
	}
}

func TestRewritePreservesDirectoryEntries(t *testing.T) {
	// CreateRaw on a directory writes a member the reader then rejects for a CRC
	// mismatch on zero bytes, so directories go through zw.Create. Without this the
	// archive unzips with an error in the middle.
	src := makeZip(t, "", []zipEntry{
		{name: "Server/", dir: true},
		{name: "media/", dir: true},
		{name: "Server/a.ini", body: "Password=" + PlaceholderJoinPassword},
	})
	sub := NewSubstituter([]string{"Server/*.ini"}, testSecs, 4<<20, nil)

	var out bytes.Buffer
	zr, err := zipReaderFor(bytes.NewReader(src), int64(len(src)))
	if err != nil {
		t.Fatal(err)
	}
	if err := sub.Rewrite(&out, zr); err != nil {
		t.Fatal(err)
	}
	got := keys(readZip(t, out.Bytes()))
	want := []string{"Server/", "Server/a.ini", "media/"}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries = %v, want %v", got, want)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
