package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/knusbaum/mmk3/cmd/mmk/runtime"
)

// When >10 pattern siblings collapse into a single group line, the tree
// must still surface the deps those siblings share — otherwise the TUI's
// tree drops information that mmk -graph shows. Regression for "× N"
// group rendering swallowing all dep edges.
func TestBuildTreeCollapsedGroupShowsSharedDeps(t *testing.T) {
	var names []string
	for i := 0; i < 11; i++ {
		names = append(names, fmt.Sprintf("o%d.o", i))
	}
	src := "all : " + strings.Join(names, " ") + "\n" +
		"'(.*)\\.o' : header.h libdep\n" +
		"header.h :\n" +
		"libdep :\n"
	b, err := runtime.NewBuild([]byte(src))
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}
	defer b.Close()
	root, err := b.Resolve("all")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	tree := buildTree(root)

	groupIdx := -1
	for i, ln := range tree.lines {
		if strings.Contains(ln.label, "× 11") {
			groupIdx = i
			break
		}
	}
	if groupIdx < 0 {
		t.Fatalf("expected a '× 11' group line in tree:\n%s", treeDump(tree))
	}
	foundHeader, foundLib := false, false
	for _, ln := range tree.lines[groupIdx+1:] {
		if strings.Contains(ln.label, "header.h") {
			foundHeader = true
		}
		if strings.Contains(ln.label, "libdep") {
			foundLib = true
		}
	}
	if !foundHeader || !foundLib {
		t.Errorf("expected header.h and libdep under group; got:\n%s", treeDump(tree))
	}
}

func treeDump(t treeData) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", t.rootLabel)
	for _, ln := range t.lines {
		fmt.Fprintf(&b, "%s%s\n", ln.prefix, ln.label)
	}
	return b.String()
}
