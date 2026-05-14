package runtime

import (
	"fmt"
	"strings"
)

// parentIndex maps each node reachable from a fixed root (via regular
// Dependencies) to the first parent discovered during BFS. The root maps to
// nil. A node in a DAG can have multiple parents; BFS-discovered "shortest
// path from root" is sufficient to explain why a node is running.
type parentIndex struct {
	parents map[*TargetNode]*TargetNode
}

func buildParentIndex(root *TargetNode) *parentIndex {
	pi := &parentIndex{parents: map[*TargetNode]*TargetNode{root: nil}}
	queue := []*TargetNode{root}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, d := range n.Dependencies() {
			if _, seen := pi.parents[d]; seen {
				continue
			}
			pi.parents[d] = n
			queue = append(queue, d)
		}
	}
	return pi
}

// path returns the chain of nodes from root down to n, inclusive, with
// runner nodes filtered out (runners are shared infrastructure, not part
// of the dep chain that explains "what work is this part of"). Returns nil
// when n was never reached from the indexed root.
func (pi *parentIndex) path(n *TargetNode) []*TargetNode {
	if _, ok := pi.parents[n]; !ok {
		return nil
	}
	var chain []*TargetNode
	for cur := n; cur != nil; cur = pi.parents[cur] {
		if cur.kind == kindRunner {
			continue
		}
		chain = append(chain, cur)
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}

// renderWhyPath formats a single-chain path with the standard tree connectors.
// Because the chain has exactly one node per level (it's a path, not a tree),
// every connector is the "last child" form (└──).
//
//	root
//	└── intermediate
//	    └── leaf
//
// Returns the rendered string so callers can write it under a mutex (Execute
// calls this from concurrent OnRun hooks).
func renderWhyPath(chain []*TargetNode) string {
	if len(chain) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintln(&sb, nodeLabel(chain[0]))
	prefix := ""
	for _, n := range chain[1:] {
		fmt.Fprintf(&sb, "%s└── %s\n", prefix, nodeLabel(n))
		prefix += "    "
	}
	return sb.String()
}
