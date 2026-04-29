package dagview

import (
	"fmt"
	"strings"
	"testing"
)

// fakeNode lets us drive the layout/render code without spinning up a real
// runtime.Build. It exposes the fields that the renderer reads via the
// graph-building shim below.
type fakeNode struct {
	target  string
	verb    string
	pattern string
	deps    []*fakeNode
}

// We bypass buildBoxGraph(*runtime.TargetNode) and assemble box graphs
// manually so this test stays in-package and self-contained.
func mkBoxes(roots []*fakeNode, threshold int) *boxGraph {
	visited := map[*fakeNode]bool{}
	var nodes []*fakeNode
	var walk func(n *fakeNode)
	walk = func(n *fakeNode) {
		if visited[n] {
			return
		}
		visited[n] = true
		nodes = append(nodes, n)
		for _, d := range n.deps {
			walk(d)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	groupKey := func(n *fakeNode) (string, bool) {
		if n.pattern != "" {
			return "pat\x00" + n.verb + "\x00" + n.pattern, true
		}
		return "one\x00" + n.verb + "\x00" + n.target, false
	}
	counts := map[string]int{}
	for _, n := range nodes {
		k, isPat := groupKey(n)
		if isPat {
			counts[k]++
		}
	}
	nodeToBox := map[*fakeNode]*box{}
	boxByID := map[string]*box{}
	for _, n := range nodes {
		k, isPat := groupKey(n)
		shouldGroup := isPat && counts[k] >= threshold
		var id string
		if shouldGroup {
			id = k
		} else {
			id = "one\x00" + n.verb + "\x00" + n.target
		}
		if b, ok := boxByID[id]; ok {
			b.keys = append(b.keys, nodeKey(n.target, n.verb))
			nodeToBox[n] = b
			continue
		}
		b := &box{id: id, keys: []string{nodeKey(n.target, n.verb)}}
		if shouldGroup {
			b.isGroup = true
			b.title = fmt.Sprintf("'%s' x %d", n.pattern, counts[k])
			if n.verb != "" {
				b.title = fmt.Sprintf("[%s '%s'] x %d", n.verb, n.pattern, counts[k])
			}
		} else {
			b.title = n.target
			if n.verb != "" {
				b.title = "[" + n.verb + " " + n.target + "]"
			}
		}
		boxByID[id] = b
		nodeToBox[n] = b
	}
	type ek struct{ from, to *box }
	seenEdge := map[ek]bool{}
	for _, n := range nodes {
		from := nodeToBox[n]
		for _, d := range n.deps {
			to := nodeToBox[d]
			if from == to {
				continue
			}
			if seenEdge[ek{from, to}] {
				continue
			}
			seenEdge[ek{from, to}] = true
			from.deps = append(from.deps, to)
			to.parents = append(to.parents, from)
		}
	}
	bg := &boxGraph{}
	seen := map[*box]bool{}
	for _, n := range nodes {
		b := nodeToBox[n]
		if !seen[b] {
			seen[b] = true
			bg.boxes = append(bg.boxes, b)
		}
	}
	return bg
}

func renderFake(bg *boxGraph, opts Options) string {
	opts.defaults()
	for _, b := range bg.boxes {
		b.computeContent(func(string, string) Status { return StatusPending }, opts.UseUnicode)
	}
	assignRows(bg)
	splitLongEdges(bg)
	assignCols(bg)
	c := newCanvas(bg, opts)
	c.draw(bg, opts)
	res := &Result{cells: c.cells, styles: c.styles, w: c.w, h: c.h}
	return res.Render(0, 0, res.w, res.h, false)
}

func TestRenderDiamond(t *testing.T) {
	leaf := &fakeNode{target: "leaf"}
	a := &fakeNode{target: "a", deps: []*fakeNode{leaf}}
	b := &fakeNode{target: "b", deps: []*fakeNode{leaf}}
	root := &fakeNode{target: "root", deps: []*fakeNode{a, b}}
	out := renderFake(mkBoxes([]*fakeNode{root}, 10), Options{UseUnicode: true})
	t.Log("\n" + out)
	if !strings.Contains(out, "root") || !strings.Contains(out, "leaf") {
		t.Fatalf("missing labels in output:\n%s", out)
	}
}

func TestRenderGrouping(t *testing.T) {
	// 12 .o files all sharing the same pattern → one group box.
	var objs []*fakeNode
	for i := range 12 {
		objs = append(objs, &fakeNode{
			target:  fmt.Sprintf("file%d.o", i),
			pattern: `(.*)\.o`,
		})
	}
	exe := &fakeNode{target: "executable", deps: objs}
	root := &fakeNode{target: "all", deps: []*fakeNode{exe}}
	out := renderFake(mkBoxes([]*fakeNode{root}, 10), Options{UseUnicode: true})
	t.Log("\n" + out)
	if !strings.Contains(out, "x 12") {
		t.Fatalf("expected group label 'x 12' in output:\n%s", out)
	}
	if strings.Contains(out, "file3.o") {
		t.Fatalf("group members should be collapsed, not visible:\n%s", out)
	}
}

func TestMatrixBaseParse(t *testing.T) {
	cases := []struct {
		in   string
		base string
		dims []string
	}{
		{"[downstream @ x=1 y=a]", "downstream", []string{"x", "y"}},
		{"[foo @ k=v]", "foo", []string{"k"}},
		{"executable", "", nil},
		{"[no-at]", "", nil},
		{"", "", nil},
		{"[base @ ]", "base", nil},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := matrixBase(c.in); got != c.base {
				t.Errorf("matrixBase(%q) = %q, want %q", c.in, got, c.base)
			}
			gotDims := matrixDimNames(c.in)
			if len(gotDims) != len(c.dims) {
				t.Fatalf("matrixDimNames(%q) = %v, want %v", c.in, gotDims, c.dims)
			}
			for i, d := range gotDims {
				if d != c.dims[i] {
					t.Errorf("dim[%d]: got %q want %q", i, d, c.dims[i])
				}
			}
		})
	}
}

func TestMatrixGroupTitle(t *testing.T) {
	got := matrixGroupTitle("", "downstream", []string{"x", "y"}, 16)
	want := "[downstream @ x= y=] x 16"
	if got != want {
		t.Errorf("title = %q, want %q", got, want)
	}
	got = matrixGroupTitle("check", "downstream", []string{"x"}, 4)
	want = "[check downstream @ x=] x 4"
	if got != want {
		t.Errorf("verbed title = %q, want %q", got, want)
	}
}

// mkBoxesMatrix builds a graph from fakeNodes where target names use the
// "[base @ k=v]" matrix format. It exercises the real buildBoxGraph code
// path through a tiny shim that maps fakeNodes to box state.
type matrixFakeNode struct {
	target  string
	verb    string
	pattern string
	deps    []*matrixFakeNode
}

// mkBoxesViaOpts replicates the box-building part of buildBoxGraph so we
// can drive matrix grouping without spinning up a runtime.Build. Kept in
// sync with buildBoxGraph by hand — if that fork drifts, this test will
// catch it via the integration assertion in TestMatrixGrouping.
func mkBoxesViaOpts(roots []*matrixFakeNode, opts Options) *boxGraph {
	opts.defaults()
	visited := map[*matrixFakeNode]bool{}
	var nodes []*matrixFakeNode
	var walk func(n *matrixFakeNode)
	walk = func(n *matrixFakeNode) {
		if visited[n] {
			return
		}
		visited[n] = true
		nodes = append(nodes, n)
		for _, d := range n.deps {
			walk(d)
		}
	}
	for _, r := range roots {
		walk(r)
	}

	const (
		kindSingle = iota
		kindPattern
		kindMatrix
	)
	type gi struct {
		key  string
		kind int
		base string
	}
	groupOf := func(n *matrixFakeNode) gi {
		if n.pattern != "" {
			return gi{key: "pat\x00" + n.verb + "\x00" + n.pattern, kind: kindPattern}
		}
		if opts.GroupMatrix {
			if base := matrixBase(n.target); base != "" {
				return gi{key: "mat\x00" + n.verb + "\x00" + base, kind: kindMatrix, base: base}
			}
		}
		return gi{key: "one\x00" + n.verb + "\x00" + n.target, kind: kindSingle}
	}
	counts := map[string]int{}
	for _, n := range nodes {
		g := groupOf(n)
		if g.kind != kindSingle {
			counts[g.key]++
		}
	}
	matrixDims := map[string][]string{}
	for _, n := range nodes {
		g := groupOf(n)
		if g.kind == kindMatrix {
			if _, ok := matrixDims[g.key]; !ok {
				matrixDims[g.key] = matrixDimNames(n.target)
			}
		}
	}
	nodeToBox := map[*matrixFakeNode]*box{}
	boxByID := map[string]*box{}
	for _, n := range nodes {
		g := groupOf(n)
		shouldGroup := false
		switch g.kind {
		case kindPattern:
			shouldGroup = counts[g.key] >= opts.GroupThreshold
		case kindMatrix:
			shouldGroup = counts[g.key] >= 2
		}
		var id string
		if shouldGroup {
			id = g.key
		} else {
			id = "one\x00" + n.verb + "\x00" + n.target
		}
		if b, ok := boxByID[id]; ok {
			b.keys = append(b.keys, nodeKey(n.target, n.verb))
			nodeToBox[n] = b
			continue
		}
		b := &box{id: id, keys: []string{nodeKey(n.target, n.verb)}}
		if shouldGroup {
			b.isGroup = true
			count := counts[g.key]
			switch g.kind {
			case kindPattern:
				if n.verb != "" {
					b.title = fmt.Sprintf("[%s '%s'] x %d", n.verb, n.pattern, count)
				} else {
					b.title = fmt.Sprintf("'%s' x %d", n.pattern, count)
				}
			case kindMatrix:
				b.title = matrixGroupTitle(n.verb, g.base, matrixDims[g.key], count)
			}
		} else {
			b.title = n.target
			if n.verb != "" {
				b.title = "[" + n.verb + " " + n.target + "]"
			}
		}
		boxByID[id] = b
		nodeToBox[n] = b
	}
	type ek struct{ from, to *box }
	seenEdge := map[ek]bool{}
	for _, n := range nodes {
		from := nodeToBox[n]
		for _, d := range n.deps {
			to := nodeToBox[d]
			if from == to {
				continue
			}
			if seenEdge[ek{from, to}] {
				continue
			}
			seenEdge[ek{from, to}] = true
			from.deps = append(from.deps, to)
			to.parents = append(to.parents, from)
		}
	}
	bg := &boxGraph{}
	seen := map[*box]bool{}
	for _, n := range nodes {
		b := nodeToBox[n]
		if !seen[b] {
			seen[b] = true
			bg.boxes = append(bg.boxes, b)
		}
	}
	return bg
}

func TestMatrixGrouping(t *testing.T) {
	// 16 combos of one base; with GroupMatrix=true they should fold.
	var combos []*matrixFakeNode
	for x := 1; x <= 4; x++ {
		for _, y := range []string{"a", "b", "c", "d"} {
			combos = append(combos, &matrixFakeNode{
				target: fmt.Sprintf("[downstream @ x=%d y=%s]", x, y),
			})
		}
	}
	root := &matrixFakeNode{target: "downstream", deps: combos}

	t.Run("on", func(t *testing.T) {
		bg := mkBoxesViaOpts([]*matrixFakeNode{root}, Options{GroupMatrix: true})
		// Should be 2 boxes: the aggregator + 1 collapsed group.
		if len(bg.boxes) != 2 {
			t.Fatalf("expected 2 boxes (root + collapsed group), got %d", len(bg.boxes))
		}
		var groupBox *box
		for _, b := range bg.boxes {
			if b.isGroup {
				groupBox = b
			}
		}
		if groupBox == nil {
			t.Fatalf("no group box found among %d boxes", len(bg.boxes))
		}
		want := "[downstream @ x= y=] x 16"
		if groupBox.title != want {
			t.Errorf("group title = %q, want %q", groupBox.title, want)
		}
	})

	t.Run("off", func(t *testing.T) {
		bg := mkBoxesViaOpts([]*matrixFakeNode{root}, Options{GroupMatrix: false})
		// Aggregator + 16 individual combo boxes.
		if len(bg.boxes) != 17 {
			t.Fatalf("expected 17 boxes, got %d", len(bg.boxes))
		}
	})

	t.Run("single combo not grouped", func(t *testing.T) {
		// A single combo with GroupMatrix=true should still render as one
		// individual box, not a "x 1" group.
		combo := &matrixFakeNode{target: "[only @ x=1]"}
		root2 := &matrixFakeNode{target: "only", deps: []*matrixFakeNode{combo}}
		bg := mkBoxesViaOpts([]*matrixFakeNode{root2}, Options{GroupMatrix: true})
		for _, b := range bg.boxes {
			if b.isGroup {
				t.Errorf("solitary combo should not be grouped: %q", b.title)
			}
		}
	})
}

// TestLongEdgeNotThroughBox guards the bug where an edge spanning more
// than one row drew a vertical line straight through any box that
// happened to share its column. The fix splits long edges into chains of
// dummy boxes; the median heuristic places dummies into clear column
// slots, and the rendered chain flows down a free corridor.
func TestLongEdgeNotThroughBox(t *testing.T) {
	// Topology: leaf → mid → goal (long edge spanning rows 1→4) plus an
	// obstacle chain mid → o1 → o2 → goal that fills the intermediate
	// rows. Without dummy splitting, the mid→goal edge would draw a
	// vertical line right through o1 and o2.
	leaf := &fakeNode{target: "leaf"}
	mid := &fakeNode{target: "mid", deps: []*fakeNode{leaf}}
	o1 := &fakeNode{target: "obstacle1", deps: []*fakeNode{mid}}
	o2 := &fakeNode{target: "obstacle2", deps: []*fakeNode{o1}}
	goal := &fakeNode{target: "goal", deps: []*fakeNode{mid, o2}}

	out := renderFake(mkBoxes([]*fakeNode{goal}, 10), Options{UseUnicode: true})
	t.Log("\n" + out)

	for _, name := range []string{"obstacle1", "obstacle2"} {
		assertNoEdgeThroughBox(t, out, name)
	}
}

// assertNoEdgeThroughBox finds the box whose label contains `name` and
// verifies that the area inside its borders (between ┌…┐ on the top row,
// │…│ on the content row, └…┘ on the bottom row) contains only the label
// content — no edge glyphs. It uses rune-indexed slicing because Unicode
// box-drawing chars are multi-byte.
func assertNoEdgeThroughBox(t *testing.T, out, name string) {
	t.Helper()
	lines := strings.Split(out, "\n")
	contentRow := -1
	for i, ln := range lines {
		if strings.Contains(ln, name) {
			contentRow = i
			break
		}
	}
	if contentRow < 1 {
		t.Fatalf("%s: not found in output:\n%s", name, out)
	}
	topRunes := []rune(lines[contentRow-1])
	left, right := -1, -1
	for i, r := range topRunes {
		if r == '┌' && left < 0 {
			left = i
		} else if r == '┐' {
			right = i
		}
	}
	if left < 0 || right < 0 || right <= left {
		t.Fatalf("%s: cannot locate box borders on row above content: %q", name, lines[contentRow-1])
	}
	contentRunes := []rune(lines[contentRow])
	if right >= len(contentRunes) {
		right = len(contentRunes) - 1
	}
	inside := contentRunes[left+1 : right]
	for i, r := range inside {
		switch r {
		case '│', '┬', '┴', '┼', '├', '┤':
			t.Fatalf("%s: edge glyph %q at column %d inside the box content:\n  content: %q\n  full:\n%s",
				name, r, left+1+i, string(contentRunes), out)
		}
	}
}

// TestDummyChainNoGaps guards a bug where the row immediately above each
// dummy stayed blank because that's the row the renderer normally fills
// with an arrowhead — for dummy targets we skip the arrow, but used to
// also skip drawing anything there. The result was a one-cell gap in the
// edge path right above each dummy.
//
// Test strategy: build the graph the way View() does so we know the dummy
// boxes' x/y coordinates, then check the cell directly above each dummy's
// top. That cell must contain edge content; it would be a space if the
// bug came back.
func TestDummyChainNoGaps(t *testing.T) {
	leaf := &fakeNode{target: "leaf"}
	mid := &fakeNode{target: "mid", deps: []*fakeNode{leaf}}
	o1 := &fakeNode{target: "obstacle1", deps: []*fakeNode{mid}}
	o2 := &fakeNode{target: "obstacle2", deps: []*fakeNode{o1}}
	goal := &fakeNode{target: "goal", deps: []*fakeNode{mid, o2}}

	bg := mkBoxes([]*fakeNode{goal}, 10)
	opts := Options{UseUnicode: true}
	opts.defaults()
	for _, b := range bg.boxes {
		b.computeContent(func(string, string) Status { return StatusPending }, opts.UseUnicode)
	}
	assignRows(bg)
	splitLongEdges(bg)
	assignCols(bg)
	cv := newCanvas(bg, opts)
	cv.draw(bg, opts)
	res := &Result{cells: cv.cells, styles: cv.styles, w: cv.w, h: cv.h}
	out := res.Render(0, 0, res.w, res.h, false)
	t.Log("\n" + out)

	dummyCount := 0
	for _, b := range bg.boxes {
		if !b.dummy {
			continue
		}
		dummyCount++
		if b.y-1 < 0 {
			continue
		}
		ch := cv.cells[b.y-1][b.x]
		if ch == ' ' {
			t.Errorf("gap in chain directly above dummy at (col %d, row %d):\n%s",
				b.x, b.y, out)
		}
	}
	if dummyCount == 0 {
		t.Fatalf("no dummy nodes were inserted; topology may have changed:\n%s", out)
	}
}

func TestRenderASCII(t *testing.T) {
	c := &fakeNode{target: "c"}
	b := &fakeNode{target: "b", deps: []*fakeNode{c}}
	a := &fakeNode{target: "a", deps: []*fakeNode{b}}
	out := renderFake(mkBoxes([]*fakeNode{a}, 10), Options{UseUnicode: false})
	t.Log("\n" + out)
	if strings.ContainsAny(out, "│─┌┐└┘┬┴┼") {
		t.Fatalf("ascii mode leaked unicode chars:\n%s", out)
	}
}
