package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/httpapi"
	"github.com/hrkcz001/pz-akash/pzctl/internal/secrets"
)

// Client talks to the controller's HTTP service: the two server-file packages on
// boot, a backup on restore, and the finished archive on upload.
//
// Every call is retried, because the agent and the controller boot at the same
// time on different providers — the controller not answering yet is the normal
// case, not an error. v1 waited six seconds for /healthz and then tried three
// times, and a slow controller start meant a server that booted with no config.
//
// It holds more than one base URL, in preference order, and rotates through them
// across retry attempts. That is the fix for a specific dead end: the preferred
// route is the provider's own host:port, which avoids Cloudflare — whose free plan
// answers 413 to a request body over 100 MB, making a large backup impossible to
// upload through the proxied name — but that address is discovered at runtime and
// could be stale. Rotating means a wrong first base costs one attempt instead of
// the whole operation. The direct route is plaintext http, so the realm password
// travels in the clear on the provider's network; that is the deliberate trade
// recorded in config.yaml, and the proxied name stays the one people use.
type Client struct {
	bases   []string
	hc      *http.Client
	tokens  map[httpapi.Realm]string
	retries int
	budget  time.Duration
	logf    func(string, ...any)
}

// NewClient builds a client for one or more controller base URLs
// ("http://host:port"), most preferred first. Empty entries and duplicates are
// dropped, so a caller can pass Direct() and Base() without checking whether they
// differ.
func NewClient(bases []string, sec *secrets.Set, a config.Agent, logf func(string, ...any)) *Client {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	tokens := map[httpapi.Realm]string{}
	if sec != nil {
		tokens[httpapi.RealmServerFiles] = sec.ServerFilesPassword
		tokens[httpapi.RealmBackups] = sec.BackupsPassword
	}
	var clean []string
	seen := map[string]bool{}
	for _, b := range bases {
		b = strings.TrimRight(strings.TrimSpace(b), "/")
		if b == "" || seen[b] {
			continue
		}
		seen[b] = true
		clean = append(clean, b)
	}
	return &Client{
		bases:  clean,
		tokens: tokens,
		// No client-wide timeout: it would apply to the whole body transfer, and
		// a backup upload legitimately takes many minutes. The per-attempt bound
		// is the context, and a stalled connection is caught by the header and
		// idle timeouts of the transport.
		hc: &http.Client{Transport: &http.Transport{
			ResponseHeaderTimeout: 60 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			DisableCompression:    true, // zips; compressing them again wastes CPU
		}},
		retries: a.RestoreDownloadRetries,
		budget:  a.RestoreDownloadTimeout.D(),
		logf:    logf,
	}
}

// Base is the controller URL this client prefers, for logs and for anything that
// needs one name for the controller.
func (c *Client) Base() string {
	if len(c.bases) == 0 {
		return ""
	}
	return c.bases[0]
}

// statusError is an HTTP response the controller answered with. It is a distinct
// type so callers can separate "will never work" (401, 404) from "not yet"
// (502 while the controller is still booting).
type statusError struct {
	Code int
	Body string
	URL  string
}

func (e *statusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("%s: HTTP %d", e.URL, e.Code)
	}
	return fmt.Sprintf("%s: HTTP %d: %s", e.URL, e.Code, e.Body)
}

// permanent reports whether retrying could plausibly change the answer. A 401 is
// a wrong password and a 404 is a backup that does not exist; retrying either
// only delays the report by retries×backoff.
func (e *statusError) permanent() bool {
	switch e.Code {
	case http.StatusTooManyRequests, http.StatusRequestTimeout:
		return false
	}
	return e.Code >= 400 && e.Code < 500
}

// IsNotFound reports whether err is a 404 from the controller. A restore target
// that is not on the controller's disk is a configuration problem the operator
// has to see, not something to retry for half an hour.
func IsNotFound(err error) bool {
	var se *statusError
	return errors.As(err, &se) && se.Code == http.StatusNotFound
}

// retry runs attempt until it succeeds, the budget expires, or the error is
// permanent. The backoff is capped so a long budget does not turn into one very
// long sleep at the end.
//
// Each attempt is handed the base URL to use, rotating through the client's bases:
// attempt 1 gets the preferred route, attempt 2 the next, and so on, wrapping. So a
// direct address that has gone stale costs one attempt rather than the call, and a
// single-base client behaves exactly as it did before there were two.
func (c *Client) retry(ctx context.Context, what string, attempt func(ctx context.Context, base string) error) error {
	if len(c.bases) == 0 {
		return fmt.Errorf("%s: no controller URL to try", what)
	}
	ctx, cancel := context.WithTimeout(ctx, c.budget)
	defer cancel()

	backoff := 2 * time.Second
	var last error
	for i := 1; i <= c.retries; i++ {
		base := c.bases[(i-1)%len(c.bases)]
		err := attempt(ctx, base)
		if err == nil {
			if i > 1 && base != c.Base() {
				c.logf("%s: succeeded via %s after %s failed", what, base, c.Base())
			}
			return nil
		}
		last = err

		var se *statusError
		if errors.As(err, &se) && se.permanent() {
			// One exception, and it is the reason the rotation exists. 413 is
			// Cloudflare refusing a body over its free-plan limit, not the controller
			// rejecting the request — a different route can carry it, so this
			// permanent-looking answer is retried when there is somewhere else to go.
			if se.Code == http.StatusRequestEntityTooLarge && len(c.bases) > 1 {
				c.logf("%s: %s refused the body size; trying another route", what, base)
			} else {
				return err
			}
		}
		if ctx.Err() != nil {
			return fmt.Errorf("%s: %w (last error: %v)", what, ctx.Err(), err)
		}
		if i == c.retries {
			break
		}
		c.logf("%s: attempt %d/%d failed: %v; retrying in %v", what, i, c.retries, err, backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return fmt.Errorf("%s: %w (last error: %v)", what, ctx.Err(), err)
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
	return fmt.Errorf("%s: gave up after %d attempts: %w", what, c.retries, last)
}

// WaitHealthy blocks until the controller answers /healthz. The agent calls it
// before anything else so a failure reads as "controller unreachable" rather
// than as a mysterious failure to download config.
func (c *Client) WaitHealthy(ctx context.Context) error {
	return c.retry(ctx, "controller health", func(ctx context.Context, base string) error {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		return c.do(ctx, base, http.MethodGet, httpapi.PathHealth, httpapi.RealmPublic, nil, func(*http.Response) error { return nil })
	})
}

// Download fetches path into dst, retrying. It writes to a .part file and
// renames on success, so an interrupted download can never be mistaken for a
// complete file by the code that unpacks it.
func (c *Client) Download(ctx context.Context, path string, realm httpapi.Realm, dst string) (int64, error) {
	var size int64
	err := c.retry(ctx, "download "+path, func(ctx context.Context, base string) error {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		tmp := dst + ".part"
		f, err := os.Create(tmp)
		if err != nil {
			return err
		}
		defer func() {
			f.Close()
			if size == 0 {
				os.Remove(tmp)
			}
		}()

		size = 0
		err = c.do(ctx, base, http.MethodGet, path, realm, nil, func(resp *http.Response) error {
			n, err := io.Copy(f, resp.Body)
			if err != nil {
				return err
			}
			// A truncated transfer with a declared length is a failure, not a
			// short file: unzip would report a corrupt archive instead.
			if resp.ContentLength >= 0 && n != resp.ContentLength {
				return fmt.Errorf("short read: %d of %d bytes", n, resp.ContentLength)
			}
			size = n
			return nil
		})
		if err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		return os.Rename(tmp, dst)
	})
	if err != nil {
		return 0, err
	}
	return size, nil
}

// UploadBackup streams src to the controller as name. sha256 is the hex digest
// of the file; the controller verifies it while writing and rejects a mismatch,
// so a torn upload never becomes a backup that fails only at restore time.
//
// The body is re-opened per attempt rather than buffered, which is what lets a
// multi-gigabyte archive be retried without holding it in memory.
func (c *Client) UploadBackup(ctx context.Context, name, src, sha256hex, requestID, phase string) (*httpapi.UploadResult, error) {
	st, err := os.Stat(src)
	if err != nil {
		return nil, err
	}
	var out httpapi.UploadResult
	err = c.retry(ctx, "upload "+name, func(ctx context.Context, base string) error {
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()

		req, err := http.NewRequestWithContext(ctx, http.MethodPut, base+httpapi.BackupPath(name), f)
		if err != nil {
			return err
		}
		// Setting ContentLength explicitly keeps the request identity-encoded.
		// Chunked would work, but it denies the controller the one cheap check it
		// can make before writing anything: does this fit on the disk.
		req.ContentLength = st.Size()
		req.Header.Set("Content-Type", "application/zip")
		req.Header.Set(httpapi.HeaderSHA256, sha256hex)
		req.Header.Set(httpapi.HeaderRequestID, requestID)
		req.Header.Set(httpapi.HeaderPhase, phase)
		httpapi.SetAuth(req, c.tokens[httpapi.RealmBackups])

		resp, err := c.hc.Do(req)
		if err != nil {
			return err
		}
		defer drain(resp)
		if resp.StatusCode/100 != 2 {
			return statusFrom(resp)
		}
		out = httpapi.UploadResult{Name: name, Size: st.Size(), SHA256: sha256hex}
		_ = decodeJSON(resp, &out)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out.SHA256 != "" && !strings.EqualFold(out.SHA256, sha256hex) {
		return nil, fmt.Errorf("upload %s: controller stored digest %s, sent %s", name, out.SHA256, sha256hex)
	}
	return &out, nil
}

// do issues one request against base and hands the response to fn while the body
// is open.
func (c *Client) do(ctx context.Context, base, method, path string, realm httpapi.Realm, body io.Reader, fn func(*http.Response) error) error {
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return err
	}
	httpapi.SetAuth(req, c.tokens[realm])
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode/100 != 2 {
		return statusFrom(resp)
	}
	return fn(resp)
}

// drain closes the body after reading a bounded amount, so the connection can be
// reused instead of being torn down after every error response.
func drain(resp *http.Response) {
	io.CopyN(io.Discard, resp.Body, 4<<10)
	resp.Body.Close()
}

// decodeJSON reads a small JSON body. The bound matters: this runs against a
// response the agent has already accepted, and an unbounded decode of a body the
// controller controls is a way to lose the agent to a memory spike.
func decodeJSON(resp *http.Response, dst any) error {
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(dst)
}

func statusFrom(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
	return &statusError{
		Code: resp.StatusCode,
		Body: strings.TrimSpace(string(b)),
		URL:  resp.Request.URL.Redacted(),
	}
}
