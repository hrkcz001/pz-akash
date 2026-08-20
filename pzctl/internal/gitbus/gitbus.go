// Package gitbus is the transport for every piece of state the controller and the
// agent exchange. The repository is the message bus; this package is the only
// code allowed to talk to it.
//
// Three properties are structural rather than conventional, and each one closes a
// v1 failure mode:
//
//  1. There is no working tree. The mirror is bare and commits are built directly
//     from objects. v1 kept a checkout and ran `git reset --hard` on it to sync,
//     which deleted files belonging to a backup that was still being written.
//
//  2. State branches are replaced, never appended to. Each publish is a single
//     parentless commit force-pushed over the branch, so the branch is exactly
//     one document version deep, there is no merge to conflict, and the
//     repository does not grow without bound. v1 committed onto the shared
//     branch and hit "fetch first" rejections whenever two writers raced.
//
//  3. Ownership is enforced by the type system. ControllerBus has no method that
//     writes the agent's branch and AgentBus has none that writes the
//     controller's. In v1 both the controller's state.sh and the server's
//     entrypoint.sh wrote server_info.json, and each one overwrote fields the
//     other owned — the direct cause of the status flapping.
//
// Everything here is single-goroutine per Repo. Concurrent use needs an external
// lock; the FSM owns exactly one Repo and drives it from one loop.
package gitbus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	cryptossh "golang.org/x/crypto/ssh"
)

// ErrNotFound reports a missing branch, directory or file. It wraps
// fs.ErrNotExist so callers can use errors.Is, and so a Repo-backed
// state.Fetcher behaves the same as a directory-backed one.
var ErrNotFound = fmt.Errorf("gitbus: not found: %w", fs.ErrNotExist)

// ErrNotFastForward reports that the operator branch moved under us. It is not a
// failure: it means a human pushed while we were consuming a trigger, and the
// right response is to re-read and decide again.
var ErrNotFastForward = errors.New("gitbus: remote branch moved (not a fast-forward)")

// ErrTimeout reports a remote operation that overran NetTimeout and was
// abandoned. The caller's goroutine is free again; the operation may still be
// running.
var ErrTimeout = errors.New("gitbus: remote operation timed out")

// ErrBusy reports that an abandoned remote operation has not finished, so this
// one did not start. Retrying on the next tick is the correct response.
var ErrBusy = errors.New("gitbus: a previous remote operation is still running")

// DefaultNetTimeout bounds a fetch or push when Options.NetTimeout is unset.
const DefaultNetTimeout = 45 * time.Second

// Options configures a Repo. Every value comes from config except DeployKeyPEM,
// which comes from the environment via internal/secrets.
type Options struct {
	RepoURL  string
	CacheDir string

	UserName  string
	UserEmail string

	// KnownHosts pins the remote's SSH host keys, in known_hosts format.
	KnownHosts string
	// AllowUnverifiedHost skips host-key checking. Only for a local test remote;
	// config validation refuses it for an SSH URL.
	AllowUnverifiedHost bool

	// DeployKeyPEM is the private key, already base64-decoded. Empty means no
	// SSH auth, which is correct for a local path remote.
	DeployKeyPEM []byte

	// Location stamps commit times, so `git log` on the state branches reads in
	// the operator's timezone rather than the container's.
	Location *time.Location

	// NetTimeout bounds one fetch or push. Zero means DefaultNetTimeout.
	NetTimeout time.Duration

	// Logf is optional.
	Logf func(format string, args ...any)
}

// Repo is a bare mirror of the state repository.
type Repo struct {
	opts Options
	repo *git.Repository
	auth transport.AuthMethod

	// gate admits one remote operation at a time. It is a TryLock gate rather
	// than a plain mutex because an operation that overran its timeout has been
	// abandoned, and the right answer for the next caller is ErrBusy now, not a
	// queue behind something that may never finish.
	gate sync.Mutex

	mu sync.Mutex
	// pushed is every commit SHA this process created. It is the fast half of the
	// self-push filter: a webhook naming one of these is our own echo, not an
	// operator action. The durable half is Controller.ProcessedSHAs, which
	// survives a restart.
	pushed   map[string]bool
	pushedIn []string
}

// pushedCap bounds the in-memory self-push set.
const pushedCap = 256

// Open prepares the mirror at opts.CacheDir, creating it if necessary, and points
// it at opts.RepoURL. It performs no network I/O; call Fetch for that.
func Open(opts Options) (*Repo, error) {
	if opts.RepoURL == "" {
		return nil, errors.New("gitbus: RepoURL is required")
	}
	if opts.CacheDir == "" {
		return nil, errors.New("gitbus: CacheDir is required")
	}
	if opts.Location == nil {
		opts.Location = time.UTC
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}

	repo, err := git.PlainInit(opts.CacheDir, true)
	if errors.Is(err, git.ErrRepositoryAlreadyExists) {
		repo, err = git.PlainOpen(opts.CacheDir)
	}
	if err != nil {
		return nil, fmt.Errorf("gitbus: open mirror at %s: %w", opts.CacheDir, err)
	}

	if err := setRemote(repo, opts.RepoURL); err != nil {
		return nil, err
	}

	r := &Repo{opts: opts, repo: repo, pushed: map[string]bool{}}
	if r.auth, err = buildAuth(opts); err != nil {
		return nil, err
	}
	return r, nil
}

// setRemote makes origin point at url, recreating it if the URL changed. A
// mirror left over from a previous config would otherwise keep talking to the old
// remote, which is a very quiet way to lose state.
func setRemote(repo *git.Repository, url string) error {
	remote, err := repo.Remote("origin")
	switch {
	case errors.Is(err, git.ErrRemoteNotFound):
	case err != nil:
		return fmt.Errorf("gitbus: read remote: %w", err)
	default:
		if len(remote.Config().URLs) == 1 && remote.Config().URLs[0] == url {
			return nil
		}
		if err := repo.DeleteRemote("origin"); err != nil {
			return fmt.Errorf("gitbus: replace remote: %w", err)
		}
	}
	if _, err := repo.CreateRemote(&gitconfig.RemoteConfig{
		Name: "origin",
		URLs: []string{url},
	}); err != nil {
		return fmt.Errorf("gitbus: create remote: %w", err)
	}
	return nil
}

func buildAuth(opts Options) (transport.AuthMethod, error) {
	if len(opts.DeployKeyPEM) == 0 {
		// A local path or file:// remote needs no credential. An SSH URL without
		// one will fail at Fetch with a clear transport error, which is a better
		// message than anything we could invent here.
		return nil, nil
	}
	keys, err := gitssh.NewPublicKeys("git", opts.DeployKeyPEM, "")
	if err != nil {
		return nil, fmt.Errorf("gitbus: parse deploy key: %w", err)
	}
	if opts.AllowUnverifiedHost {
		keys.HostKeyCallback = cryptossh.InsecureIgnoreHostKey()
		return keys, nil
	}
	cb, err := hostKeyCallback(opts.KnownHosts)
	if err != nil {
		return nil, err
	}
	keys.HostKeyCallback = cb
	return keys, nil
}

// hostKeyCallback builds a verifier from inline known_hosts content, so host-key
// pinning is a config value and not a file that has to be planted in the image.
//
// Only plain (unhashed) host entries are supported, which is what
// `curl https://api.github.com/meta` and `ssh-keyscan` produce by default.
func hostKeyCallback(knownHosts string) (cryptossh.HostKeyCallback, error) {
	if strings.TrimSpace(knownHosts) == "" {
		return nil, errors.New("gitbus: no known_hosts configured and AllowUnverifiedHost is false")
	}
	allowed := map[string][]cryptossh.PublicKey{}
	rest := []byte(knownHosts)
	for {
		_, hosts, key, _, remainder, err := cryptossh.ParseKnownHosts(rest)
		if errors.Is(err, fs.ErrNotExist) || err != nil {
			if len(allowed) == 0 {
				return nil, fmt.Errorf("gitbus: parse known_hosts: %w", err)
			}
			break
		}
		for _, h := range hosts {
			if strings.HasPrefix(h, "|") {
				return nil, errors.New("gitbus: hashed known_hosts entries are not supported; use plain hostnames")
			}
			allowed[stripPort(h)] = append(allowed[stripPort(h)], key)
		}
		if len(remainder) == 0 {
			break
		}
		rest = remainder
	}
	if len(allowed) == 0 {
		return nil, errors.New("gitbus: known_hosts contained no usable entries")
	}

	return func(hostname string, _ net.Addr, key cryptossh.PublicKey) error {
		host := stripPort(hostname)
		want := allowed[host]
		if len(want) == 0 {
			return fmt.Errorf("gitbus: no pinned host key for %q; add it to git.known_hosts", host)
		}
		got := key.Marshal()
		for _, k := range want {
			if bytes.Equal(k.Marshal(), got) {
				return nil
			}
		}
		// Refusing here is the whole point: the next thing we would do is present
		// a repository write key.
		return fmt.Errorf("gitbus: host key for %q does not match any pinned key (%s offered)",
			host, key.Type())
	}, nil
}

// stripPort reduces a known_hosts pattern or a callback hostname to a bare host.
//
// SplitHostPort has to be tried before the brackets are touched, because it is
// what distinguishes "[::1]:22" (host ::1, port 22) from "::1" (a host that
// merely contains colons). Stripping first turns "[github.com]:443" into
// "github.com]:443", whose port splits off leaving a trailing bracket — and a
// host that never matches anything pinned. That form is not hypothetical:
// GitHub documents ssh://git@github.com:443 as the way through a firewall that
// blocks port 22.
func stripPort(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
}

// netOp runs one remote operation under a deadline the caller cannot outlive.
//
// This exists because context cancellation is not enough. go-git's transports do
// not all honour it once they are blocked in a read: the local transport blocks
// reading the ref advertisement from a git-upload-pack subprocess, and an SSH
// session to a host that stops answering blocks in a socket read with no
// deadline. Either one pins the calling goroutine indefinitely.
//
// For the agent that is fatal in a specific way. The goroutine that fetches is
// the same one that answers a halt, stops the game and saves the world when the
// lease closes — so a wedged fetch does not merely delay a reconcile, it takes
// away the agent's ability to shut down cleanly. An operation that overruns is
// therefore abandoned, not waited for.
//
// Abandoning is only safe because nothing may run beside it: go-git is not safe
// for concurrent use of one Repository, so the abandoned operation keeps the gate
// until it finishes and every caller in the meantime gets ErrBusy. Callers must
// treat a failed Fetch as "do not read" — an abandoned fetch is still writing to
// the object store — which is what both the agent's reconcile and the FSM do.
func (r *Repo) netOp(ctx context.Context, what string, fn func(context.Context) error) error {
	if !r.gate.TryLock() {
		return fmt.Errorf("%w (%s)", ErrBusy, what)
	}
	timeout := r.opts.NetTimeout
	if timeout <= 0 {
		timeout = DefaultNetTimeout
	}
	opCtx, cancel := context.WithTimeout(ctx, timeout)
	done := make(chan error, 1)
	go func() {
		// Three orderings matter here, and each of them was a bug.
		//
		// The gate is released before the caller is woken. A defer around the send
		// runs after it, leaving a window where netOp has returned while the gate is
		// still held — and both sides fetch and then immediately publish on every
		// reconcile, so that window hands out ErrBusy for an operation that has
		// finished: a reconcile skipped for no reason, and on the agent a phase
		// change the controller never sees.
		//
		// cancel comes after the send, because cancelling opCtx is what makes the
		// caller's opCtx.Done() fire. Cancel first and both select cases are ready at
		// once, so the caller reports a timeout for work that succeeded.
		//
		// The send is a closure's return value so that a panic in fn still releases
		// the gate.
		done <- func() error {
			defer r.gate.Unlock()
			return fn(opCtx)
		}()
		cancel()
	}()

	select {
	case err := <-done:
		return err
	case <-opCtx.Done():
		// A result already waiting outranks the deadline. The two are not mutually
		// exclusive: opCtx is cancelled once the operation finishes, so a caller
		// descheduled between the send and the cancel arrives here with the answer
		// sitting in the channel.
		select {
		case err := <-done:
			return err
		default:
		}
		if err := ctx.Err(); err != nil {
			// The caller is shutting down. Returning now is the whole point: the
			// container has a SIGTERM grace period, and it is needed for the save.
			return fmt.Errorf("gitbus: %s: %w", what, err)
		}
		r.opts.Logf("gitbus: %s exceeded %v and was abandoned; the next attempt will report busy until it finishes", what, timeout)
		return fmt.Errorf("%w: %s after %v", ErrTimeout, what, timeout)
	}
}

// Fetch updates every branch in the mirror from the remote.
//
// All heads are fetched with a single forced wildcard refspec rather than one
// refspec per branch, because a state branch does not exist until its writer
// first publishes, and naming a missing ref is a hard error.
func (r *Repo) Fetch(ctx context.Context) error {
	return r.netOp(ctx, "fetch "+r.opts.RepoURL, r.fetch)
}

func (r *Repo) fetch(ctx context.Context) error {
	err := r.repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName: "origin",
		RefSpecs:   []gitconfig.RefSpec{"+refs/heads/*:refs/heads/*"},
		Auth:       r.auth,
		Force:      true,
		Prune:      true,
		Tags:       git.NoTags,
	})
	switch {
	case err == nil, errors.Is(err, git.NoErrAlreadyUpToDate):
		return nil
	case errors.Is(err, transport.ErrEmptyRemoteRepository):
		// A repository with no refs yet. Nothing to read, and publishing will
		// create the branches.
		return nil
	default:
		return fmt.Errorf("gitbus: fetch %s: %w", r.opts.RepoURL, err)
	}
}

// Head returns the commit SHA at branch in the mirror, as of the last Fetch.
func (r *Repo) Head(branch string) (string, error) {
	h, err := r.resolve(branch)
	if err != nil {
		return "", err
	}
	return h.String(), nil
}

func (r *Repo) resolve(branch string) (plumbing.Hash, error) {
	ref, err := r.repo.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return plumbing.ZeroHash, fmt.Errorf("branch %q: %w", branch, ErrNotFound)
		}
		return plumbing.ZeroHash, fmt.Errorf("gitbus: resolve %s: %w", branch, err)
	}
	return ref.Hash(), nil
}

func (r *Repo) tree(branch string) (*object.Tree, error) {
	h, err := r.resolve(branch)
	if err != nil {
		return nil, err
	}
	commit, err := r.repo.CommitObject(h)
	if err != nil {
		return nil, fmt.Errorf("gitbus: read commit %s: %w", h, err)
	}
	t, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("gitbus: read tree of %s: %w", h, err)
	}
	return t, nil
}

// ReadFile reads one path from branch. A missing branch or path is ErrNotFound,
// which callers treat as "not written yet" rather than as a failure.
func (r *Repo) ReadFile(branch, path string) ([]byte, error) {
	t, err := r.tree(branch)
	if err != nil {
		return nil, err
	}
	f, err := t.File(path)
	if err != nil {
		if errors.Is(err, object.ErrFileNotFound) || errors.Is(err, object.ErrDirectoryNotFound) {
			return nil, fmt.Errorf("%s:%s: %w", branch, path, ErrNotFound)
		}
		return nil, fmt.Errorf("gitbus: read %s:%s: %w", branch, path, err)
	}
	reader, err := f.Reader()
	if err != nil {
		return nil, fmt.Errorf("gitbus: open %s:%s: %w", branch, path, err)
	}
	defer reader.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(reader); err != nil {
		return nil, fmt.Errorf("gitbus: read %s:%s: %w", branch, path, err)
	}
	return buf.Bytes(), nil
}

// Fetcher adapts a branch to state.Fetcher, so the same repair-on-read code path
// serves a git ref and a directory on disk.
func (r *Repo) Fetcher(branch string) func(path string) ([]byte, error) {
	return func(path string) ([]byte, error) { return r.ReadFile(branch, path) }
}

// ListDir returns the names of the files directly under dir on branch, sorted. A
// missing branch or directory yields an empty list, not an error: "no triggers"
// and "no triggers directory" mean the same thing to the caller.
func (r *Repo) ListDir(branch, dir string) ([]string, error) {
	t, err := r.tree(branch)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	sub := t
	if dir != "" && dir != "." {
		sub, err = t.Tree(dir)
		if err != nil {
			if errors.Is(err, object.ErrDirectoryNotFound) {
				return nil, nil
			}
			return nil, fmt.Errorf("gitbus: read %s:%s/: %w", branch, dir, err)
		}
	}
	var names []string
	for _, e := range sub.Entries {
		if e.Mode.IsFile() {
			names = append(names, e.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// PutOrphan replaces branch with a single parentless commit whose tree is exactly
// files, and force-pushes it. It returns the new commit SHA.
//
// Replacing rather than appending is what makes a state write unconditional: there
// is no parent to be out of date, so there is no rejection to retry and no
// conflict to resolve. The cost is that the branch has no history, which is
// correct — the state documents are a snapshot, and their history lives in the
// operator branch's trigger commits and in the log.
func (r *Repo) PutOrphan(ctx context.Context, branch string, files map[string][]byte, message string) (string, error) {
	if len(files) == 0 {
		return "", errors.New("gitbus: refusing to publish an empty tree")
	}
	entries := make(map[string]plumbing.Hash, len(files))
	for path, data := range files {
		h, err := r.writeBlob(data)
		if err != nil {
			return "", err
		}
		entries[path] = h
	}
	treeHash, err := r.writeTree(entries)
	if err != nil {
		return "", err
	}
	commit, err := r.writeCommit(treeHash, nil, message)
	if err != nil {
		return "", err
	}
	if err := r.setBranch(branch, commit); err != nil {
		return "", err
	}
	if err := r.push(ctx, branch, true); err != nil {
		return "", err
	}
	r.recordPush(commit.String())
	return commit.String(), nil
}

// Mutation is a change to apply on top of a branch's current contents.
type Mutation struct {
	// Put adds or replaces files, keyed by slash-separated path.
	Put map[string][]byte
	// Delete removes files. Paths already absent are ignored.
	Delete []string
}

// Commit applies mut to branch as an ordinary child commit and pushes it without
// force, returning the new commit SHA — or "" if mut would not have changed
// anything, in which case no commit is made and nothing is pushed.
//
// This is the write path for any branch we do not own outright: consuming a
// trigger on the operator branch, and writing state in the `single` layout where
// state shares a branch with config. The push is deliberately not forced. That
// branch belongs to a human, and discarding one of their commits to make our own
// bookkeeping tidy is the wrong trade — v1 force-pushed a working tree it had
// `git reset --hard`ed, which is exactly how an operator edit disappears. A
// rejection surfaces as ErrNotFastForward and the caller re-fetches and decides
// again.
func (r *Repo) Commit(ctx context.Context, branch string, mut Mutation, message string) (string, error) {
	var parents []plumbing.Hash
	var baseTree plumbing.Hash
	entries := map[string]plumbing.Hash{}

	switch parent, err := r.resolve(branch); {
	case err == nil:
		parents = append(parents, parent)
		t, err := r.tree(branch)
		if err != nil {
			return "", err
		}
		baseTree = t.Hash
		doomed := make(map[string]bool, len(mut.Delete))
		for _, p := range mut.Delete {
			doomed[p] = true
		}
		iter := t.Files()
		for {
			f, err := iter.Next()
			if err != nil {
				break
			}
			if !doomed[f.Name] {
				entries[f.Name] = f.Hash
			}
		}
		iter.Close()
	case errors.Is(err, ErrNotFound):
		// The branch does not exist yet. Creating a ref is not a
		// non-fast-forward, so an unforced push still works.
	default:
		return "", err
	}

	for path, data := range mut.Put {
		h, err := r.writeBlob(data)
		if err != nil {
			return "", err
		}
		entries[path] = h
	}
	if len(entries) == 0 && !baseTree.IsZero() {
		// An empty tree is legal in git, but a branch that suddenly holds nothing
		// is far more likely to be our bug than the operator's intent.
		return "", errors.New("gitbus: refusing to empty branch " + branch)
	}

	treeHash, err := r.writeTree(entries)
	if err != nil {
		return "", err
	}
	if treeHash == baseTree {
		// Nothing actually changed: every Put matched what was already there and
		// every Delete named a file that was already gone. An empty commit here
		// would be a push, which would be a webhook delivery, which is how v1
		// turned a quiet tick into an event loop.
		return "", nil
	}

	commit, err := r.writeCommit(treeHash, parents, message)
	if err != nil {
		return "", err
	}
	if err := r.setBranch(branch, commit); err != nil {
		return "", err
	}
	if err := r.push(ctx, branch, false); err != nil {
		// Put the local ref back, so a retry after a fresh Fetch is not building
		// on a commit the remote rejected.
		if len(parents) == 1 {
			_ = r.setBranch(branch, parents[0])
		} else {
			_ = r.repo.Storer.RemoveReference(plumbing.NewBranchReferenceName(branch))
		}
		return "", err
	}
	r.recordPush(commit.String())
	return commit.String(), nil
}

// RemovePaths is Commit with only deletions. Consuming a trigger is the only
// caller, and naming the operation is clearer at that call site than an inline
// Mutation literal.
func (r *Repo) RemovePaths(ctx context.Context, branch string, paths []string, message string) (string, error) {
	return r.Commit(ctx, branch, Mutation{Delete: paths}, message)
}

func (r *Repo) writeBlob(data []byte) (plumbing.Hash, error) {
	obj := r.repo.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("gitbus: blob writer: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		w.Close()
		return plumbing.ZeroHash, fmt.Errorf("gitbus: write blob: %w", err)
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("gitbus: close blob: %w", err)
	}
	h, err := r.repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("gitbus: store blob: %w", err)
	}
	return h, nil
}

// writeTree builds the tree objects for a set of slash-separated paths,
// bottom-up. Subtrees are created as needed.
func (r *Repo) writeTree(files map[string]plumbing.Hash) (plumbing.Hash, error) {
	// Group by first path segment: plain files at this level, and everything
	// deeper by subdirectory name.
	here := map[string]plumbing.Hash{}
	sub := map[string]map[string]plumbing.Hash{}
	for path, hash := range files {
		dir, rest, nested := strings.Cut(path, "/")
		if !nested {
			here[path] = hash
			continue
		}
		if sub[dir] == nil {
			sub[dir] = map[string]plumbing.Hash{}
		}
		sub[dir][rest] = hash
	}

	entries := make([]object.TreeEntry, 0, len(here)+len(sub))
	for name, hash := range here {
		entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: hash})
	}
	for name, children := range sub {
		h, err := r.writeTree(children)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: h})
	}
	// Git requires tree entries in its own canonical order. go-git does not sort
	// for us, and an unsorted tree is accepted by go-git but reported as corrupt
	// by git fsck and can confuse diffs.
	sort.Slice(entries, func(i, j int) bool {
		return baseNameCompare(entries[i], entries[j]) < 0
	})

	tree := &object.Tree{Entries: entries}
	obj := r.repo.Storer.NewEncodedObject()
	if err := tree.Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("gitbus: encode tree: %w", err)
	}
	h, err := r.repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("gitbus: store tree: %w", err)
	}
	return h, nil
}

// baseNameCompare reproduces git's base_name_compare: names sort byte-wise, and
// when one is a prefix of the other, a directory compares as if its name ended
// in '/'.
func baseNameCompare(a, b object.TreeEntry) int {
	n1, n2 := a.Name, b.Name
	l := len(n1)
	if len(n2) < l {
		l = len(n2)
	}
	if c := strings.Compare(n1[:l], n2[:l]); c != 0 {
		return c
	}
	c1, c2 := byte(0), byte(0)
	switch {
	case len(n1) > l:
		c1 = n1[l]
	case a.Mode == filemode.Dir:
		c1 = '/'
	}
	switch {
	case len(n2) > l:
		c2 = n2[l]
	case b.Mode == filemode.Dir:
		c2 = '/'
	}
	return int(c1) - int(c2)
}

func (r *Repo) writeCommit(tree plumbing.Hash, parents []plumbing.Hash, message string) (plumbing.Hash, error) {
	// Commit times are stamped in the configured location, so `git log` on the
	// state branches reads in the operator's timezone. v1 used the container's
	// clock, which had no zone configured at all.
	when := time.Now().In(r.opts.Location)
	sig := object.Signature{Name: r.opts.UserName, Email: r.opts.UserEmail, When: when}
	commit := &object.Commit{
		Author:       sig,
		Committer:    sig,
		Message:      strings.TrimRight(message, "\n") + "\n",
		TreeHash:     tree,
		ParentHashes: parents,
	}
	obj := r.repo.Storer.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("gitbus: encode commit: %w", err)
	}
	h, err := r.repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("gitbus: store commit: %w", err)
	}
	return h, nil
}

func (r *Repo) setBranch(branch string, commit plumbing.Hash) error {
	ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), commit)
	if err := r.repo.Storer.SetReference(ref); err != nil {
		return fmt.Errorf("gitbus: set %s: %w", branch, err)
	}
	return nil
}

func (r *Repo) push(ctx context.Context, branch string, force bool) error {
	return r.netOp(ctx, "push "+branch, func(ctx context.Context) error {
		return r.pushNow(ctx, branch, force)
	})
}

func (r *Repo) pushNow(ctx context.Context, branch string, force bool) error {
	spec := fmt.Sprintf("refs/heads/%s:refs/heads/%s", branch, branch)
	if force {
		spec = "+" + spec
	}
	err := r.repo.PushContext(ctx, &git.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []gitconfig.RefSpec{gitconfig.RefSpec(spec)},
		Auth:       r.auth,
		Force:      force,
	})
	switch {
	case err == nil, errors.Is(err, git.NoErrAlreadyUpToDate):
		return nil
	case errors.Is(err, git.ErrNonFastForwardUpdate),
		strings.Contains(err.Error(), "non-fast-forward"):
		return fmt.Errorf("%w: %s", ErrNotFastForward, branch)
	default:
		return fmt.Errorf("gitbus: push %s: %w", branch, err)
	}
}

// recordPush notes a SHA we created, for the self-push filter.
func (r *Repo) recordPush(sha string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pushed[sha] {
		return
	}
	r.pushed[sha] = true
	r.pushedIn = append(r.pushedIn, sha)
	if len(r.pushedIn) > pushedCap {
		drop := r.pushedIn[0]
		r.pushedIn = r.pushedIn[1:]
		delete(r.pushed, drop)
	}
}

// WasOurs reports whether sha is a commit this process pushed.
//
// This is half of the loop-breaker. v1 had no equivalent: every state write
// pushed to the branch the webhook watched, GitHub delivered the push back, the
// controller read it as a new event, and acted again — which is how one halt
// became several and one backup became duplicates. The other half is filtering by
// ref, since state branches are not the trigger branch at all.
func (r *Repo) WasOurs(sha string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pushed[sha]
}

// Pushed returns the SHAs this process created, oldest first, so the FSM can fold
// them into the durable dedup ring before it writes state.
func (r *Repo) Pushed() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.pushedIn...)
}
