package gitbus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// File names on the state branches. They are constants rather than config
// because nothing outside this package needs to know them: the buses below are
// the only readers and writers.
const (
	FileController = "controller.json"
	FileBackups    = "backups.json"
	FileAgent      = "agent.json"
	// FileConfig is on the operator branch. Both sides read it; neither writes it.
	FileConfig = "config.yaml"
)

// Branches names the refs and directories the buses use. It mirrors the git
// section of the config, passed in explicitly so this package stays a leaf with
// no dependency on config loading.
type Branches struct {
	// Main is the operator-owned branch: config.yaml and the triggers directory.
	Main string
	// Controller and Agent are the state branches. When either equals Main the
	// layout is `single`, and state is written as a fast-forward child commit
	// alongside config instead of as a force-pushed orphan.
	Controller string
	Agent      string
	// TriggersDir is the directory on Main whose files are operator requests.
	TriggersDir string
}

func (b Branches) validate() error {
	switch {
	case b.Main == "":
		return errors.New("gitbus: Branches.Main is required")
	case b.Controller == "":
		return errors.New("gitbus: Branches.Controller is required")
	case b.Agent == "":
		return errors.New("gitbus: Branches.Agent is required")
	case b.TriggersDir == "":
		return errors.New("gitbus: Branches.TriggersDir is required")
	case b.Controller == b.Agent && b.Controller != b.Main:
		// Two writers on one dedicated branch is the one combination with no
		// coherent meaning: each force-push would erase the other's document.
		return errors.New("gitbus: Branches.Controller and Branches.Agent must differ unless both equal Main")
	}
	return nil
}

// dedicated reports whether branch is ours alone, and so may be replaced whole.
func (b Branches) dedicated(branch string) bool { return branch != b.Main }

// Trigger is one file the operator put in the triggers directory. Its content is
// opaque here; the FSM interprets it.
type Trigger struct {
	Name string
	Body []byte
}

// Path is the trigger's path on the operator branch.
func (t Trigger) Path(b Branches) string { return b.TriggersDir + "/" + t.Name }

// --- controller side ---

// ControllerBus is the controller's view of the repository.
//
// It can read everything, write the controller state branch, and consume triggers
// on the operator branch. It has no method that writes the agent's branch — and
// that absence, not a comment or a code review, is what enforces single-writer
// ownership. In v1 both the controller's state.sh and the server's entrypoint.sh
// wrote server_info.json, each clobbering fields the other owned, which is the
// direct cause of the status flapping.
type ControllerBus struct {
	repo *Repo
	br   Branches
	loc  *time.Location
}

// NewControllerBus wraps a Repo for controller use.
func NewControllerBus(repo *Repo, br Branches) (*ControllerBus, error) {
	if err := br.validate(); err != nil {
		return nil, err
	}
	return &ControllerBus{repo: repo, br: br, loc: repo.opts.Location}, nil
}

// Branches returns the ref layout in use.
func (b *ControllerBus) Branches() Branches { return b.br }

// Fetch updates the mirror.
func (b *ControllerBus) Fetch(ctx context.Context) error { return b.repo.Fetch(ctx) }

// ShouldAct reports whether a webhook push delivery deserves a reconcile. It is
// the loop-breaker for invariant I6, and it rejects on two independent grounds:
//
//   - the ref is not the operator branch, so it carries no intent. State branches
//     generate a delivery on every publish; in v1 state and intent shared one
//     branch, so every status write looked exactly like an operator request.
//   - the head SHA is one we pushed ourselves. Our own echo.
//
// This is the in-memory half. It forgets across a restart, which is why the FSM
// also checks Controller.WasProcessed before acting and calls MarkProcessed after
// — that ring is in the published document and therefore survives.
func (b *ControllerBus) ShouldAct(ref, sha string) bool {
	if ref != "refs/heads/"+b.br.Main && ref != b.br.Main {
		return false
	}
	return !b.repo.WasOurs(sha)
}

// Pushed returns the SHAs this process created, oldest first, so the FSM can fold
// them into the document's dedup ring before publishing.
func (b *ControllerBus) Pushed() []string { return b.repo.Pushed() }

// Head returns the commit SHA of the operator branch as of the last Fetch.
func (b *ControllerBus) Head() (string, error) { return b.repo.Head(b.br.Main) }

// ReadMain returns one path from the operator branch as of the last Fetch, with a
// missing file reported as ErrNotFound.
//
// It is how the controller mirrors the guide out of the repository without this
// package growing an opinion about what a guide is. Reading a blob is also the
// only way to do it: there is deliberately no working tree here, because v1 kept
// one and ran `git reset --hard` on it to sync, which deleted files belonging to a
// backup that was still being written.
func (b *ControllerBus) ReadMain(path string) ([]byte, error) {
	return b.repo.ReadFile(b.br.Main, path)
}

// ReadConfigBytes returns config.yaml from the operator branch. It is bytes
// rather than a *config.Config so this package stays free of the config import;
// the caller decodes and validates.
func (b *ControllerBus) ReadConfigBytes() ([]byte, error) {
	return b.repo.ReadFile(b.br.Main, FileConfig)
}

// Exists reports whether a path is present on a branch.
//
// It exists for level signals — backups.pause_file being the one — which are
// plain files rather than triggers precisely because they are not consumed: a
// trigger is an edge and is deleted when acted on, whereas "do not back up for a
// while" has to stay true until the operator deletes it.
func (b *ControllerBus) Exists(branch, path string) (bool, error) {
	_, err := b.repo.ReadFile(branch, path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrNotFound):
		return false, nil
	default:
		return false, err
	}
}

// Triggers lists the pending operator requests, sorted by name.
func (b *ControllerBus) Triggers() ([]Trigger, error) {
	names, err := b.repo.ListDir(b.br.Main, b.br.TriggersDir)
	if err != nil {
		return nil, err
	}
	out := make([]Trigger, 0, len(names))
	for _, name := range names {
		body, err := b.repo.ReadFile(b.br.Main, b.br.TriggersDir+"/"+name)
		if err != nil {
			return nil, err
		}
		out = append(out, Trigger{Name: name, Body: body})
	}
	return out, nil
}

// ConsumeTriggers deletes the named trigger files in one commit, so a burst of
// requests costs one push. It returns the commit SHA, or "" if there was nothing
// to delete.
//
// Deleting is what makes a trigger a one-shot. v1 used sentinel file contents
// ("requested" / "pending") that had to be rewritten to be cleared, so a crash
// between acting and clearing re-ran the action on the next tick — one of the two
// ways a single halt became several.
func (b *ControllerBus) ConsumeTriggers(ctx context.Context, names []string) (string, error) {
	if len(names) == 0 {
		return "", nil
	}
	paths := make([]string, 0, len(names))
	for _, n := range names {
		paths = append(paths, b.br.TriggersDir+"/"+n)
	}
	msg := fmt.Sprintf("consume trigger(s): %s", strings.Join(names, ", "))
	return b.repo.RemovePaths(ctx, b.br.Main, paths, msg)
}

// ReadAgent returns the agent's document. A branch that does not exist yet yields
// a fresh Agent and no error: "the agent has never reported" is a normal state,
// not a failure.
func (b *ControllerBus) ReadAgent() (*state.Agent, *state.Repairs, error) {
	doc := state.NewAgent(b.loc)
	r, err := readDoc(b.repo, b.br.Agent, FileAgent, doc)
	return doc, r, err
}

// ReadOwn re-reads what the controller last published. Used at startup to recover
// the lease across a restart — the one piece of state that costs money if it is
// lost, which is why a fatal repair here must be treated as "reconcile against
// Akash", never as "there is no lease".
func (b *ControllerBus) ReadOwn() (*state.Controller, *state.Backups, *state.Repairs, error) {
	doc := state.NewController(b.loc)
	idx := state.NewBackups()
	r1, err := readDoc(b.repo, b.br.Controller, FileController, doc)
	if err != nil {
		return nil, nil, nil, err
	}
	r2, err := readDoc(b.repo, b.br.Controller, FileBackups, idx)
	if err != nil {
		return nil, nil, nil, err
	}
	return doc, idx, mergeRepairs(r1, r2), nil
}

// Publish writes the controller document and the backup index together, so a
// reader can never see an index that mentions an archive the document's
// restore_target contradicts. idx may be nil to leave the index untouched — but
// only in the single layout, where the two files live independently; on a
// dedicated branch the write replaces the whole tree, so the index must be
// supplied.
func (b *ControllerBus) Publish(ctx context.Context, doc *state.Controller, idx *state.Backups, reason string) (string, error) {
	doc.UpdatedAt = state.Now(b.loc)
	files := map[string][]byte{}
	if err := encodeInto(files, FileController, doc); err != nil {
		return "", err
	}
	switch {
	case idx != nil:
		idx.UpdatedAt = state.Now(b.loc)
		if err := encodeInto(files, FileBackups, idx); err != nil {
			return "", err
		}
	case b.br.dedicated(b.br.Controller):
		return "", errors.New("gitbus: Publish needs the backup index (a dedicated state branch is replaced whole)")
	}
	return b.write(ctx, b.br.Controller, files, "controller: "+reason)
}

func (b *ControllerBus) write(ctx context.Context, branch string, files map[string][]byte, msg string) (string, error) {
	if b.br.dedicated(branch) {
		return b.repo.PutOrphan(ctx, branch, files, msg)
	}
	return b.repo.Commit(ctx, branch, Mutation{Put: files}, msg)
}

// --- agent side ---

// AgentBus is the agent's view. It reads the controller's branch and writes its
// own, and has no method that can write the controller's — so the agent cannot
// invent a lease, change the intent, or edit the backup index even if its own
// logic is wrong.
type AgentBus struct {
	repo *Repo
	br   Branches
	loc  *time.Location
}

// NewAgentBus wraps a Repo for agent use.
func NewAgentBus(repo *Repo, br Branches) (*AgentBus, error) {
	if err := br.validate(); err != nil {
		return nil, err
	}
	return &AgentBus{repo: repo, br: br, loc: repo.opts.Location}, nil
}

// Fetch updates the mirror.
func (b *AgentBus) Fetch(ctx context.Context) error { return b.repo.Fetch(ctx) }

// ReadConfigBytes returns config.yaml from the operator branch.
func (b *AgentBus) ReadConfigBytes() ([]byte, error) {
	return b.repo.ReadFile(b.br.Main, FileConfig)
}

// ReadController returns the controller's document and backup index. This is the
// agent's only source of intent: it never decides for itself whether to run.
func (b *AgentBus) ReadController() (*state.Controller, *state.Backups, *state.Repairs, error) {
	doc := state.NewController(b.loc)
	idx := state.NewBackups()
	r1, err := readDoc(b.repo, b.br.Controller, FileController, doc)
	if err != nil {
		return nil, nil, nil, err
	}
	r2, err := readDoc(b.repo, b.br.Controller, FileBackups, idx)
	if err != nil {
		return nil, nil, nil, err
	}
	return doc, idx, mergeRepairs(r1, r2), nil
}

// ReadOwn re-reads what the agent last published, so a restarted agent resumes
// its restart counter instead of getting a fresh budget. Without this, the crash
// limit is unenforceable: the container restart that resets the count is exactly
// the event the count is supposed to bound.
func (b *AgentBus) ReadOwn() (*state.Agent, *state.Repairs, error) {
	doc := state.NewAgent(b.loc)
	r, err := readDoc(b.repo, b.br.Agent, FileAgent, doc)
	return doc, r, err
}

// Publish writes the agent document.
func (b *AgentBus) Publish(ctx context.Context, doc *state.Agent, reason string) (string, error) {
	doc.Touch(state.Now(b.loc))
	files := map[string][]byte{}
	if err := encodeInto(files, FileAgent, doc); err != nil {
		return "", err
	}
	msg := "agent: " + reason
	if b.br.dedicated(b.br.Agent) {
		return b.repo.PutOrphan(ctx, b.br.Agent, files, msg)
	}
	return b.repo.Commit(ctx, b.br.Agent, Mutation{Put: files}, msg)
}

// --- shared helpers ---

// readDoc reads one document with repair-on-read. A missing branch or file is
// reported as no repairs and no error, leaving dst at the caller's defaults —
// which is the same treatment state.ReadFileInto gives a missing file on disk, so
// the two transports are interchangeable.
func readDoc(repo *Repo, branch, path string, dst any) (*state.Repairs, error) {
	data, err := repo.ReadFile(branch, path)
	if errors.Is(err, ErrNotFound) {
		r := &state.Repairs{}
		if n, ok := dst.(state.Normalizer); ok {
			n.Normalize(r)
		}
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	return state.Unmarshal(data, dst), nil
}

func encodeInto(files map[string][]byte, path string, doc any) error {
	data, err := state.Marshal(doc)
	if err != nil {
		return err
	}
	files[path] = data
	return nil
}

func mergeRepairs(rs ...*state.Repairs) *state.Repairs {
	out := &state.Repairs{}
	for _, r := range rs {
		if r != nil {
			out.Items = append(out.Items, r.Items...)
		}
	}
	return out
}
