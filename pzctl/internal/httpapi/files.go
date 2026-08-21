package httpapi

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hrkcz001/pz-akash/pzctl/internal/secrets"
)

// Placeholder tokens carried by the templates in pz-saves. They exist so a
// password can be committed to a public repository as the shape of a password
// without being one, and so the same file can be an image layer, a git blob and
// a manifest without any of the three holding the secret.
//
// The substitution happens exactly once, in the response body: nothing writes a
// substituted file to the controller's disk. That is deliberate. A rendered copy
// on disk would be a secret at rest on an ephemeral volume nobody audits, and it
// would have to be invalidated whenever a secret rotated.
const (
	PlaceholderRCONPassword  = "__RCON_PASSWORD__"
	PlaceholderAdminPassword = "__ADMIN_PASSWORD__"
	PlaceholderJoinPassword  = "__JOIN_PASSWORD__"
)

// Substituter rewrites the guarded entries of an archive as it is served.
type Substituter struct {
	// patterns select entries by path.Match against the zip entry name.
	patterns []string
	// values maps a placeholder token to its replacement.
	values map[string]string
	// maxBytes bounds one entry that will be rewritten.
	maxBytes int64

	logf func(string, ...any)
}

// NewSubstituter builds the rewriter from the config patterns and the loaded
// secrets.
//
// A secret that is empty maps its placeholder to the empty string rather than
// being skipped. Skipping would leave the literal `__JOIN_PASSWORD__` in the
// .ini, and PZ would enforce that string as the join password — a server nobody
// can enter, configured by an omission. Writing an empty value means "no
// password", which is what an unset secret means.
func NewSubstituter(patterns []string, sec *secrets.Set, maxBytes int64, logf func(string, ...any)) *Substituter {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &Substituter{
		patterns: append([]string{}, patterns...),
		values:   map[string]string{},
		maxBytes: maxBytes,
		logf:     logf,
	}
	if sec != nil {
		s.values[PlaceholderRCONPassword] = sec.RCONPassword
		s.values[PlaceholderAdminPassword] = sec.AdminPassword
		s.values[PlaceholderJoinPassword] = sec.JoinPassword
	}
	return s
}

// Active reports whether this substituter would do anything. A zip with no
// matching entries is served by the raw path, which is both faster and lets the
// handler set a Content-Length.
func (s *Substituter) Active() bool { return s != nil && len(s.patterns) > 0 }

// matches reports whether an entry gets rewritten.
func (s *Substituter) matches(name string) bool {
	for _, pat := range s.patterns {
		if ok, err := path.Match(pat, name); err == nil && ok {
			return true
		}
	}
	return false
}

// Rewrite copies src to dst, substituting inside the matching entries.
//
// Entries that do not match are copied with their compressed bytes intact via
// OpenRaw/CreateRaw — no inflate, no deflate, no CRC recomputation. That is what
// makes this affordable on every boot: server.zip is a few hundred megabytes of
// mods around fifteen kilobytes of .ini, and only the .ini is touched.
func (s *Substituter) Rewrite(dst io.Writer, src *zip.Reader) error {
	zw := zip.NewWriter(dst)
	var rewritten []string

	for _, f := range src.File {
		var err error
		switch {
		case s.matches(f.Name) && int64(f.UncompressedSize64) <= s.maxBytes:
			var changed bool
			changed, err = s.rewriteEntry(zw, f)
			if changed {
				rewritten = append(rewritten, f.Name)
			}
		default:
			if s.matches(f.Name) {
				// Matched but too big to hold in memory. Passing it through is the
				// lesser evil: refusing would make server.zip unservable, and the
				// server cannot boot without it. The log line is the only way anyone
				// finds out, so it says what it means rather than "skipped".
				s.logf("server.zip: %s matches a substitution pattern but is %d bytes "+
					"(limit %d); served with its placeholders intact",
					f.Name, f.UncompressedSize64, s.maxBytes)
			}
			err = copyRaw(zw, f)
		}
		if err != nil {
			zw.Close()
			return fmt.Errorf("httpapi: rewriting %s: %w", f.Name, err)
		}
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("httpapi: finishing the archive: %w", err)
	}
	if len(rewritten) > 0 {
		sort.Strings(rewritten)
		s.logf("server.zip: substituted secrets into %s", strings.Join(rewritten, ", "))
	}
	return nil
}

// rewriteEntry inflates one entry, substitutes, and deflates it back.
func (s *Substituter) rewriteEntry(zw *zip.Writer, f *zip.File) (changed bool, err error) {
	rc, err := f.Open()
	if err != nil {
		return false, err
	}
	defer rc.Close()
	// Bounded by the size the header declares plus one, so a header that lies about
	// a small entry cannot turn into an unbounded read.
	raw, err := io.ReadAll(io.LimitReader(rc, s.maxBytes+1))
	if err != nil {
		return false, err
	}
	if int64(len(raw)) > s.maxBytes {
		return false, fmt.Errorf("entry is larger than its header declared (over %d bytes)", s.maxBytes)
	}

	body := string(raw)
	for token, value := range s.values {
		if strings.Contains(body, token) {
			body = strings.ReplaceAll(body, token, value)
			changed = true
		}
	}

	// A fresh header: the sizes and CRC of the original describe bytes we are no
	// longer writing, and CreateHeader recomputes them. Name, mode and modification
	// time are carried over so the archive an agent unpacks looks the same either
	// way.
	h := &zip.FileHeader{
		Name:     f.Name,
		Method:   zip.Deflate,
		Modified: f.Modified,
	}
	h.SetMode(f.Mode())
	w, err := zw.CreateHeader(h)
	if err != nil {
		return false, err
	}
	_, err = io.WriteString(w, body)
	return changed, err
}

// copyRaw copies an entry without touching its compressed bytes.
func copyRaw(zw *zip.Writer, f *zip.File) error {
	// A directory entry has no data stream; CreateRaw on one writes a member the
	// reader then rejects for a CRC mismatch on zero bytes.
	if f.FileInfo().IsDir() {
		_, err := zw.Create(f.Name)
		return err
	}
	r, err := f.OpenRaw()
	if err != nil {
		return err
	}
	h := f.FileHeader
	w, err := zw.CreateRaw(&h)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, r)
	return err
}

// --- serving ---

// zipFile is one of the three packages, resolved against packages_dir.
type zipFile struct {
	// urlPath is the path clients request.
	urlPath string
	// fileName is the basename inside packages_dir.
	fileName string
	// realm guards it. Empty means public.
	realm Realm
	// substitute is set for the archive that carries the game secrets.
	substitute bool
}

// packages is the complete served set, in the order the dashboard lists them.
//
// It is a table rather than three registrations so that the realm and the
// substitution flag sit next to the path they apply to. In v1 the equivalent
// facts were spread across a `do_GET` if-chain, an auth helper that took the path
// as a string, and a separate branch for server.zip — and the way that fails is a
// new path added to the router and to neither of the other two.
var packages = []zipFile{
	{PathCommonZip, "common.zip", RealmPublic, false},
	{PathClientZip, "client.zip", RealmPublic, false},
	{PathServerZip, "server.zip", RealmServerFiles, true},
}

// openPackage opens one package for reading, as a *zip.Reader when it needs
// rewriting and as a plain file when it does not.
func openPackage(dir string, p zipFile) (*os.File, os.FileInfo, error) {
	// filepath.Base defends the join even though fileName comes from the table
	// above: it costs nothing, and the day someone makes the table configurable is
	// the day the absence of this line becomes a path traversal.
	f, err := os.Open(filepath.Join(dir, filepath.Base(p.fileName)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("%w: %s", ErrNotFound, p.fileName)
		}
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, info, nil
}
