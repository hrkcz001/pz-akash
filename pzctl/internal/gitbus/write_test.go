package gitbus

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestPutOrphanKeepsTheBranchOneCommitDeep(t *testing.T) {
	t.Parallel()
	remote := seedRemote(t, liveish())
	r := openMirror(t, remote)

	first, err := r.PutOrphan(t.Context(), "state/controller",
		map[string][]byte{"controller.json": []byte(`{"version":1}`)}, "first")
	if err != nil {
		t.Fatalf("PutOrphan: %v", err)
	}
	second, err := r.PutOrphan(t.Context(), "state/controller",
		map[string][]byte{"controller.json": []byte(`{"version":1,"status":"online"}`)}, "second")
	if err != nil {
		t.Fatalf("PutOrphan again: %v", err)
	}
	if first == second {
		t.Fatal("two different documents produced the same commit")
	}

	// One commit, always. A state branch that accumulated history would grow
	// without bound at one commit per transition — and every one of those commits
	// is a webhook delivery.
	if depth := runGit(t, remote, "rev-list", "--count", "state/controller"); depth != "1" {
		t.Fatalf("state/controller is %s commits deep, want 1", depth)
	}
	if parents := runGit(t, remote, "rev-list", "--parents", "-1", "state/controller"); parents != second {
		t.Fatalf("commit has parents: %q; an orphan commit must have none", parents)
	}
	if head := runGit(t, remote, "rev-parse", "state/controller"); head != second {
		t.Fatalf("remote head = %s, PutOrphan reported %s", head, second)
	}
	fsck(t, remote)
}

func TestPutOrphanLeavesOtherBranchesAlone(t *testing.T) {
	t.Parallel()
	// The controller force-pushes its own branch on every transition. If that could
	// disturb the agent's, single-writer ownership would buy nothing.
	remote := seedRemote(t, liveish())
	r := openMirror(t, remote)

	if _, err := r.PutOrphan(t.Context(), "state/agent",
		map[string][]byte{"agent.json": []byte(`{"phase":"running"}`)}, "agent"); err != nil {
		t.Fatalf("publish agent: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := r.PutOrphan(t.Context(), "state/controller",
			map[string][]byte{"controller.json": []byte(`{"version":1}`)}, "controller"); err != nil {
			t.Fatalf("publish controller: %v", err)
		}
	}

	if got, ok := remoteFile(t, remote, "state/agent", "agent.json"); !ok || !strings.Contains(got, "running") {
		t.Fatalf("agent.json after three controller publishes = %q, ok=%v", got, ok)
	}
	if got, ok := remoteFile(t, remote, "main", "config.yaml"); !ok {
		t.Fatalf("config.yaml on main disappeared: %q", got)
	}
}

func TestPutOrphanRefusesAnEmptyTree(t *testing.T) {
	t.Parallel()
	r := openMirror(t, seedRemote(t, liveish()))
	if _, err := r.PutOrphan(t.Context(), "state/controller", nil, "nothing"); err == nil {
		t.Fatal("PutOrphan accepted an empty file set; publishing nothing would read as a wiped document")
	}
}

// TestWriteTreeUsesGitsSortOrder pins the one piece of git's on-disk format we
// reimplement. Names are chosen so a plain lexical sort disagrees with git:
// "foo.txt" must precede the subtree "foo", because a directory compares as if
// its name ended in "/" ('/' is 0x2F, '.' is 0x2E). go-git reads a wrongly sorted
// tree back without complaint, so only git can catch this.
func TestWriteTreeUsesGitsSortOrder(t *testing.T) {
	t.Parallel()
	remote := seedRemote(t, liveish())
	r := openMirror(t, remote)

	if _, err := r.PutOrphan(t.Context(), "state/controller", map[string][]byte{
		"foo.txt":       []byte("blob before the subtree\n"),
		"foo/inner":     []byte("nested\n"),
		"foo/deep/leaf": []byte("deeper\n"),
		"fop":           []byte("after\n"),
		"a":             []byte("first\n"),
	}, "sort order"); err != nil {
		t.Fatalf("PutOrphan: %v", err)
	}

	fsck(t, remote)

	got := runGit(t, remote, "ls-tree", "--name-only", "state/controller")
	want := "a\nfoo.txt\nfoo\nfop"
	if strings.ReplaceAll(got, "\r\n", "\n") != want {
		t.Fatalf("tree order:\n got %q\nwant %q", got, want)
	}
	if body, ok := remoteFile(t, remote, "state/controller", "foo/deep/leaf"); !ok || body != "deeper" {
		t.Fatalf("nested blob = %q, ok=%v", body, ok)
	}
}

func TestCommitIsAFastForwardThatPreservesSiblings(t *testing.T) {
	t.Parallel()
	remote := seedRemote(t, liveish())
	r := openMirror(t, remote)
	before := runGit(t, remote, "rev-parse", "main")

	sha, err := r.Commit(t.Context(), "main", Mutation{
		Put:    map[string][]byte{"controller.json": []byte(`{"version":1}`)},
		Delete: []string{"triggers/start"},
	}, "single-layout write")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Ordinary child commit: history is preserved, unlike a state branch.
	if parents := runGit(t, remote, "rev-list", "--parents", "-1", "main"); parents != sha+" "+before {
		t.Fatalf("parents = %q, want %q", parents, sha+" "+before)
	}
	// The operator's files must survive a write that only mentions ours. This is
	// the `single` layout, where state shares a branch with config.yaml; treating
	// it like a dedicated branch would delete the config the system reads.
	for _, path := range []string{"config.yaml", "README.md", "triggers/backup-please"} {
		if _, ok := remoteFile(t, remote, "main", path); !ok {
			t.Fatalf("%s was deleted by a state write", path)
		}
	}
	if _, ok := remoteFile(t, remote, "main", "triggers/start"); ok {
		t.Fatal("triggers/start survived its deletion")
	}
	fsck(t, remote)
}

func TestCommitOfNoChangeMakesNoCommit(t *testing.T) {
	t.Parallel()
	// An empty commit is still a push, a push is still a webhook delivery, and a
	// delivery is still a reconcile. That is the shape of v1's event loop, so a
	// no-op has to be silent all the way down.
	remote := seedRemote(t, liveish())
	r := openMirror(t, remote)
	before := runGit(t, remote, "rev-parse", "main")

	for _, mut := range []Mutation{
		{Put: map[string][]byte{"config.yaml": []byte(liveish()["config.yaml"])}},
		{Delete: []string{"triggers/never-existed"}},
		{},
	} {
		sha, err := r.Commit(t.Context(), "main", mut, "no change")
		if err != nil {
			t.Fatalf("Commit(%+v): %v", mut, err)
		}
		if sha != "" {
			t.Fatalf("Commit(%+v) made commit %s for an unchanged tree", mut, sha)
		}
	}
	if after := runGit(t, remote, "rev-parse", "main"); after != before {
		t.Fatalf("main moved from %s to %s without a change", before, after)
	}
	if len(r.Pushed()) != 0 {
		t.Fatalf("no-op commits registered as our pushes: %v", r.Pushed())
	}
}

func TestCommitRefusesToEmptyABranch(t *testing.T) {
	t.Parallel()
	remote := seedRemote(t, map[string]string{"only.txt": "sole file\n"})
	r := openMirror(t, remote)

	if _, err := r.Commit(t.Context(), "main", Mutation{Delete: []string{"only.txt"}}, "wipe"); err == nil {
		t.Fatal("Commit emptied a branch; a tree with nothing in it is far likelier to be our bug than intent")
	}
	if _, ok := remoteFile(t, remote, "main", "only.txt"); !ok {
		t.Fatal("the file was deleted anyway")
	}
}

func TestCommitFailsSafelyWhenTheOperatorPushedFirst(t *testing.T) {
	t.Parallel()
	remote := seedRemote(t, liveish())
	r := openMirror(t, remote)

	// An operator pushes while we hold a stale mirror.
	work := t.TempDir()
	runGit(t, work, "clone", remote, ".")
	runGit(t, work, "-c", "user.name=op", "-c", "user.email=op@example.invalid",
		"commit", "--allow-empty", "-m", "operator edit")
	runGit(t, work, "push", "origin", "main")
	operatorHead := runGit(t, remote, "rev-parse", "main")

	_, err := r.Commit(t.Context(), "main",
		Mutation{Delete: []string{"triggers/start"}}, "consume trigger")
	if !errors.Is(err, ErrNotFastForward) {
		t.Fatalf("Commit onto a moved branch = %v, want ErrNotFastForward", err)
	}
	// Their commit must still be there. Forcing here would silently discard an
	// operator's work to tidy our own bookkeeping.
	if head := runGit(t, remote, "rev-parse", "main"); head != operatorHead {
		t.Fatalf("main = %s, operator left it at %s", head, operatorHead)
	}

	// And the local ref must have been rolled back, so a retry after Fetch builds
	// on the operator's commit rather than on the rejected one.
	if err := r.Fetch(t.Context()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if _, err := r.Commit(t.Context(), "main",
		Mutation{Delete: []string{"triggers/start"}}, "consume trigger (retry)"); err != nil {
		t.Fatalf("retry after Fetch: %v", err)
	}
	if _, ok := remoteFile(t, remote, "main", "triggers/start"); ok {
		t.Fatal("retry did not delete the trigger")
	}
	if out, err := tryGit(t, remote, "merge-base", "--is-ancestor", operatorHead, "main"); err != nil {
		t.Fatalf("the retry did not build on the operator's commit: %v\n%s", err, out)
	}
}

func TestSelfPushFilterRemembersOurCommitsAndIsBounded(t *testing.T) {
	t.Parallel()
	remote := seedRemote(t, liveish())
	r := openMirror(t, remote)

	sha, err := r.PutOrphan(t.Context(), "state/controller",
		map[string][]byte{"controller.json": []byte(`{"version":1}`)}, "ours")
	if err != nil {
		t.Fatalf("PutOrphan: %v", err)
	}
	if !r.WasOurs(sha) {
		t.Fatal("we do not recognise a commit we just pushed; the webhook echo would be acted on")
	}
	if theirs := runGit(t, remote, "rev-parse", "main"); r.WasOurs(theirs) {
		t.Fatal("an operator commit was mistaken for ours; a real request would be dropped")
	}

	// The set is bounded, and the bound evicts oldest-first.
	for i := 0; i < pushedCap+10; i++ {
		r.recordPush(fmt.Sprintf("%040d", i))
	}
	if got := len(r.Pushed()); got > pushedCap {
		t.Fatalf("self-push set holds %d SHAs, cap is %d", got, pushedCap)
	}
	if r.WasOurs(sha) {
		t.Fatal("the oldest entry survived eviction, so the set is not really bounded")
	}
}
