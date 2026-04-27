package runtime

import (
	"fmt"
	"io"
	"os"
)

// Graph resolves target+verb and prints the dependency tree to stdout.
func (b *Build) Graph(target, verb string) error {
	return b.GraphTo(os.Stdout, target, verb)
}

// GraphTo is like Graph but writes to an arbitrary io.Writer (useful for tests).
// Order-only edges (from nodes implementing OrderDependencies) are shown with
// an "(order)" tag, but only for targets that are independently reachable from
// the root via regular deps — matching the dag library's actual behavior.
func (b *Build) GraphTo(w io.Writer, target, verb string) error {
	var root *TargetNode
	var err error
	if verb == "" {
		root, err = b.Resolve(target)
	} else {
		root, err = b.ResolveVerb(target, verb)
	}
	if err != nil {
		return err
	}
	if err := checkVerbApplicable(root); err != nil {
		return err
	}

	// First pass: collect all nodes reachable from root via regular deps.
	// Order-only edges that point outside this set are dropped from the display.
	inGraph := make(map[*TargetNode]bool)
	var collect func(*TargetNode)
	collect = func(n *TargetNode) {
		if inGraph[n] {
			return
		}
		inGraph[n] = true
		for _, d := range n.Dependencies() {
			collect(d)
		}
	}
	collect(root)

	visited := make(map[string]bool)
	visited[nodeKey(root)] = true
	fmt.Fprintln(w, nodeLabel(root))
	deps := visibleDeps(root, inGraph)
	for i, dep := range deps {
		printTreeNode(w, dep, "", i == len(deps)-1, visited, inGraph)
	}
	return nil
}

// displayDep wraps a dependency with how it should be rendered.
type displayDep struct {
	node    *TargetNode
	orderly bool // true for order-only edges
}

func printTreeNode(w io.Writer, dd displayDep, prefix string, isLast bool, visited map[string]bool, inGraph map[*TargetNode]bool) {
	connector := "├── "
	if isLast {
		connector = "└── "
	}
	n := dd.node
	key := nodeKey(n)
	label := nodeLabel(n)
	tag := ""
	if dd.orderly {
		tag = " (order)"
	}
	if visited[key] {
		fmt.Fprintf(w, "%s%s%s%s (*)\n", prefix, connector, label, tag)
		return
	}
	visited[key] = true
	fmt.Fprintf(w, "%s%s%s%s\n", prefix, connector, label, tag)
	childPrefix := prefix + "│   "
	if isLast {
		childPrefix = prefix + "    "
	}
	deps := visibleDeps(n, inGraph)
	for i, dep := range deps {
		printTreeNode(w, dep, childPrefix, i == len(deps)-1, visited, inGraph)
	}
}

func nodeLabel(n *TargetNode) string {
	if n.verb != "" {
		return "[" + n.verb + " " + n.target + "]"
	}
	return n.target
}

func nodeKey(n *TargetNode) string {
	return n.verb + "\x00" + n.target
}

// visibleDeps returns the regular deps followed by any order-only deps whose
// targets are also reachable from the root. Order-only edges to nodes that
// aren't in the graph (e.g. when only the runner-image's verb is requested)
// are dropped, matching the dag library's runtime behavior.
//
// Verb subtrees with no executable body anywhere (e.g. [update python] when
// nothing in python's tree defines an update verb) are pruned — they're
// noise that obscures the real work without conveying useful structure.
func visibleDeps(n *TargetNode, inGraph map[*TargetNode]bool) []displayDep {
	var result []displayDep
	for _, d := range n.Dependencies() {
		if d.kind == kindRunner {
			continue
		}
		if shouldPruneVerbSubtree(d) {
			continue
		}
		result = append(result, displayDep{node: d, orderly: false})
	}
	for _, d := range n.OrderDependencies() {
		if d.kind == kindRunner {
			continue
		}
		if !inGraph[d] {
			continue
		}
		if shouldPruneVerbSubtree(d) {
			continue
		}
		result = append(result, displayDep{node: d, orderly: true})
	}
	return result
}

// shouldPruneVerbSubtree returns true when n is a verb node whose entire
// subtree contains no executable verb body. Non-verb nodes are never pruned
// — they're real build steps. A verb node with its own body is never pruned.
// A virtual / inherited verb node (no own body) is pruned only when none of
// its descendants has work either.
func shouldPruneVerbSubtree(n *TargetNode) bool {
	if n.verb == "" {
		return false
	}
	return !hasApplicableVerbBody(n, make(map[*TargetNode]bool))
}
