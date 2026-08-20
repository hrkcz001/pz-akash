package dns

// The HTTP layer. Cloudflare wraps every answer in an envelope carrying its own
// success flag, so there are two ways for a call to fail — the status line and the
// body — and v1 checked the body's flag without ever logging what it said. That is
// how a zone could quietly stop being updated: `upsert_dns_record` returned False,
// nobody looked at the return value, and the boot log said nothing at all.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxResponseBytes bounds one response. A record list for a small zone is a few
// KiB; this is three orders of magnitude of headroom and still a bound.
const maxResponseBytes = 4 << 20

// maxErrorBody is how much of a failing body is kept for the message.
const maxErrorBody = 512

// envelope is the shape every Cloudflare v4 response shares. Result is left raw
// so each call decodes only the fields it reads: the API gains fields without
// asking us, and ignoring the unknown ones is the only stable posture.
type envelope struct {
	Success  bool            `json:"success"`
	Errors   []apiMessage    `json:"errors"`
	Messages []apiMessage    `json:"messages"`
	Result   json.RawMessage `json:"result"`
}

type apiMessage struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// APIError is a call Cloudflare refused, whether it said so with a status code or
// with success:false. Status is 0 for the latter, which is the case that has to be
// distinguishable: a 200 that changed nothing is the failure mode v1 could not see.
type APIError struct {
	Method   string
	Path     string
	Status   int
	Messages []apiMessage
	Body     string
	Attempts int
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("cloudflare: %s %s", e.Method, e.Path)
	if e.Status != 0 {
		msg += fmt.Sprintf(": HTTP %d", e.Status)
	} else {
		msg += ": refused"
	}
	if e.Attempts > 1 {
		msg += fmt.Sprintf(" after %d attempts", e.Attempts)
	}
	switch {
	case len(e.Messages) > 0:
		parts := make([]string, 0, len(e.Messages))
		for _, m := range e.Messages {
			parts = append(parts, fmt.Sprintf("%d %s", m.Code, m.Message))
		}
		msg += ": " + strings.Join(parts, "; ")
	case e.Body != "":
		msg += ": " + e.Body
	}
	return msg
}

// Code reports whether err carries a particular Cloudflare error code. 81057 is
// "record already exists" and 81044 is "record does not exist"; both describe a
// state we were trying to reach, so a caller has to be able to tell them from a
// real failure without matching on prose.
func Code(err error, code int) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	for _, m := range ae.Messages {
		if m.Code == code {
			return true
		}
	}
	return false
}

// Status returns the HTTP status behind err, or 0 when err is not an APIError or
// the refusal came in the body.
func Status(err error) int {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// do performs one API call: method on path, sending in as JSON when non-nil and
// decoding the envelope's result into out when out is non-nil.
func (c *Cloudflare) do(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return fmt.Errorf("cloudflare: encoding %s %s: %w", method, path, err)
		}
	}

	// Every call is bounded here rather than at the call site. A DNS update is
	// never worth blocking a deploy on, and an unbounded one could.
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	url := c.base + path
	var last error
	for attempt := 1; ; attempt++ {
		// Rebuilt per attempt: a reader consumed by a failed request has nothing
		// left to send, and a silently empty PUT would blank a ruleset.
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, rdr)
		if err != nil {
			return fmt.Errorf("cloudflare: building %s %s: %w", method, path, err)
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		wait, err := c.attempt(req, out)
		if err == nil {
			return nil
		}
		last = err

		// Negative means "not retryable": hand back the real error rather than
		// spending the remaining attempts on a request Cloudflare rejected.
		if wait < 0 || attempt > c.retries || ctx.Err() != nil {
			return finalize(last, method, path, attempt)
		}
		if wait == 0 {
			wait = c.backoff(attempt)
		}
		c.logf("cloudflare: %s %s failed (%v), retry %d/%d in %v", method, path, err, attempt, c.retries, wait)
		if err := c.sleep(ctx, wait); err != nil {
			return finalize(last, method, path, attempt)
		}
	}
}

// attempt makes one request. The returned duration says what to do next: negative
// means do not retry, zero means retry after our own backoff, positive is a wait
// the server asked for.
func (c *Cloudflare) attempt(req *http.Request, out any) (time.Duration, error) {
	resp, err := c.hc.Do(req)
	if err != nil {
		if req.Context().Err() != nil {
			return -1, err
		}
		return 0, err
	}
	defer resp.Body.Close()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))

	if resp.StatusCode/100 != 2 {
		e := &APIError{
			Method: req.Method, Path: req.URL.Path,
			Status: resp.StatusCode, Body: snip(string(raw)),
		}
		// The body usually carries the reason even on a 4xx, and the reason is what
		// tells "the token lacks Zone.DNS" from "the name is malformed".
		var env envelope
		if json.Unmarshal(raw, &env) == nil {
			e.Messages = env.Errors
		}
		if !retryable(resp.StatusCode) {
			return -1, e
		}
		if d, ok := retryAfter(resp.Header); ok && d > 0 {
			return d, e
		}
		return 0, e
	}
	if readErr != nil {
		if req.Context().Err() != nil {
			return -1, readErr
		}
		// Truncated transfer, not a schema problem: worth another try.
		return 0, fmt.Errorf("reading response: %w", readErr)
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// A body that does not fit will not fit on the retry either.
		return -1, fmt.Errorf("decoding %s: %w (body: %s)", req.URL.Path, err, snip(string(raw)))
	}
	if !env.Success {
		// A 200 that refused. This is the case v1 dropped on the floor.
		return -1, &APIError{
			Method: req.Method, Path: req.URL.Path,
			Messages: env.Errors, Body: snip(string(raw)),
		}
	}
	if out == nil {
		return 0, nil
	}
	if len(env.Result) == 0 {
		return 0, nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return -1, fmt.Errorf("decoding %s result: %w (body: %s)", req.URL.Path, err, snip(string(raw)))
	}
	return 0, nil
}

func finalize(err error, method, path string, attempts int) error {
	var ae *APIError
	if errors.As(err, &ae) {
		ae.Attempts = attempts
		return ae
	}
	if attempts > 1 {
		return fmt.Errorf("cloudflare: %s %s after %d attempts: %w", method, path, attempts, err)
	}
	return fmt.Errorf("cloudflare: %s %s: %w", method, path, err)
}

// backoff doubles per attempt and stops growing at half a minute — the whole
// sequence has to fit inside the patience of whoever is waiting on a deploy.
func (c *Cloudflare) backoff(attempt int) time.Duration {
	d := c.retryWait
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= 30*time.Second {
			return 30 * time.Second
		}
	}
	return d
}

// retryable reports whether a status is worth trying again. 429 is the documented
// rate limit and Cloudflare's is per-zone, so it happens; 5xx is their problem.
// Every other 4xx means the request was wrong, and repeating it buys the same
// answer for another unit of quota.
func retryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusRequestTimeout,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func retryAfter(h http.Header) (time.Duration, bool) {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	if when, err := http.ParseTime(v); err == nil {
		return time.Until(when), true
	}
	return 0, false
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// snip collapses a body to one short line, for a log an operator reads a line at
// a time.
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
