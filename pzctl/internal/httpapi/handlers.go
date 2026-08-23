package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// packageHandler serves one file out of packages_dir.
//
// The public ones are served with http.ServeContent, which brings Range support
// and conditional requests for free — worth having, because client.zip is what
// players download and a resumable transfer over a bad connection is the
// difference between a player joining and a player giving up.
//
// server.zip cannot have any of that. Its bytes are generated per request, so
// there is no stable ETag, no meaningful Last-Modified, and a Range would name an
// offset in a body that does not exist yet. It gets a plain 200 and a chunked
// body, which the agent's downloader already tolerates: it only compares against
// Content-Length when one was sent.
func (s *Server) packageHandler(p staticFile) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			methodNotAllowed(w, http.MethodGet, http.MethodHead)
			return
		}
		if !s.guard.requireRealm(w, r, p.realm) {
			return
		}

		f, info, err := openPackage(s.packagesDir, p)
		if err != nil {
			s.fail(w, r, statusFor(err), err)
			return
		}
		defer f.Close()

		w.Header().Set("Content-Type", p.mime)
		// Only where the saved name must differ from the URL's. Set on a 304 as well
		// as a 200, which costs nothing and keeps the header from depending on whether
		// the client happened to have a cached copy.
		if p.downloadAs != "" && p.downloadAs != p.fileName {
			w.Header().Set("Content-Disposition", `attachment; filename="`+p.downloadAs+`"`)
		}

		if !p.substitute || !s.sub.Active() {
			// ServeContent sets Content-Length, handles Range and HEAD, and returns
			// 304 for a matching If-Modified-Since.
			http.ServeContent(w, r, p.fileName, info.ModTime(), f)
			return
		}

		zr, err := zipReaderFor(f, info.Size())
		if err != nil {
			// A corrupt or truncated package in the image. 500 is right — this is the
			// controller's own file, not anything the caller did.
			s.fail(w, r, http.StatusInternalServerError, err)
			return
		}

		// No Content-Length: the rewritten size is not known until it has been
		// written. Go will use chunked encoding.
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		// Once a byte is written the status is committed, so a failure part-way
		// through cannot become an error response. Abandoning the connection without
		// a final chunk is what tells the client the body is incomplete — which is
		// the honest signal, and better than a 200 that ends early and unzips to a
		// config file with placeholders still in it.
		if err := s.sub.Rewrite(w, zr); err != nil {
			s.logf("http: %s %s failed mid-body, connection abandoned: %v",
				r.Method, r.URL.Path, err)
			panic(http.ErrAbortHandler)
		}
	}
}

// handleBackup is GET (download) and PUT (upload) for one archive.
func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	name, ok := BackupName(r.URL.Path)
	if !ok {
		// Everything BackupName rejects: the bare directory, a nested path, "..".
		// Rejecting the shape here means no handler below ever joins an
		// attacker-controlled string onto a directory.
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if !s.guard.requireRealm(w, r, RealmBackups) {
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.downloadBackup(w, r, name)
	case http.MethodPut:
		s.uploadBackup(w, r, name)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodHead, http.MethodPut)
	}
}

func (s *Server) downloadBackup(w http.ResponseWriter, r *http.Request, name string) {
	f, entry, err := s.store.Open(name)
	if err != nil {
		s.fail(w, r, statusFor(err), err)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	// The digest travels on the download too, so an operator can verify the copy on
	// their laptop against the same value the agent verified against. It is the one
	// header that makes "I have a backup" checkable rather than assumed.
	if entry != nil && entry.SHA256 != "" {
		w.Header().Set(HeaderSHA256, entry.SHA256)
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)

	// Marked before serving, not after. A download that starts and is interrupted
	// still means a copy is being taken; marking on completion would need the
	// handler to distinguish a client that gave up from one that finished, which
	// over a chunked response it cannot do reliably. The stamp answers "has anyone
	// ever fetched this", and over-reporting there costs a warning that is absent
	// while under-reporting costs an archive deleted as already-saved.
	//
	// Range requests are the exception: a partial fetch is what a resuming client
	// does, and counting it would mark an archive nobody has whole.
	if r.Method == http.MethodGet && r.Header.Get("Range") == "" {
		s.store.MarkDownloaded(name)
	}
	http.ServeContent(w, r, name, info.ModTime(), f)
}

func (s *Server) uploadBackup(w http.ResponseWriter, r *http.Request, name string) {
	ctx := r.Context()
	if s.uploadLimit > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.uploadLimit)
		defer cancel()

		// The context alone bounds nothing. A deadline fires on a timer, but the
		// handler is sitting inside a socket read that no timer interrupts, so it
		// keeps waiting until the sender gives up — which for a hung sender is
		// never. SetReadDeadline pushes the same limit down to the connection,
		// where it can actually break the read; ctxReader below then reports the
		// result as the cancellation rather than as a torn connection.
		if err := http.NewResponseController(w).SetReadDeadline(s.now().Add(s.uploadLimit)); err != nil {
			// Some wrapped ResponseWriters cannot. Worth saying out loud: it means
			// upload_timeout is advisory on this connection, and a stalled upload
			// will hold a handler until the client disconnects.
			s.logf("backups: this connection takes no read deadline, so upload_timeout "+
				"is advisory for %s: %v", name, err)
		}
	}

	// Request-Id and Phase are the agent's, and they are the fix for bug 4: v1's
	// controller could not tell the backup it had asked for from one that happened
	// to arrive, so a halt would wait for a report it had already been sent. The
	// FSM matches this against its outstanding backup_request.id.
	requestID := r.Header.Get(HeaderRequestID)
	phase := r.Header.Get(HeaderPhase)
	wantSHA := strings.TrimSpace(r.Header.Get(HeaderSHA256))

	// A digest is not required, because an operator uploading an archive from their
	// laptop with curl has no easy way to compute one first and refusing them would
	// break the one restore path that exists when the disk is gone. The agent always
	// sends it, and a missing digest is logged so the difference is visible.
	if wantSHA == "" {
		s.logf("backups: %s is being uploaded without a digest (request %q, phase %q)",
			name, requestID, phase)
	}

	// Asked before the write, because after it the answer is always yes.
	replaced := s.store.Has(name)

	entry, err := s.store.Put(name, bodyWithContext(ctx, r), r.ContentLength, wantSHA)
	if err != nil {
		if timedOut(err, ctx) {
			s.logf("backups: upload of %s ran past the %s limit", name, s.uploadLimit)
		}
		s.fail(w, r, statusFor(err), err)
		return
	}

	verb := "stored"
	if replaced {
		verb = "replaced"
	}
	s.logf("backups: %s %s (%d bytes, request %q, phase %q)",
		verb, entry.Name, entry.Size, requestID, phase)

	body, err := json.Marshal(UploadResult{
		Name:   entry.Name,
		Size:   entry.Size,
		SHA256: entry.SHA256,
	})
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	body = append(body, '\n')
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	// 201 for a new archive, 200 for one that replaced an existing name. Both are
	// success — PUT is idempotent, and a retry after a half-finished upload is
	// supposed to work — but the difference is what lets a caller tell a retry that
	// landed twice from two archives it thought it had.
	code := http.StatusCreated
	if replaced {
		code = http.StatusOK
	}
	w.WriteHeader(code)
	w.Write(body)
}

// timedOut reports whether err is this handler's own deadline rather than
// anything the sender did. There are two shapes of the same event: the socket
// read deadline surfaces as os.ErrDeadlineExceeded, and a cancellation noticed
// between reads surfaces as the context error.
func timedOut(err error, ctx context.Context) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	ctxErr := ctx.Err()
	return ctxErr != nil && errors.Is(err, ctxErr)
}

// bodyWithContext makes the body read fail when ctx expires.
//
// This is the second half of the pair with SetReadDeadline above. The deadline
// is what breaks a read already in progress; this is what stops the next one
// from starting after a client has gone away, and what makes the error say
// "cancelled" rather than reporting whatever the torn connection looked like.
func bodyWithContext(ctx context.Context, r *http.Request) *ctxReader {
	return &ctxReader{ctx: ctx, r: r.Body}
}

type ctxReader struct {
	ctx context.Context
	r   interface{ Read([]byte) (int, error) }
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.r.Read(p)
	// A cancellation that landed during the read is reported as the cancellation,
	// not as whatever the torn connection looked like. The distinction reaches the
	// log line that says the upload ran past its limit.
	if err != nil {
		if ctxErr := c.ctx.Err(); ctxErr != nil {
			return n, ctxErr
		}
	}
	return n, err
}
