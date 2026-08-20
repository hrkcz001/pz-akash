// Package akash talks to the Akash Console managed-wallet API: the same REST
// surface the v1 bash controller drove through curl, behind a typed client.
//
// The managed wallet is why there is no cosmos SDK here. Console holds the key
// and signs on our behalf, so every operation — deploy, lease, close, top up —
// is an authenticated HTTP call with an x-api-key header. That key is the only
// secret in this package and it is never logged, not even truncated.
//
// Two habits in here are deliberate and load-bearing:
//
//   - Every call has a deadline, from config, and none is unbounded. A deploy
//     that hangs on a socket is worse than one that fails: the FSM cannot tell
//     "still working" from "wedged", and money keeps burning in escrow.
//   - Responses are decoded into narrow structs with the fields we actually
//     read. The API belongs to Console and gains fields without asking us;
//     ignoring the ones we do not know about is the only stable posture.
package akash

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxResponseBytes bounds what we will read from a single response. Provider
// lists run to a few hundred KiB; anything past this is a bug or a hostile
// endpoint, and either way it must not become the controller's memory profile.
const maxResponseBytes = 16 << 20

// maxErrorBody is how much of a failing response we keep for the message. Enough
// to carry an API error string, short enough to log.
const maxErrorBody = 512

// Client is a Console API client. It is safe for concurrent use.
type Client struct {
	base string
	key  string
	hc   *http.Client

	logf  func(string, ...any)
	sleep func(context.Context, time.Duration) error

	retries   int
	retryWait time.Duration
}

// Options configures a Client. Only APIBase and APIKey are required.
type Options struct {
	APIBase string
	APIKey  string

	// HTTP is the transport. Nil gets a client with no global timeout — per-call
	// deadlines come from the context, because a deploy and a status poll do not
	// deserve the same patience.
	HTTP *http.Client

	Logf func(string, ...any)

	// Retries is how many times a retryable failure (429, 5xx, network) is
	// tried again; 0 means the default. RetryWait is the base backoff, doubled
	// per attempt, and overridden by a Retry-After header when the API sends one.
	Retries   int
	RetryWait time.Duration

	// sleep is overridden in tests so a backoff does not cost wall-clock time.
	sleep func(context.Context, time.Duration) error
}

// New builds a Client.
func New(o Options) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(o.APIBase), "/")
	if base == "" {
		return nil, errors.New("akash: api_base is empty")
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return nil, fmt.Errorf("akash: api_base %q is not an http(s) URL", base)
	}
	if strings.TrimSpace(o.APIKey) == "" {
		return nil, errors.New("akash: api key is empty")
	}

	c := &Client{
		base:      base,
		key:       o.APIKey,
		hc:        o.HTTP,
		logf:      o.Logf,
		sleep:     o.sleep,
		retries:   o.Retries,
		retryWait: o.RetryWait,
	}
	if c.hc == nil {
		c.hc = &http.Client{}
	}
	if c.logf == nil {
		c.logf = func(string, ...any) {}
	}
	if c.sleep == nil {
		c.sleep = sleepCtx
	}
	if c.retries <= 0 {
		c.retries = 4
	}
	if c.retryWait <= 0 {
		c.retryWait = 2 * time.Second
	}
	return c, nil
}

// APIError is a non-2xx response. It carries the status so callers can tell a
// closed deployment (404, often fine) from a rejected one (400, never fine).
type APIError struct {
	Method string
	Path   string
	Status int
	Body   string
	// Attempts is how many times the call was made before giving up.
	Attempts int
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("akash: %s %s: HTTP %d", e.Method, e.Path, e.Status)
	if e.Attempts > 1 {
		msg += fmt.Sprintf(" after %d attempts", e.Attempts)
	}
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// NotFound reports whether err is an APIError with status 404. Closing a
// deployment that is already gone is a success, not a failure, and the FSM has
// to be able to say so without string-matching.
func NotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// Status returns the HTTP status behind err, or 0 if err is not an APIError.
func Status(err error) int {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// retryable reports whether a status is worth trying again. A 429 is the
// documented rate limit; 5xx and 408 are the provider's problem, not ours. Every
// other 4xx means the request itself was wrong, and repeating it just spends
// quota to get the same answer.
func retryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusRequestTimeout,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
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

// retryAfter reads the Retry-After header, which the API sends on 429 as a
// number of seconds. Honouring it is politer and more effective than our own
// backoff guess.
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
