package agent

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The archive shape is a compatibility contract, not an implementation detail:
// backups the operator already downloaded from v1 must stay restorable, and
// backups v2 produces must stay restorable by hand with `unzip`. These tests pin
// it.

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestZipDirStoresPathsRelativeToTheSource(t *testing.T) {
	saves := filepath.Join(t.TempDir(), "Saves")
	writeFile(t, filepath.Join(saves, "Multiplayer", "vsrania", "map_t.bin"), "world")
	writeFile(t, filepath.Join(saves, "Multiplayer", "vsrania", "players.db"), "db")

	dst := filepath.Join(t.TempDir(), "backup_20260819_120000.zip")
	size, sum, err := zipDir(saves, dst)
	if err != nil {
		t.Fatal(err)
	}
	if size <= 0 {
		t.Fatalf("size = %d, want > 0", size)
	}
	if len(sum) != 64 {
		t.Fatalf("sha256 = %q, want 64 hex chars", sum)
	}
	// The digest is computed while writing, so it must equal a digest of the file
	// on disk. If these ever disagree the controller rejects every upload.
	if onDisk, err := sha256File(dst); err != nil || onDisk != sum {
		t.Fatalf("sha256File = %q (err %v), zipDir reported %q", onDisk, err, sum)
	}

	zr, err := zip.OpenReader(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	// Entries are the *contents* of Saves — no "Saves/" prefix — with forward
	// slashes even though this test may run on Windows. That is exactly what v1's
	// `cd Saves && zip -q -r - .` produced.
	want := "Multiplayer/vsrania/map_t.bin"
	if !containsStr(names, want) {
		t.Fatalf("entries = %v, want one named %q", names, want)
	}
	for _, n := range names {
		if strings.HasPrefix(n, "Saves/") || strings.Contains(n, `\`) {
			t.Fatalf("entry %q: archive must hold Saves' contents with slash-separated names", n)
		}
	}
}

func TestZipUnzipRoundTrip(t *testing.T) {
	src := filepath.Join(t.TempDir(), "Saves")
	writeFile(t, filepath.Join(src, "a.txt"), "alpha")
	writeFile(t, filepath.Join(src, "nested", "deep", "b.txt"), "beta")
	if err := os.MkdirAll(filepath.Join(src, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(t.TempDir(), "a.zip")
	if _, _, err := zipDir(src, archive); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "restored")
	if err := unzip(archive, dst); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		filepath.Join(dst, "a.txt"):                   "alpha",
		filepath.Join(dst, "nested", "deep", "b.txt"): "beta",
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", path, got, want)
		}
	}
	// An empty directory in the world (PZ makes several) must survive, or the game
	// recreates it and logs errors on the way.
	if st, err := os.Stat(filepath.Join(dst, "empty")); err != nil || !st.IsDir() {
		t.Fatalf("empty/ did not survive the round trip: %v", err)
	}
}

func TestUnzipOverwritesInPlace(t *testing.T) {
	src := filepath.Join(t.TempDir(), "Saves")
	writeFile(t, filepath.Join(src, "a.txt"), "new")
	archive := filepath.Join(t.TempDir(), "a.zip")
	if _, _, err := zipDir(src, archive); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	// A longer stale file: an extract that did not truncate would leave a tail
	// behind, which is a corrupt save that only shows up in game.
	writeFile(t, filepath.Join(dst, "a.txt"), "old and considerably longer")
	if err := unzip(archive, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("a.txt = %q, want %q", got, "new")
	}
}

func TestSafeJoinRefusesEscapes(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{
		"../escape.txt",
		"a/../../escape.txt",
		"/etc/passwd",
		"",
	} {
		if _, err := safeJoin(root, name); err == nil {
			t.Errorf("safeJoin(%q) = nil error, want a refusal", name)
		}
	}
	for _, name := range []string{"a.txt", "a/b/c.txt", "./a.txt"} {
		if _, err := safeJoin(root, name); err != nil {
			t.Errorf("safeJoin(%q) = %v, want it accepted", name, err)
		}
	}
}

func TestUnzipRefusesAnEscapingEntry(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "evil.zip")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("../../pwned.txt")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("x"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	dst := filepath.Join(t.TempDir(), "out")
	if err := unzip(archive, dst); err == nil {
		t.Fatal("unzip accepted an entry that escapes the destination")
	}
}

func TestEmptyDirKeepsTheDirectoryItself(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Saves")
	writeFile(t, filepath.Join(dir, "sub", "file"), "x")

	if err := emptyDir(dir); err != nil {
		t.Fatal(err)
	}
	// The directory survives because it may be a mount point or the target of the
	// lowercase symlink; replacing it would break both.
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		t.Fatalf("stat %s: %v", dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("%d entries left, want 0", len(entries))
	}
}

func TestEmptyDirCreatesAMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does", "not", "exist")
	if err := emptyDir(dir); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("stat %s: %v", dir, err)
	}
}

func TestDirSizeCountsFilesOnly(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a"), "12345")
	writeFile(t, filepath.Join(dir, "sub", "b"), "678")
	if got := dirSize(dir); got != 8 {
		t.Fatalf("dirSize = %d, want 8", got)
	}
}
