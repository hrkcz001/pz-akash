// Package webhook receives GitHub push deliveries and reduces them to a single
// question: is there any reason to go and read the triggers directory?
//
// Two properties are worth stating up front, because they are what make this
// component safe to get wrong.
//
// The receiver is a latency optimisation, not a source of truth. Everything it
// can conclude, the controller's poll loop concludes on its own within
// controller.poll.idle. So the failure mode of a missed delivery is a slower
// reaction, never a lost request — which is why the filters below are allowed to
// be conservative in the direction of acting, and why an unparseable payload is
// answered with "reconcile anyway" rather than an error.
//
// Nothing here mutates state or decides policy. It authenticates the sender,
// answers the question, and calls back. The dedup ring that stops a delivery from
// being acted on twice lives in the controller's document, because it has to
// survive a restart and this package holds nothing across one.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// MaxBodyDefault bounds the payload we will read. GitHub's push payloads run to
// tens of kilobytes; a megabyte is generous and stops an unauthenticated caller
// from making us allocate. The limit is applied before the signature is checked,
// because we have to hold the body to check it.
const MaxBodyDefault = 1 << 20

// Push is a push delivery reduced to what a decision needs.
type Push struct {
	// Ref is the full ref name, e.g. refs/heads/main.
	Ref string
	// Before and After are the branch tips. After is the SHA the controller
	// records in its dedup ring.
	Before, After string
	// Pusher is the GitHub account that pushed, and Author the git identity of
	// the head commit. Both are recorded; see Options.IgnoreAuthors for why
	// neither is filtered on by default.
	Pusher, Author string
	// Paths is the union of added, modified and removed paths across the commits
	// GitHub chose to include. It is empty when the delivery carried no file
	// list — a large push, or a payload shape we do not recognise.
	Paths []string
	// Created and Deleted mark branch creation and deletion.
	Created, Deleted bool
}

// Decision is the outcome of the filter chain, with the reason spelled out. The
// reason is returned to GitHub in the response body, so the delivery log in the
// repository settings answers "why did my trigger not fire" without needing our
// logs at all.
type Decision struct {
	Act    bool
	Reason string
}

// Options configures a Handler. Everything except Secret comes from config.
type Options struct {
	// Secret is the shared HMAC key, from PZ_WEBHOOK_SECRET. An empty secret is
	// refused at construction: an unauthenticated endpoint that can start an
	// Akash deployment is a funded stranger's toy.
	Secret string

	// Branch is the operator branch. A delivery for any other ref carries no
	// operator intent — in particular the state branches, whose every publish
	// generates a delivery. In v1 state and triggers shared one branch, so each
	// status write looked exactly like a human request; that is the loop.
	Branch string

	// TriggersDir is the only directory whose changes are interesting.
	TriggersDir string

	// WatchPaths are individual paths outside TriggersDir that also warrant a
	// reconcile. It carries the guide files: a README edit is not an operator
	// request and creates no trigger, but the controller mirrors it into the served
	// directory on the same fetch, and without this the corrected sentence waits out
	// an idle poll interval before anyone sees it.
	//
	// Exact paths, not prefixes. Everything under the repository root that is not a
	// trigger is content the controller reads at boot and nothing else — mods, the
	// world, config.yaml — and acting on a mod push would make every content commit
	// a reconcile for no gain.
	WatchPaths []string

	// IgnoreAuthors drops deliveries pushed by these git identities (matched
	// against pusher name and head-commit author, case-insensitively).
	//
	// It is empty by default, and that is deliberate rather than an omission. In
	// this deployment git.user_name is the operator's own account: the controller
	// commits as the same identity that pushes the triggers. An author filter
	// would therefore drop exactly the deliveries we exist to receive. Our own
	// echoes are excluded by SHA instead — gitbus.WasOurs in this process, and
	// Controller.ProcessedSHAs across a restart — which identifies the commit
	// rather than guessing at the person. Set this only if you give the
	// controller a distinct bot identity.
	IgnoreAuthors []string

	// MaxBody overrides MaxBodyDefault.
	MaxBody int64

	// OnPush is called for accepted deliveries, from the HTTP goroutine. It must
	// not block: the caller sends on a buffered channel and returns.
	OnPush func(Push)

	Logf func(format string, args ...any)
}

// Handler is the HTTP endpoint. Use New to build one.
type Handler struct {
	opts   Options
	ignore map[string]bool
	// watch is WatchPaths as a set. A path is matched exactly as GitHub spells it,
	// which is repository-root-relative with forward slashes on every platform.
	watch map[string]bool
}

// New validates the options and returns the handler.
func New(o Options) (*Handler, error) {
	switch {
	case o.Secret == "":
		return nil, errors.New("webhook: a secret is required")
	case o.Branch == "":
		return nil, errors.New("webhook: Branch is required")
	case o.TriggersDir == "":
		return nil, errors.New("webhook: TriggersDir is required")
	}
	if o.MaxBody <= 0 {
		o.MaxBody = MaxBodyDefault
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	h := &Handler{opts: o, ignore: map[string]bool{}, watch: map[string]bool{}}
	for _, a := range o.IgnoreAuthors {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
			h.ignore[a] = true
		}
	}
	for _, p := range o.WatchPaths {
		if p = strings.TrimSpace(p); p != "" {
			h.watch[p] = true
		}
	}
	return h, nil
}

// Path is where the handler expects to be mounted.
const Path = "/webhook"

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "webhook: POST only", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, h.opts.MaxBody+1))
	if err != nil {
		http.Error(w, "webhook: read body", http.StatusBadRequest)
		return
	}
	if int64(len(body)) > h.opts.MaxBody {
		http.Error(w, "webhook: payload too large", http.StatusRequestEntityTooLarge)
		return
	}

	if err := Verify(h.opts.Secret, r.Header.Get("X-Hub-Signature-256"), body); err != nil {
		// Deliberately terse to the caller and loud in the log: a signature
		// failure is either a misconfigured secret or someone probing.
		h.opts.Logf("webhook: rejected delivery %s: %v", r.Header.Get("X-GitHub-Delivery"), err)
		http.Error(w, "webhook: bad signature", http.StatusUnauthorized)
		return
	}

	switch event := r.Header.Get("X-GitHub-Event"); event {
	case "ping":
		// GitHub sends this when the hook is created. Answering it is how the
		// repository settings page shows a green tick.
		writeText(w, http.StatusOK, "pong")
		return
	case "push":
	case "":
		// Not from GitHub, but correctly signed, so it is one of ours — the
		// `pzctl` smoke test posts a bare push payload with no event header.
	default:
		writeText(w, http.StatusAccepted, "ignored: event "+event)
		return
	}

	push, err := ParsePush(body)
	if err != nil {
		// Signed but unparseable. Reconcile anyway: the poll loop would have got
		// there eventually, and refusing would turn a payload shape we failed to
		// anticipate into a silently missed trigger.
		h.opts.Logf("webhook: parse push: %v — reconciling anyway", err)
		h.dispatch(Push{Ref: "refs/heads/" + h.opts.Branch})
		writeText(w, http.StatusOK, "acting: payload unparseable, reconciling anyway")
		return
	}

	d := h.Decide(push)
	if !d.Act {
		writeText(w, http.StatusOK, "ignored: "+d.Reason)
		return
	}
	h.dispatch(push)
	writeText(w, http.StatusOK, "acting: "+d.Reason)
}

func (h *Handler) dispatch(p Push) {
	if h.opts.OnPush != nil {
		h.opts.OnPush(p)
	}
}

// Decide applies the filter chain. It is exported and pure so the rules can be
// tested without an HTTP server or a signature.
func (h *Handler) Decide(p Push) Decision {
	want := "refs/heads/" + h.opts.Branch
	if p.Ref != want && p.Ref != h.opts.Branch {
		return Decision{Reason: fmt.Sprintf("ref %q is not the operator branch %q", p.Ref, want)}
	}
	if p.Deleted {
		return Decision{Reason: "branch deletion carries no trigger"}
	}
	for _, who := range []string{p.Pusher, p.Author} {
		if who != "" && h.ignore[strings.ToLower(who)] {
			return Decision{Reason: fmt.Sprintf("pushed by ignored identity %q", who)}
		}
	}
	if len(p.Paths) == 0 {
		// No file list: either a push large enough for GitHub to truncate the
		// commit array, or a shape we did not anticipate. Reading the triggers
		// directory is cheap and idempotent, so act.
		return Decision{Act: true, Reason: "no file list in the delivery"}
	}
	prefix := strings.TrimSuffix(h.opts.TriggersDir, "/") + "/"
	for _, p := range p.Paths {
		if strings.HasPrefix(p, prefix) {
			return Decision{Act: true, Reason: "changed " + p}
		}
		if h.watch[p] {
			return Decision{Act: true, Reason: "changed watched path " + p}
		}
	}
	return Decision{Reason: "no paths under " + prefix + " changed"}
}

// Verify checks a GitHub X-Hub-Signature-256 header against the body.
//
// The comparison is constant-time, and a malformed header is a failure rather
// than something to be lenient about: leniency here is the whole vulnerability.
func Verify(secret, header string, body []byte) error {
	if header == "" {
		return errors.New("no X-Hub-Signature-256 header")
	}
	algo, hexsum, ok := strings.Cut(header, "=")
	if !ok || !strings.EqualFold(algo, "sha256") {
		return fmt.Errorf("signature %q is not sha256=…", header)
	}
	sum, err := hex.DecodeString(strings.TrimSpace(hexsum))
	if err != nil {
		return errors.New("signature is not hex")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if !hmac.Equal(sum, mac.Sum(nil)) {
		return errors.New("signature does not match the body")
	}
	return nil
}

// Sign produces the header value for a body. Used by the tests and by
// `pzctl controller --self-test`, so the receiver is exercised by the same code
// path GitHub uses rather than by a bypass.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// pushPayload is the subset of GitHub's push event we read. Unknown fields are
// ignored on purpose — the opposite of the config loader's rule, because this
// payload belongs to GitHub and gains fields without asking us.
type pushPayload struct {
	Ref     string `json:"ref"`
	Before  string `json:"before"`
	After   string `json:"after"`
	Created bool   `json:"created"`
	Deleted bool   `json:"deleted"`
	Pusher  struct {
		Name string `json:"name"`
	} `json:"pusher"`
	HeadCommit *commit  `json:"head_commit"`
	Commits    []commit `json:"commits"`
}

type commit struct {
	Author struct {
		Name     string `json:"name"`
		Username string `json:"username"`
	} `json:"author"`
	Added    []string `json:"added"`
	Modified []string `json:"modified"`
	Removed  []string `json:"removed"`
}

// ParsePush reduces a raw payload to a Push.
func ParsePush(body []byte) (Push, error) {
	var p pushPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return Push{}, err
	}
	if p.Ref == "" {
		return Push{}, errors.New("payload has no ref")
	}
	out := Push{
		Ref: p.Ref, Before: p.Before, After: p.After,
		Created: p.Created, Deleted: p.Deleted,
		Pusher: p.Pusher.Name,
	}
	if p.HeadCommit != nil {
		out.Author = p.HeadCommit.Author.Username
		if out.Author == "" {
			out.Author = p.HeadCommit.Author.Name
		}
	}
	seen := map[string]bool{}
	for _, c := range p.Commits {
		for _, group := range [][]string{c.Added, c.Modified, c.Removed} {
			for _, path := range group {
				if path != "" && !seen[path] {
					seen[path] = true
					out.Paths = append(out.Paths, path)
				}
			}
		}
	}
	return out, nil
}

func writeText(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprintln(w, msg)
}
