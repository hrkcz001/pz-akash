package fsm

// Finding out where we are.
//
// The controller is deployed by submitting an SDL and then learning, minutes later,
// what host and port the provider chose for it. Nothing can put that answer into the
// container's environment, because it does not exist until after the container has
// been scheduled. So the controller has always published only the DNS name — and the
// DNS name goes through Cloudflare, whose free plan refuses a request body over
// 100 MB with a 413.
//
// That is not a corner case. A backup upload is one large request body, so a world
// big enough to be worth backing up is a world whose backup silently cannot be
// uploaded. The fix is for the controller to ask Akash where it was put, publish that
// alongside the name, and let bulk traffic use it — see state.URLs.Direct.

import (
	"context"
	"strings"
	"time"
)

// selfLookInterval bounds how often discoverSelfURL asks. It is not configurable
// because there is nothing to tune: the value is looked up once and then never again,
// so the interval only governs how fast a controller retries while its own lease is
// still becoming routable.
const selfLookInterval = 60 * time.Second

// discoverSelfURL resolves the direct route to this controller, once.
//
// Called from the housekeeping tick rather than from advance, for the same reason
// topUpEscrow is: advance is a function of the documents and the clock, and putting a
// network call inside it would make the lifecycle's behaviour depend on whether an
// API answered.
//
// It stops asking as soon as it has an answer. A lease's address does not change
// while the lease lives, and if the lease dies this process dies with it.
func (m *Machine) discoverSelfURL(ctx context.Context) {
	switch {
	case m.selfURL != "":
		return
	case m.ctlURL != "":
		// An operator passed --controller-url. That is a deliberate override of exactly
		// this value, and asking the API to second-guess it would be both wasteful and
		// wrong.
		return
	case !m.lastSelfLook.IsZero() && m.now().Sub(m.lastSelfLook) < selfLookInterval:
		return
	}
	// Stamped before the call, so a failing lookup waits a full interval rather than
	// retrying on every tick against an API that is already unhappy.
	m.lastSelfLook = m.now()

	url, err := m.akash.SelfURL(ctx)
	if err != nil {
		// Logged and dropped. The published Public URL still works for everything
		// except a very large upload, so this degrades rather than breaks — and it is
		// retried on the next interval.
		m.logf("fsm: could not establish our own address: %v", err)
		return
	}
	if url = strings.TrimSpace(url); url == "" {
		// The ordinary answer when the lease is not routable yet, and on a laptop.
		return
	}
	m.selfURL = url
	m.logf("fsm: direct route to this controller is %s (bulk traffic bypasses the proxy)", url)
	// resolveURLs is what publishes it, and the caller runs it on every event; calling
	// it here means the document carries the address on this pass rather than the next.
	m.resolveURLs()
}
