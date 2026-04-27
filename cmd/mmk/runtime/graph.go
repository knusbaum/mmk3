package runtime

import (
	"fmt"
	"io"
	"os"
)

// Graph resolves target+verb and prints the dependency tree to stdout.
func (b *Build) Graph(target, verb string) error {
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
	visited := make(map[string]bool)
	visited[nodeKey(root)] = true
	fmt.Fprintln(os.Stdout, nodeLabel(root))
	deps := visibleDeps(root)
	for i, dep := range deps {
		printTreeNode(os.Stdout, dep, "", i == len(deps)-1, visited)
	}
	return nil
}

func printTreeNode(w io.Writer, n *TargetNode, prefix string, isLast bool, visited map[string]bool) {
	connector := "├── "
	if isLast {
		connector = "└── "
	}
	key := nodeKey(n)
	label := nodeLabel(n)
	if visited[key] {
		fmt.Fprintf(w, "%s%s%s (*)\n", prefix, connector, label)
		return
	}
	visited[key] = true
	fmt.Fprintf(w, "%s%s%s\n", prefix, connector, label)
	childPrefix := prefix + "│   "
	if isLast {
		childPrefix = prefix + "    "
	}
	deps := visibleDeps(n)
	for i, dep := range deps {
		printTreeNode(w, dep, childPrefix, i == len(deps)-1, visited)
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

func visibleDeps(n *TargetNode) []*TargetNode {
	var result []*TargetNode
	for _, d := range n.Dependencies() {
		if d.kind != kindRunner {
			result = append(result, d)
		}
	}
	return result
}
