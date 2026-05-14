package runtime

import (
	"strings"
	"testing"
)

func TestParentIndexBuildsBFSChain(t *testing.T) {
	// root → a → b → c, plus root → x (sibling branch).
	src := `
all : a x
a : b
b : c
c :
x :
`
	b := newBuild(t, src)
	root, err := b.Resolve("all")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Materialize deps so the BFS can walk them. Resolve doesn't eagerly
	// pull Dependencies; the dag.Build pass does. Calling Dependencies
	// transitively forces resolution.
	var force func(n *TargetNode)
	force = func(n *TargetNode) {
		for _, d := range n.Dependencies() {
			force(d)
		}
	}
	force(root)

	pi := buildParentIndex(root)

	c := b.NodeFor("c", "")
	if c == nil {
		t.Fatal("NodeFor(c): nil")
	}
	chain := pi.path(c)
	if got := names(chain); !equal(got, []string{"all", "a", "b", "c"}) {
		t.Errorf("path(c) = %v; want [all a b c]", got)
	}

	x := b.NodeFor("x", "")
	if got := names(pi.path(x)); !equal(got, []string{"all", "x"}) {
		t.Errorf("path(x) = %v; want [all x]", got)
	}

	if got := names(pi.path(root)); !equal(got, []string{"all"}) {
		t.Errorf("path(root) = %v; want [all]", got)
	}
}

func TestParentIndexFirstParentWins(t *testing.T) {
	// Both `a` and `b` depend on `shared`. BFS visits `a` before `b` (deps
	// are iterated in declaration order), so shared.parent should be `a`.
	src := `
all : a b
a : shared
b : shared
shared :
`
	b := newBuild(t, src)
	root, err := b.Resolve("all")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	var force func(n *TargetNode)
	force = func(n *TargetNode) {
		for _, d := range n.Dependencies() {
			force(d)
		}
	}
	force(root)

	pi := buildParentIndex(root)
	shared := b.NodeFor("shared", "")
	chain := pi.path(shared)
	if got := names(chain); !equal(got, []string{"all", "a", "shared"}) {
		t.Errorf("path(shared) = %v; want [all a shared] (first-parent-wins)", got)
	}
}

func TestRenderWhyPathChain(t *testing.T) {
	// Render directly from a hand-built chain; no Build needed.
	chain := []*TargetNode{
		{target: "all"},
		{target: "prog"},
		{target: "build/lib.a"},
		{target: "build/lib/util.o"},
	}
	got := renderWhyPath(chain)
	want := "all\n" +
		"└── prog\n" +
		"    └── build/lib.a\n" +
		"        └── build/lib/util.o\n"
	if got != want {
		t.Errorf("renderWhyPath:\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestRenderWhyPathHandlesVerbNodes(t *testing.T) {
	chain := []*TargetNode{
		{target: "all", verb: "fmt"},
		{target: "main.c"},
	}
	got := renderWhyPath(chain)
	if !strings.Contains(got, "[fmt all]") {
		t.Errorf("expected verb-formatted root in output; got:\n%s", got)
	}
	if !strings.Contains(got, "└── main.c") {
		t.Errorf("expected child connector; got:\n%s", got)
	}
}

func TestRenderWhyPathEmpty(t *testing.T) {
	if got := renderWhyPath(nil); got != "" {
		t.Errorf("renderWhyPath(nil) = %q; want empty", got)
	}
}

// names extracts target labels from a node chain.
func names(chain []*TargetNode) []string {
	out := make([]string, 0, len(chain))
	for _, n := range chain {
		out = append(out, n.target)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
