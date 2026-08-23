package dashboard

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
)

// The pages and their two static files travel in the binary. v1 read its
// README from the saves working tree on every request and had the rest inline in
// the Python source; embedding means a running controller has no template files
// to lose and no directory to get wrong.
//
//go:embed templates/*.html assets/*
var files embed.FS

// Paths this package serves. All but one are v1's, so bookmarks and the guide's
// links keep working.
//
// The exception is the main page, which v1 served at the root and which now lives
// at PathConnect. The tab above it says "connect", and a tab whose name and whose
// URL disagree is the kind of small wrongness that makes a link hard to give out
// loud. The root still has to answer — it is the bare domain a player types — so
// it redirects rather than 404s.
const (
	PathRoot    = "/"
	PathConnect = "/connect"
	PathBackups = "/backups"
	PathAssets  = "/assets/"
	PathStatus  = "/api/status"
	PathUnlock  = "/api/unlock"
)

// Data is what the handler needs from the rest of the process.
//
// Three methods rather than a struct of values, because all three answers depend
// on the request: the snapshot must be taken when the page is rendered and not
// before, and the other two are about who is asking.
type Data interface {
	// Snapshot returns what is currently true, with Guide already selected for
	// the locale being rendered. It must not block on IO the FSM holds a lock
	// over — a slow snapshot is a slow page for every viewer at once.
	Snapshot(lang Lang) Inputs

	// Unlocked reports which guarded downloads this request may follow.
	Unlocked(r *http.Request) Unlocked

	// Unlock verifies password for realm and, when it matches, marks the response
	// as unlocked — setting whatever cookie the caller's auth uses. It reports
	// whether the password was accepted.
	//
	// It is the caller's, not this package's, because it is the one operation here
	// that must be rate limited and constant-time, and that belongs next to the
	// rest of the auth rather than next to the HTML.
	Unlock(w http.ResponseWriter, r *http.Request, realm, password string) bool
}

// Handler serves the two pages, the status poll, the unlock endpoint and the two
// static files.
type Handler struct {
	opts Options
	data Data

	hub     *template.Template
	backups *template.Template
	assets  http.Handler

	// assetETag is one digest over every embedded asset, so the stylesheet and
	// the script get a validator that changes with the build and not otherwise.
	assetETag string

	logf func(string, ...any)
}

// HandlerOptions configures a Handler.
type HandlerOptions struct {
	// View is the configured half of every render. Required.
	View Options

	// Data supplies the per-request half. Required.
	Data Data

	Logf func(string, ...any)
}

// NewHandler parses the templates and returns the routed handler.
//
// Parsing happens once, here, and a broken template is an error the process
// reports at startup. v1 built its HTML by string formatting, so a mistake in the
// markup was a page that rendered wrong for whoever hit it first.
func NewHandler(o HandlerOptions) (*Handler, error) {
	if o.Data == nil {
		return nil, errors.New("dashboard: Data is required")
	}
	if o.View.Loc == nil {
		// Every timestamp on the backups page is formatted through it, so a nil
		// here would silently mean UTC — which is the host clock problem the
		// configured timezone exists to remove.
		return nil, errors.New("dashboard: View.Loc is required")
	}

	hub, err := parsePage("page.html")
	if err != nil {
		return nil, err
	}
	backups, err := parsePage("backups.html")
	if err != nil {
		return nil, err
	}

	sub, err := fs.Sub(files, "assets")
	if err != nil {
		return nil, err
	}
	etag, err := assetsDigest()
	if err != nil {
		return nil, err
	}

	logf := o.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	return &Handler{
		opts:      o.View,
		data:      o.Data,
		hub:       hub,
		backups:   backups,
		assets:    http.StripPrefix(PathAssets, http.FileServer(http.FS(sub))),
		assetETag: etag,
		logf:      logf,
	}, nil
}

// parsePage parses one page together with the shared blocks, and returns the page
// itself.
//
// The Lookup is the point. ParseFS names the returned template after the first
// file it was given, so calling Execute on the set runs whichever file happened to
// be listed first — and common.html is nothing but {{define}} blocks, so that
// renders as six blank lines with a 200 next to it. Naming the page explicitly
// makes the argument order irrelevant.
func parsePage(name string) (*template.Template, error) {
	set, err := template.ParseFS(files, "templates/common.html", "templates/"+name)
	if err != nil {
		return nil, err
	}
	page := set.Lookup(name)
	if page == nil {
		return nil, fmt.Errorf("dashboard: %s has no body to render", name)
	}
	return page, nil
}

// ServeHTTP routes. It is a switch rather than a ServeMux because this handler is
// mounted at "/" inside another mux, and a nested ServeMux would answer for every
// path that reached it — including the ones the outer mux meant to 404.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == PathUnlock:
		h.serveUnlock(w, r)
	case path == PathStatus:
		h.serveStatus(w, r)
	case strings.HasPrefix(path, PathAssets):
		h.serveAsset(w, r)
	case path == PathBackups, path == PathBackups+"/":
		h.servePage(w, r, false)
	case path == PathConnect, path == PathConnect+"/":
		h.servePage(w, r, true)
	case path == PathRoot:
		h.serveRoot(w, r)
	default:
		http.NotFound(w, r)
	}
}
