package dashboard

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The realms a page can ask to unlock. They are httpapi's, and a test in this
// package asserts they still are — the strings also appear in the templates, so a
// rename there has to be caught somewhere.
const (
	realmServerFiles = "server-files"
	realmBackups     = "backups"
)

// langCookie is v1's key. v1 kept the choice in localStorage, which meant the
// server never knew it and every page arrived in Russian before JavaScript
// rewrote it; a cookie tells the renderer which locale to produce.
const langCookie = "pz_lang"

// --- pages ---

// serveRoot sends the bare domain to the page that used to be there.
//
// 302 and not 301. A permanent redirect is cached by the browser until someone
// clears it, so the day this route moves again — or the day the root is given a
// page of its own — every reader who has been here once would keep going to the
// old place, and nothing the controller serves could tell them otherwise.
//
// The query travels with it. The root is what a shared link looks like, and a
// ?lang= on one that dropped at the redirect would land the reader in whichever
// locale their browser asked for instead of the one they were sent.
func (h *Handler) serveRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	to := PathConnect
	if q := r.URL.RawQuery; q != "" {
		to += "?" + q
	}
	http.Redirect(w, r, to, http.StatusFound)
}

func (h *Handler) servePage(w http.ResponseWriter, r *http.Request, hub bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}

	want, fromQuery := h.wantedLang(r)
	lang := h.opts.lang(want)
	if fromQuery {
		h.setLangCookie(w, r, lang)
	}

	in := h.data.Snapshot(lang)
	in.Unlocked = h.data.Unlocked(r)

	// A wrong password comes back as a marker on the URL, not as a body: the
	// attempt was a POST, and answering it with a rendered page would make the
	// browser re-submit the password on every refresh.
	if realm, ok := failedRealm(r); ok {
		in.UnlockFailed, in.UnlockRealm = true, realm
	}

	var (
		tmpl *template.Template
		data any
	)
	if hub {
		tmpl, data = h.hub, BuildPage(h.opts, in, lang)
	} else {
		page := BuildBackupsPage(h.opts, in, lang)
		if in.UnlockFailed && !page.Unlocked {
			page.Error = catalog[lang].WrongPwd
		}
		tmpl, data = h.backups, page
	}

	// Rendered into a buffer first. A template that fails half way through would
	// otherwise have already sent 200 and a truncated document, and the reader
	// would see a broken page instead of an error.
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		h.logf("dashboard: rendering %s: %v", r.URL.Path, err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Every page states the server's status, so a cached copy is a lie about
	// whether anyone can connect.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.Header().Set("Vary", "Cookie, Accept-Language")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		w.Write(buf.Bytes())
	}
}

// --- status poll ---

func (h *Handler) serveStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}

	want, _ := h.wantedLang(r)
	lang := h.opts.lang(want)

	body, err := json.Marshal(BuildStatus(h.opts, h.data.Snapshot(lang), lang))
	if err != nil {
		h.logf("dashboard: encoding status: %v", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		w.Write(body)
	}
}

// --- unlock ---

// unlockFormLimit bounds the body. A password form is a few dozen bytes; the only
// reason to send more is to make the controller read it.
const unlockFormLimit = 4 << 10

func (h *Handler) serveUnlock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.methodNotAllowed(w, http.MethodPost)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, unlockFormLimit)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	realm := r.PostFormValue("realm")
	if realm != realmServerFiles && realm != realmBackups {
		// Not a hint about which realms exist: an unknown one is a request this
		// page never generated.
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	next := returnTo(r.PostFormValue("next"), realm)
	if h.data.Unlock(w, r, realm, r.PostFormValue("password")) {
		// 303, so the browser follows with GET and the password is not in the
		// history entry the reader lands on.
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, next+"?unlock=wrong&realm="+realm, http.StatusSeeOther)
}

// returnTo picks where a completed unlock lands.
//
// The value is attacker-supplied, so it is not sanitised — it is matched against
// the two pages that exist. Anything else falls back to the page the realm
// belongs to, which makes an open-redirect through this form impossible rather
// than merely difficult.
//
// PathRoot is deliberately not in the list. It is a redirect, not a page, and
// answering an unlock with "now go and get redirected" would put the unlock=wrong
// marker through a hop that does not have to carry it. A form still posting the
// old value falls through to the default, which is where it was going anyway.
func returnTo(next, realm string) string {
	switch next {
	case PathConnect, PathBackups:
		return next
	}
	if realm == realmBackups {
		return PathBackups
	}
	return PathConnect
}

// failedRealm reads the marker returnTo's failure branch adds.
func failedRealm(r *http.Request) (string, bool) {
	if r.URL.Query().Get("unlock") != "wrong" {
		return "", false
	}
	switch realm := r.URL.Query().Get("realm"); realm {
	case realmServerFiles, realmBackups:
		return realm, true
	}
	return "", false
}

// --- assets ---

func (h *Handler) serveAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	// Embedded files carry no modification time, so there is no Last-Modified for
	// a browser to revalidate against and it would cache heuristically — for how
	// long is up to it. One digest over the whole asset set, computed at startup,
	// gives every file a validator that changes exactly when a build does.
	w.Header().Set("ETag", h.assetETag)
	w.Header().Set("Cache-Control", "no-cache")
	h.assets.ServeHTTP(w, r)
}

// assetsDigest hashes every embedded asset into one ETag value.
func assetsDigest() (string, error) {
	sum := sha256.New()
	entries, err := files.ReadDir("assets")
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		b, err := files.ReadFile("assets/" + e.Name())
		if err != nil {
			return "", err
		}
		sum.Write([]byte(e.Name()))
		sum.Write(b)
	}
	return `"` + hex.EncodeToString(sum.Sum(nil)[:8]) + `"`, nil
}

// --- locale ---

// wantedLang reads the locale the request asks for, and whether it asked
// explicitly. The order is the one a reader expects: what I just clicked, what I
// chose last time, what my browser says.
func (h *Handler) wantedLang(r *http.Request) (want Lang, fromQuery bool) {
	if l, ok := ParseLang(r.URL.Query().Get("lang")); ok {
		return l, true
	}
	if c, err := r.Cookie(langCookie); err == nil {
		if l, ok := ParseLang(c.Value); ok {
			return l, false
		}
	}
	for _, tag := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		if l, ok := ParseLang(tag); ok {
			return l, false
		}
	}
	return "", false
}

func (h *Handler) setLangCookie(w http.ResponseWriter, r *http.Request, lang Lang) {
	http.SetCookie(w, &http.Cookie{
		Name:  langCookie,
		Value: string(lang),
		Path:  "/",
		// A year. It is a display preference, and asking again every session is
		// the annoyance v1's localStorage was avoiding.
		MaxAge: int((365 * 24 * time.Hour).Seconds()),
		// Nothing in the page reads it, and it is not a credential either way.
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
	})
}

// isHTTPS reports whether the reader's connection is encrypted, including the
// case that matters here: TLS terminated at Cloudflare and plain HTTP to the
// container.
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// --- small helpers ---

func (h *Handler) methodNotAllowed(w http.ResponseWriter, allowed ...string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}
