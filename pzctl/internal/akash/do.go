package akash

// do is the one place an HTTP request is made. Every API method goes through it,
// which is what makes "no call without a deadline" and "retry only what is worth
// retrying" properties of the package rather than of each call site.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// do performs method on path (relative to the API base), sending in as JSON when
// it is non-nil and decoding the response into out when out is non-nil.
//
// A nil out with a 2xx response is a successful call whose body we do not care
// about; the body is still drained so the connection can be reused.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return fmt.Errorf("akash: encoding %s %s request: %w", method, path, err)
		}
	}

	url := c.base + path
	var last error

	for attempt := 1; ; attempt++ {
		// The body has to be rebuilt per attempt: a reader consumed by a failed
		// request has nothing left to send on the retry, and a silently empty
		// POST body is a deploy that succeeds with the wrong SDL.
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, rdr)
		if err != nil {
			return fmt.Errorf("akash: building %s %s: %w", method, path, err)
		}
		req.Header.Set("x-api-key", c.key)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		wait, err := c.attempt(req, out)
		if err == nil {
			return nil
		}
		last = err

		// wait < 0 means "not retryable": give the caller the real error rather
		// than burning the remaining attempts on a request the API rejected.
		if wait < 0 || attempt > c.retries {
			return c.finalize(last, method, path, attempt)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return c.finalize(last, method, path, attempt)
		}
		if wait == 0 {
			wait = c.backoff(attempt)
		}
		c.logf("akash: %s %s failed (%v), retry %d/%d in %v", method, path, err, attempt, c.retries, wait)
		if err := c.sleep(ctx, wait); err != nil {
			return c.finalize(last, method, path, attempt)
		}
	}
}

// attempt makes one request. The returned duration says what to do next: a
// negative value means do not retry, zero means retry after our own backoff, and
// a positive value is a wait the server asked for.
func (c *Client) attempt(req *http.Request, out any) (time.Duration, error) {
	resp, err := c.hc.Do(req)
	if err != nil {
		// A transport error is retryable unless the context is what killed it —
		// retrying a cancelled call cannot succeed and delays the shutdown.
		if req.Context().Err() != nil {
			return -1, err
		}
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		snippet := readSnippet(resp.Body)
		apiErr := &APIError{
			Method: req.Method, Path: req.URL.Path,
			Status: resp.StatusCode, Body: snippet,
		}
		if !retryable(resp.StatusCode) {
			return -1, apiErr
		}
		if d, ok := retryAfter(resp.Header); ok && d > 0 {
			return d, apiErr
		}
		return 0, apiErr
	}

	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		return 0, nil
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		if req.Context().Err() != nil {
			return -1, err
		}
		// A truncated body is a transport failure, not a schema failure, so it
		// gets another try rather than a decode error the operator cannot act on.
		return 0, fmt.Errorf("reading response: %w", err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		// A body that does not fit the schema will not fit it on the retry
		// either. Quote a little of it: this is how the v1 mystery of
		// "Expecting value: line 1 column 106" stayed a mystery for so long.
		return -1, fmt.Errorf("decoding %s response: %w (body: %s)", req.URL.Path, err, snip(string(raw)))
	}
	return 0, nil
}

// finalize attaches the attempt count to an APIError so the message says whether
// we gave up immediately or after a fight.
func (c *Client) finalize(err error, method, path string, attempts int) error {
	var ae *APIError
	if errors.As(err, &ae) {
		ae.Attempts = attempts
		return ae
	}
	if attempts > 1 {
		return fmt.Errorf("akash: %s %s after %d attempts: %w", method, path, attempts, err)
	}
	return fmt.Errorf("akash: %s %s: %w", method, path, err)
}

// backoff doubles per attempt and stops growing at a minute. Capping matters
// because the caller's deadline is the real bound: an uncapped backoff would
// spend the whole deploy window asleep.
func (c *Client) backoff(attempt int) time.Duration {
	d := c.retryWait
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= time.Minute {
			return time.Minute
		}
	}
	return d
}

func readSnippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, maxErrorBody+1))
	return snip(string(b))
}

// snip collapses a body to one short line. Newlines out, because these end up in
// a log the operator reads a line at a time.
func snip(s string) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s))
	if len(s) > maxErrorBody {
		return s[:maxErrorBody] + "…"
	}
	return s
}
