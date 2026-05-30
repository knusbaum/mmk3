package runtime

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Graph resolves target+verb and prints the dependency tree to stdout.
// When full is true, subproject delegations are expanded by recursively
// invoking `mmk -graph -full` in the subproject directory and splicing
// the result under the delegating node.
func (b *Build) Graph(target, verb string, full bool) error {
	return b.GraphTo(os.Stdout, target, verb, full)
}

// GraphTo is like Graph but writes to an arbitrary io.Writer (useful for tests).
// Order-only edges (from nodes implementing OrderDependencies) are shown with
// an "(order)" tag, but only for targets that are independently reachable from
// the root via regular deps — matching the dag library's actual behavior.
func (b *Build) GraphTo(w io.Writer, target, verb string, full bool) error {
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
	if err := checkVerbHasTargets(root); err != nil {
		return err
	}

	// First pass: collect all nodes reachable from root via regular deps.
	// Order-only edges that point outside this set are dropped from the display.
	inGraph := make(map[*TargetNode]bool)
	var collectErr error
	var collect func(*TargetNode)
	collect = func(n *TargetNode) {
		if inGraph[n] {
			return
		}
		inGraph[n] = true
		for _, d := range n.Dependencies() {
			collect(d)
		}
		if n.resolveErr != nil && collectErr == nil {
			collectErr = n.resolveErr
		}
	}
	collect(root)
	if collectErr != nil {
		return collectErr
	}

	gp := &graphPrinter{w: w, build: b, inGraph: inGraph, full: full, visited: map[string]bool{}}
	gp.visited[nodeKey(root)] = true
	fmt.Fprintln(w, nodeLabel(root))
	gp.printChildren(root, "")
	return nil
}

// graphPrinter carries per-call state for tree rendering (writer, full flag,
// reachability set, visited cache) so the recursion doesn't need to thread
// six parameters.
type graphPrinter struct {
	w       io.Writer
	build   *Build
	inGraph map[*TargetNode]bool
	full    bool
	visited map[string]bool
}

// printChildren renders the visible deps of n under prefix. When -full is set
// and n is a subproject delegation, the sub-mmk's graph is appended as the
// last child.
func (gp *graphPrinter) printChildren(n *TargetNode, prefix string) {
	deps := visibleDeps(n, gp.inGraph)

	var subOutput string
	if gp.full {
		if path, args, ok := gp.build.subprojectDelegate(n.target, n.verb); ok {
			subOutput = runSubGraph(path, args)
		}
	}

	// Preserve the orderly flag per node before GroupSiblings reorders.
	orderlyOf := make(map[*TargetNode]bool, len(deps))
	nodes := make([]*TargetNode, len(deps))
	for i, d := range deps {
		nodes[i] = d.node
		orderlyOf[d.node] = d.orderly
	}

	groups := GroupSiblings(nodes)
	total := len(groups)
	if subOutput != "" {
		total++
	}
	for i, g := range groups {
		isLast := i == total-1
		if g.IsGroup() {
			gp.printGroupNode(g, prefix, isLast)
		} else {
			gp.printNode(displayDep{node: g.Node, orderly: orderlyOf[g.Node]}, prefix, isLast)
		}
	}
	if subOutput != "" {
		spliceSubGraph(gp.w, subOutput, prefix, true)
	}
}

// printGroupNode renders a collapsed group header line followed by the union
// of deps from all members.
func (gp *graphPrinter) printGroupNode(g SiblingGroup, prefix string, isLast bool) {
	connector := "├── "
	childPrefix := prefix + "│   "
	if isLast {
		connector = "└── "
		childPrefix = prefix + "    "
	}
	label := fmt.Sprintf("'%s' × %d", g.Pattern, len(g.Members))
	if g.Verb != "" {
		label = fmt.Sprintf("[%s '%s'] × %d", g.Verb, g.Pattern, len(g.Members))
	}
	fmt.Fprintf(gp.w, "%s%s%s\n", prefix, connector, label)
	gp.printGroupChildren(g.Members, childPrefix)
}

// printGroupChildren renders the union of DisplayDeps across all members,
// applying GroupSiblings so nested pattern groups also collapse.
func (gp *graphPrinter) printGroupChildren(members []*TargetNode, prefix string) {
	seen := map[string]bool{}
	var combined []*TargetNode
	for _, m := range members {
		for _, d := range m.DisplayDeps() {
			k := nodeKey(d)
			if seen[k] {
				continue
			}
			seen[k] = true
			combined = append(combined, d)
		}
	}
	groups := GroupSiblings(combined)
	for i, g := range groups {
		isLast := i == len(groups)-1
		if g.IsGroup() {
			gp.printGroupNode(g, prefix, isLast)
		} else {
			gp.printNode(displayDep{node: g.Node}, prefix, isLast)
		}
	}
}

func (gp *graphPrinter) printNode(dd displayDep, prefix string, isLast bool) {
	connector := "├── "
	childPrefix := prefix + "│   "
	if isLast {
		connector = "└── "
		childPrefix = prefix + "    "
	}
	n := dd.node
	key := nodeKey(n)
	tag := ""
	if dd.orderly {
		tag = " (order)"
	}
	if gp.visited[key] {
		fmt.Fprintf(gp.w, "%s%s%s%s (*)\n", prefix, connector, nodeLabel(n), tag)
		return
	}
	gp.visited[key] = true
	fmt.Fprintf(gp.w, "%s%s%s%s\n", prefix, connector, nodeLabel(n), tag)
	gp.printChildren(n, childPrefix)
}

// runSubGraph invokes `mmk -graph -full <args>` in the given path and returns
// stdout. The current binary is reused (os.Executable) so a recursive -full
// from a freshly-built mmk doesn't accidentally pick up an older `mmk` from
// PATH. Errors are folded into the returned string so the parent's tree
// rendering doesn't abort on a malformed sub-mmkfile.
func runSubGraph(path string, args []string) string {
	exe, err := os.Executable()
	if err != nil {
		exe = "mmk"
	}
	cmdArgs := append([]string{"-graph", "-full"}, args...)
	cmd := exec.Command(exe, cmdArgs...)
	cmd.Dir = path
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Sprintf("(mmk -graph in %s failed: %v)\n", path, err)
	}
	return out.String()
}

// spliceSubGraph writes sub-mmk graph output as the last child under the
// caller's prefix. The first line uses an arrow connector (└─▶) to mark the
// edge as "expansion of" rather than "dependency of" — the sub-graph isn't
// a dep of the parent node, it's what running that node actually does.
// Continuation prefix matches the width of the regular tee/elbow so columns
// inside the spliced subtree line up the same as the rest of the tree.
func spliceSubGraph(w io.Writer, output, prefix string, isLast bool) {
	connector := "├─▶ "
	childPrefix := prefix + "│   "
	if isLast {
		connector = "└─▶ "
		childPrefix = prefix + "    "
	}
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	for i, line := range lines {
		if i == 0 {
			fmt.Fprintf(w, "%s%s%s\n", prefix, connector, line)
		} else {
			fmt.Fprintf(w, "%s%s\n", childPrefix, line)
		}
	}
}

// subprojectDelegate returns (path, subArgs, ok) for a node that delegates
// to a subproject — either a root subproject target like "src" or a subpath
// target like "src/build". subArgs are the positional args to pass to the
// sub-mmk (verb first if present, then the in-subproject target name).
func (b *Build) subprojectDelegate(target, verb string) (string, []string, bool) {
	if sp, ok := b.subprojects[target]; ok {
		var args []string
		if verb != "" {
			args = append(args, verb)
		}
		return sp.path, args, true
	}
	i := strings.IndexByte(target, '/')
	if i <= 0 {
		return "", nil, false
	}
	sp, ok := b.subprojects[target[:i]]
	if !ok {
		return "", nil, false
	}
	suffix := target[i+1:]
	var args []string
	if verb != "" {
		args = append(args, verb)
	}
	args = append(args, suffix)
	return sp.path, args, true
}

// displayDep wraps a dependency with how it should be rendered.
type displayDep struct {
	node    *TargetNode
	orderly bool // true for order-only edges
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
		if shouldPruneVerbSubgraph(d) {
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
		if shouldPruneVerbSubgraph(d) {
			continue
		}
		result = append(result, displayDep{node: d, orderly: true})
	}
	return result
}

// shouldPruneVerbSubgraph returns true when n is a verb node whose entire
// dependency subgraph contains no node with a body to run. Non-verb nodes
// are never pruned — they're real build steps. A verb node with its own
// body is never pruned. A virtual / inherited verb node (no own body) is
// pruned only when none of its descendants has work either.
func shouldPruneVerbSubgraph(n *TargetNode) bool {
	if n.verb == "" {
		return false
	}
	return !subgraphHasBody(n, make(map[*TargetNode]bool))
}

// SiblingGroup is either a single node or a collapsed group of
// pattern-instantiated siblings sharing the same source pattern and verb.
// Display layers call GroupSiblings to partition a sibling list, then render
// groups as one summary line followed by their shared deps.
type SiblingGroup struct {
	Node    *TargetNode   // non-nil for individual (non-group) items
	Pattern string        // source pattern (non-empty for groups)
	Verb    string        // verb (for groups with a verb)
	Members []*TargetNode // group members (non-empty for groups)
}

// IsGroup reports whether this item represents a collapsed pattern group.
func (g SiblingGroup) IsGroup() bool { return g.Node == nil }

// siblingCollapseThreshold is the minimum number of same-pattern siblings
// that triggers collapsing them into a single summary line.
const siblingCollapseThreshold = 10

// GroupSiblings partitions children into individual nodes and collapsed groups.
// Nodes that share the same source pattern (and verb) are grouped when there
// are more than siblingCollapseThreshold of them. Groups are placed at the
// position of their first member in the original order.
func GroupSiblings(children []*TargetNode) []SiblingGroup {
	counts := map[string]int{}
	for _, c := range children {
		if p := c.SourcePattern(); p != "" {
			counts[c.Verb()+"\x00"+p]++
		}
	}
	rendered := map[string]bool{}
	var out []SiblingGroup
	for _, c := range children {
		p := c.SourcePattern()
		if p == "" || counts[c.Verb()+"\x00"+p] <= siblingCollapseThreshold {
			out = append(out, SiblingGroup{Node: c})
			continue
		}
		key := c.Verb() + "\x00" + p
		if rendered[key] {
			continue
		}
		rendered[key] = true
		var members []*TargetNode
		for _, c2 := range children {
			if c2.SourcePattern() == p && c2.Verb() == c.Verb() {
				members = append(members, c2)
			}
		}
		out = append(out, SiblingGroup{
			Pattern: p,
			Verb:    c.Verb(),
			Members: members,
		})
	}
	return out
}
