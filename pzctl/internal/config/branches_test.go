package config

import "testing"

// TestBranchLayoutResolvesTheSingleLayout pins the one thing the layout switch
// has to get right. In the single layout config leaves the two state branch
// fields empty — validation does not require them — so a caller that read them
// unresolved would name the empty branch, and gitbus would then treat "" as a
// dedicated branch and force-push a whole-tree replace to it.
func TestBranchLayoutResolvesTheSingleLayout(t *testing.T) {
	branches := Git{
		Branch:                "main",
		Layout:                LayoutBranches,
		ControllerStateBranch: "state/controller",
		AgentStateBranch:      "state/agent",
		TriggersDir:           "triggers",
	}.BranchLayout()
	if branches.Main != "main" || branches.Controller != "state/controller" ||
		branches.Agent != "state/agent" || branches.TriggersDir != "triggers" {
		t.Fatalf("branches layout = %+v", branches)
	}

	// The state branch fields are deliberately left empty, as an operator using
	// the single layout would leave them.
	single := Git{Branch: "main", Layout: LayoutSingle, TriggersDir: "triggers"}.BranchLayout()
	if single.Controller != "main" || single.Agent != "main" {
		t.Fatalf("single layout = %+v; both state branches must resolve to main", single)
	}

	// An unset layout must not resolve to empty branch names either. Validation
	// rejects it, but a zero Config is still constructed in tests and by tools.
	zero := Git{Branch: "main", TriggersDir: "triggers"}.BranchLayout()
	if zero.Controller == "" || zero.Agent == "" {
		t.Fatalf("unset layout = %+v; an empty branch name is never a valid ref", zero)
	}
}

// TestBranchLayoutMatchesTheShippedConfig checks the real config.yaml resolves to
// three distinct branches, since that separation is what invariant I4 rests on.
func TestBranchLayoutMatchesTheShippedConfig(t *testing.T) {
	path, err := Find("")
	if err != nil {
		t.Skipf("no config.yaml to check: %v", err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%s): %v", path, err)
	}
	bl := c.Git.BranchLayout()
	if c.Git.Layout != LayoutBranches {
		t.Skipf("shipped config uses layout %q", c.Git.Layout)
	}
	seen := map[string]bool{}
	for _, b := range []string{bl.Main, bl.Controller, bl.Agent} {
		if b == "" {
			t.Fatalf("empty branch in %+v", bl)
		}
		if seen[b] {
			t.Fatalf("branch %q is used twice in %+v; single-writer ownership depends on them differing", b, bl)
		}
		seen[b] = true
	}
}
