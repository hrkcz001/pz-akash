package httpapi

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/secrets"
)

// Server is the controller's HTTP file service: health, the three packages, the
// backup index, and backup download/upload.
//
// It deliberately does not include the dashboard or the webhook. The dashboard is
// step 7 and mounts onto the same handler; the webhook has its own package and its
// own secret, and may run on a separate port. Keeping them apart means the thing
// that serves multi-gigabyte archives has no HTML in it and the thing that
// verifies HMACs has no file paths in it.
type Server struct {
	store *Store
	guard *guard
	sub   *Substituter

	packagesDir string
	torrentFile string
	torrentName string
	uploadLimit time.Duration
	readHeader  time.Duration
	idle        time.Duration
	shutdown    time.Duration

	logf func(string, ...any)
	now  func() time.Time

	// extra is mounted for anything the caller adds — the dashboard in step 7.
	extra http.Handler
}

// ServerOptions configures a Server.
type ServerOptions struct {
	// Store owns backups.dir. Required.
	Store *Store

	// Cfg supplies controller.storage. Only that block is read.
	Cfg *config.Config

	// Secrets supplies the realm tokens and the values substituted into
	// server.zip. A nil Set makes every guarded path answer 401 and every
	// placeholder resolve to empty, which is the correct behaviour for a
	// controller whose secrets did not arrive: refuse, loudly, rather than serve.
	Secrets *secrets.Set

	Logf func(string, ...any)
	Now  func() time.Time

	// Extra is mounted under any path this package does not claim.
	Extra http.Handler
}

// NewServer builds the handler set.
func NewServer(o ServerOptions) (*Server, error) {
	if o.Store == nil {
		return nil, errors.New("httpapi: a Store is required")
	}
	if o.Cfg == nil {
		return nil, errors.New("httpapi: a Config is required")
	}
	st := o.Cfg.Controller.Storage
	logf := o.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	d := o.Cfg.Dashboard
	sess, err := newSessions(d.SessionTTL.D(), d.UnlockWindow.D(), d.UnlockAttempts, now)
	if err != nil {
		return nil, fmt.Errorf("httpapi: generating the unlock signing key: %w", err)
	}
	return &Server{
		store:       o.Store,
		guard:       newGuard(o.Secrets, sess, logf),
		sub:         NewSubstituter(st.SubstituteEntries, o.Secrets, st.SubstituteMaxBytes, logf),
		packagesDir: st.PackagesDir,
		torrentFile: d.TorrentFile,
		torrentName: d.TorrentName(),
		uploadLimit: st.UploadTimeout.D(),
		readHeader:  st.ReadHeaderTimeout.D(),
		idle:        st.IdleTimeout.D(),
		shutdown:    st.ShutdownGrace.D(),
		logf:        logf,
		now:         now,
		extra:       o.Extra,
	}, nil
}

// Handler is the routed handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(PathHealth, s.handleHealth)
	for _, p := range s.static() {
		mux.HandleFunc(p.urlPath, s.packageHandler(p))
	}
	mux.HandleFunc(PathBackupsIndex, s.handleIndex)
	// One segment, not a subtree. Registering the bare prefix would make
	// ServeMux redirect "/backups" — the dashboard's page — into this handler, and
	// it would also route "/backups/sub/dir.zip" here for BackupName to reject.
	// The wildcard matches exactly the shape a backup name has.
	mux.HandleFunc(PathBackupsDir+"{name}", s.handleBackup)
	if s.extra != nil {
		mux.Handle("/", s.extra)
	}
	return mux
}

// ListenAndServe runs the service until ctx is cancelled, then drains.
//
// The timeouts come from config for a reason worth stating: there is no
// ReadTimeout. A whole-request read deadline would kill a backup upload part-way
// through, and the request most likely to be long is the one whose failure costs
// the world. ReadHeaderTimeout bounds the part an attacker controls cheaply — a
// connection that dribbles headers forever — and the body is bounded by
// upload_timeout, per handler, where the number can be large without also
// applying to a health check.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	st := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: s.readHeaderTimeout(),
		IdleTimeout:       s.idleTimeout(),
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	errs := make(chan error, 1)
	go func() {
		s.logf("http: listening on %s", addr)
		err := st.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errs <- err
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.shutdownGrace())
	defer cancel()
	s.logf("http: draining for up to %s", s.shutdownGrace())
	if err := st.Shutdown(grace); err != nil {
		// A drain that timed out means a transfer was cut off. Say so: the operator
		// is about to wonder why an agent reported a short read.
		s.logf("http: shutdown did not drain cleanly: %v", err)
	}
	return <-errs
}

// The three durations are read back through accessors so a zero — which
// validation rejects, but a hand-built Config in a test may carry — cannot become
// "no timeout at all" on the two where that is dangerous.
func (s *Server) readHeaderTimeout() time.Duration { return orDefault(s.readHeader, 30*time.Second) }
func (s *Server) idleTimeout() time.Duration       { return orDefault(s.idle, 120*time.Second) }
func (s *Server) shutdownGrace() time.Duration     { return orDefault(s.shutdown, 30*time.Second) }

func orDefault(d, fallback time.Duration) time.Duration {
	if d <= 0 {
		return fallback
	}
	return d
}

// --- the unlock, for the dashboard ---

// Unlocked reports whether r may follow realm's downloads.
//
// The dashboard calls it to decide between a link and a lock, and it is the same
// predicate the download handlers enforce — deliberately the same function, so a
// page that renders a link cannot be showing one the file handler would 401.
func (s *Server) Unlocked(realm Realm, r *http.Request) bool {
	return s.guard.allow(realm, r)
}

// Unlock checks a submitted password for realm and, when it matches, sets the
// cookie that Unlocked will accept. It reports whether the password was right.
//
// Rate limiting lives here rather than in the dashboard because this is the only
// place a password is compared, and a limiter attached to the page could be walked
// around by whatever else learns to call this. Exhausting the limit answers the
// same "no" a wrong password does: an attacker who can tell "throttled" from
// "wrong" knows when to back off, and the operator's own retry is a page reload
// either way.
func (s *Server) Unlock(w http.ResponseWriter, r *http.Request, realm Realm, password string) bool {
	if realm != RealmServerFiles && realm != RealmBackups {
		return false
	}
	if !s.sessions().take(r) {
		s.logf("auth: unlock for realm %q from %s refused — attempt limit reached", realm, clientKey(r))
		return false
	}
	if !s.guard.verify(realm, password) {
		s.sessions().penalise(r)
		return false
	}
	s.sessions().grant(w, r, realm)
	s.logf("auth: unlocked realm %q for %s", realm, clientKey(r))
	return true
}

func (s *Server) sessions() *sessions { return s.guard.sess }

// --- handlers ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		fmt.Fprintln(w, "ok")
	}
}

// handleIndex serves backups.json.
//
// Public, like the two open packages: it lists names, sizes and digests, which are
// not secrets, and the dashboard reads it from a browser that holds no bearer
// token. Downloading any archive it names still requires the backups credential.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	idx := s.store.Index()
	body, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	body = append(body, '\n')
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	// The index changes whenever a backup lands. A cached copy is how an agent
	// verifies a restore against a digest for an archive that has since been
	// replaced.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		w.Write(body)
	}
}

// methodNotAllowed answers with the Allow header RFC 9110 requires, which is what
// lets a client tell "wrong verb" from "no such path".
func methodNotAllowed(w http.ResponseWriter, allowed ...string) {
	w.Header().Set("Allow", joinComma(allowed))
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func joinComma(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ", "
		}
		out += v
	}
	return out
}

// fail logs the real error and returns a short one.
//
// The split is the point: the log gets the path, the digest and the underlying
// errno, and the response gets a sentence. A 507 that quotes the free-space
// numbers back to whoever asked is telling an unauthenticated caller the size of
// the disk.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, code int, err error) {
	s.logf("http: %s %s -> %d: %v", r.Method, r.URL.Path, code, err)
	http.Error(w, http.StatusText(code), code)
}

// statusFor maps a store error to a status code. Every sentinel appears here, so
// adding one without deciding its code is a compile-time-visible omission rather
// than a silent 500.
func statusFor(err error) int {
	switch {
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrBadName):
		return http.StatusBadRequest
	case errors.Is(err, ErrDigestMismatch):
		// Not 400: the request was well-formed and the sender's intent was clear.
		// 422 says "I understood and the content is wrong", which is what tells the
		// agent to retry the transfer rather than to stop and report a bad request.
		return http.StatusUnprocessableEntity
	case errors.Is(err, ErrTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, ErrNoSpace):
		return http.StatusInsufficientStorage
	case errors.Is(err, os.ErrDeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		// 408, not 500: the controller is fine and the transfer is not. That is what
		// tells the agent to retry — a 500 would have it report a broken controller
		// and stop, leaving the halt backup unsent.
		return http.StatusRequestTimeout
	case errors.Is(err, context.Canceled):
		// The client hung up. Nothing will read this status, but 499 is what appears
		// in the log line, and it must not be a 500 that pages someone.
		return 499
	}
	return http.StatusInternalServerError
}

// zipReaderFor wraps an open file for archive/zip, which needs a ReaderAt.
func zipReaderFor(f interface {
	ReadAt([]byte, int64) (int, error)
}, size int64) (*zip.Reader, error) {
	return zip.NewReader(readerAt{f}, size)
}

type readerAt struct {
	f interface {
		ReadAt([]byte, int64) (int, error)
	}
}

func (r readerAt) ReadAt(p []byte, off int64) (int, error) { return r.f.ReadAt(p, off) }
