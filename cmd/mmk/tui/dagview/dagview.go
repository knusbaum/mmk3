// Package dagview renders the runtime DAG as a top-down boxes-and-arrows
// graph. Dependencies sit *above* their dependents so the build flow reads
// downward: leaves at the top, the requested target at the bottom.
//
// Pattern siblings (same verb + SourcePattern) are collapsed into a single
// box that aggregates the underlying statuses.
//
// The canvas is held as a parallel rune + style-id grid so the caller can
// render only the visible window with ANSI styling. That keeps the viewport
// in tui/ free of ANSI-aware string slicing.
package dagview

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/knusbaum/mmk3/cmd/mmk/runtime"
)

// Status mirrors the TUI status enum so callers don't have to reach into
// the tui package.
type Status int

const (
	StatusPending Status = iota
	StatusRunning
	StatusDone
	StatusSkipped
	StatusFailed
)

// StatusFn returns the live status for a (target, verb) pair.
type StatusFn func(target, verb string) Status

// Options controls rendering knobs. Zero-value gives sensible defaults via
// (*Options).defaults.
type Options struct {
	// UseUnicode picks Unicode box-drawing chars (true) over ASCII (+/-/|).
	UseUnicode bool
	// GroupThreshold is the minimum number of pattern-siblings (same verb +
	// SourcePattern) required to collapse them into one box. 0 → 10.
	GroupThreshold int
	// GroupMatrix opts into collapsing matrix combos that share a base
	// name (e.g. "[downstream @ x=1 y=a]" + "[downstream @ x=1 y=b]") into
	// one box labelled "[downstream @ x= y=] x N". Off by default — the
	// per-combo view is more informative for small matrixes; turn this on
	// for fan-outs that overwhelm the screen.
	GroupMatrix bool
	// HPad is the horizontal gap (in cells) between adjacent boxes in the
	// same row. 0 → 3.
	HPad int
	// VPad is the vertical gap between rows. Must be ≥ 2 to leave room for
	// the edge corridor. 0 → 3.
	VPad int
}

func (o *Options) defaults() {
	if o.GroupThreshold <= 0 {
		o.GroupThreshold = 10
	}
	if o.HPad < 1 {
		o.HPad = 3
	}
	if o.VPad < 2 {
		o.VPad = 3
	}
}

// styleID identifies which lipgloss palette entry a cell belongs to.
type styleID uint8

const (
	stPlain    styleID = iota // default terminal color
	stPending                 // dim grey
	stRunning                 // bright yellow
	stDone                    // green
	stSkipped                 // cyan
	stFailed                  // red
	stHeader                  // bright blue (box title bar — unused for now, reserved)
)

// palette maps styleIDs to lipgloss styles. Lazily initialised because tests
// run before init order matters; cheap enough to compute at package load.
var palette = [...]lipgloss.Style{
	stPlain:   lipgloss.NewStyle(),
	stPending: lipgloss.NewStyle().Foreground(lipgloss.Color("240")),
	stRunning: lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Bold(true),
	stDone:    lipgloss.NewStyle().Foreground(lipgloss.Color("10")),
	stSkipped: lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Bold(true),
	stFailed:  lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true),
	stHeader:  lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true),
}

// Result is a rendered, styled DAG. Held as plain-rune + style-id grids so
// the caller can ask for a windowed slice without ANSI parsing.
type Result struct {
	cells  [][]rune
	styles [][]styleID
	w, h   int
}

// W returns the full drawing width in cells.
func (r *Result) W() int {
	if r == nil {
		return 0
	}
	return r.w
}

// H returns the full drawing height in rows.
func (r *Result) H() int {
	if r == nil {
		return 0
	}
	return r.h
}

// Render returns the rectangle (x..x+w, y..y+h) of the drawing as a string.
// If useColor is true, ANSI styling from the palette is applied; otherwise
// the output is plain text. Out-of-range rows pad with blank lines so the
// caller's layout stays stable.
func (r *Result) Render(x, y, w, h int, useColor bool) string {
	if r == nil || w <= 0 || h <= 0 {
		return ""
	}
	var sb strings.Builder
	for row := y; row < y+h; row++ {
		if row < 0 || row >= r.h {
			sb.WriteByte('\n')
			continue
		}
		r.renderRow(&sb, row, x, w, useColor)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// renderRow emits one row segment of width w starting at column x. When
// useColor is true, contiguous same-style runs are wrapped in one
// lipgloss.Render call so we don't pay the ANSI overhead per rune. Out-of-
// range columns become spaces. Trailing whitespace is trimmed only on the
// plain-text path; styled output keeps full width so callers can rely on a
// rectangular result.
func (r *Result) renderRow(sb *strings.Builder, row, x, w int, useColor bool) {
	cells := r.cells[row]
	styles := r.styles[row]
	cellAt := func(col int) (rune, styleID) {
		if col < 0 || col >= r.w {
			return ' ', stPlain
		}
		return cells[col], styles[col]
	}

	if !useColor {
		end := x + w
		var line strings.Builder
		for col := x; col < end; col++ {
			ch, _ := cellAt(col)
			line.WriteRune(ch)
		}
		sb.WriteString(strings.TrimRight(line.String(), " "))
		return
	}

	// Run-length grouping: walk the segment, gather consecutive same-style
	// cells, then emit each run with one Render() call.
	col := x
	end := x + w
	for col < end {
		_, st := cellAt(col)
		runStart := col
		var run strings.Builder
		for col < end {
			ch, st2 := cellAt(col)
			if st2 != st {
				break
			}
			run.WriteRune(ch)
			col++
		}
		_ = runStart
		text := run.String()
		if st == stPlain {
			sb.WriteString(text)
		} else {
			sb.WriteString(palette[st].Render(text))
		}
	}
}

// View builds a styled DAG layout for the graph rooted at root. The result
// is cheap to re-render at different viewports.
func View(root *runtime.TargetNode, statusOf StatusFn, opts Options) *Result {
	opts.defaults()
	if statusOf == nil {
		statusOf = func(string, string) Status { return StatusPending }
	}
	if root == nil {
		return &Result{}
	}
	g := buildBoxGraph(root, opts)
	if len(g.boxes) == 0 {
		return &Result{}
	}
	for _, b := range g.boxes {
		b.computeContent(statusOf, opts.UseUnicode)
	}
	assignRows(g)
	splitLongEdges(g)
	assignCols(g)
	c := newCanvas(g, opts)
	c.draw(g, opts)
	return &Result{cells: c.cells, styles: c.styles, w: c.w, h: c.h}
}

// --- Box graph (collapsed runtime DAG) -------------------------------------

type box struct {
	id      string
	title   string
	isGroup bool
	// dummy is true for synthetic single-cell boxes inserted by
	// splitLongEdges. They take a column slot in their row so the
	// crossing-reduction sweep gives them a position; drawing renders
	// them as a continuation of the edge instead of a visible box.
	dummy bool
	keys  []string

	// Aggregate status. Computed by computeContent; drives border and
	// content cell colors.
	style styleID

	// Per-content-line styles, one entry per character per line. Filled in
	// by computeContent so groups can render each "v3 *2" segment in its
	// matching color.
	contentLines  []string
	contentStyles [][]styleID
	w, h          int

	// Layout outputs.
	row, col int
	x, y     int

	// deps = boxes this depends on. After the orientation flip, deps render
	// ABOVE this box (lower y), and parents render below.
	deps    []*box
	parents []*box
}

type boxGraph struct {
	root   *box
	boxes  []*box
	rows   map[int][]*box
	maxRow int
}

func buildBoxGraph(root *runtime.TargetNode, opts Options) *boxGraph {
	type rtKey struct{ target, verb string }
	visited := map[rtKey]bool{}
	var nodes []*runtime.TargetNode

	var walk func(n *runtime.TargetNode)
	walk = func(n *runtime.TargetNode) {
		k := rtKey{n.Target(), n.Verb()}
		if visited[k] {
			return
		}
		visited[k] = true
		nodes = append(nodes, n)
		for _, d := range n.DisplayDeps() {
			walk(d)
		}
	}
	walk(root)

	// kindPattern (auto-grouped at threshold), kindMatrix (grouped only
	// when opts.GroupMatrix is true), or kindSingle.
	const (
		kindSingle = iota
		kindPattern
		kindMatrix
	)
	type groupInfo struct {
		key  string
		kind int
		base string // matrix base name; empty for non-matrix
	}
	groupInfoOf := func(n *runtime.TargetNode) groupInfo {
		if p := n.SourcePattern(); p != "" {
			return groupInfo{
				key:  "pat\x00" + n.Verb() + "\x00" + p,
				kind: kindPattern,
			}
		}
		if opts.GroupMatrix {
			if base := matrixBase(n.Target()); base != "" {
				return groupInfo{
					key:  "mat\x00" + n.Verb() + "\x00" + base,
					kind: kindMatrix,
					base: base,
				}
			}
		}
		return groupInfo{
			key:  "one\x00" + n.Verb() + "\x00" + n.Target(),
			kind: kindSingle,
		}
	}

	// Count members per group so we can decide whether to actually fold.
	// Patterns need ≥ GroupThreshold; matrix groups need ≥ 2 (lone combos
	// stay single — collapsing 1 into 1 hides info for nothing).
	counts := map[string]int{}
	for _, n := range nodes {
		gi := groupInfoOf(n)
		if gi.kind != kindSingle {
			counts[gi.key]++
		}
	}

	// Per-matrix-group: capture dim names from the first member so the
	// label can read "[base @ x= y=]". Order is whatever the target name
	// was sorted into.
	matrixDims := map[string][]string{}
	for _, n := range nodes {
		gi := groupInfoOf(n)
		if gi.kind != kindMatrix {
			continue
		}
		if _, ok := matrixDims[gi.key]; ok {
			continue
		}
		matrixDims[gi.key] = matrixDimNames(n.Target())
	}

	nodeToBox := map[rtKey]*box{}
	boxByID := map[string]*box{}

	for _, n := range nodes {
		gi := groupInfoOf(n)
		shouldGroup := false
		switch gi.kind {
		case kindPattern:
			shouldGroup = counts[gi.key] >= opts.GroupThreshold
		case kindMatrix:
			shouldGroup = counts[gi.key] >= 2
		}

		var id string
		if shouldGroup {
			id = gi.key
		} else {
			id = "one\x00" + n.Verb() + "\x00" + n.Target()
		}

		if b, ok := boxByID[id]; ok {
			b.keys = append(b.keys, nodeKey(n.Target(), n.Verb()))
			nodeToBox[rtKey{n.Target(), n.Verb()}] = b
			continue
		}

		b := &box{
			id:   id,
			keys: []string{nodeKey(n.Target(), n.Verb())},
		}
		if shouldGroup {
			b.isGroup = true
			count := counts[gi.key]
			switch gi.kind {
			case kindPattern:
				pat := n.SourcePattern()
				if v := n.Verb(); v != "" {
					b.title = fmt.Sprintf("[%s '%s'] x %d", v, pat, count)
				} else {
					b.title = fmt.Sprintf("'%s' x %d", pat, count)
				}
			case kindMatrix:
				b.title = matrixGroupTitle(n.Verb(), gi.base, matrixDims[gi.key], count)
			}
		} else {
			b.title = nodeLabel(n)
		}
		boxByID[id] = b
		nodeToBox[rtKey{n.Target(), n.Verb()}] = b
	}

	type edgeKey struct{ from, to *box }
	seenEdge := map[edgeKey]bool{}
	for _, n := range nodes {
		from := nodeToBox[rtKey{n.Target(), n.Verb()}]
		for _, d := range n.DisplayDeps() {
			to := nodeToBox[rtKey{d.Target(), d.Verb()}]
			if from == to {
				continue
			}
			ek := edgeKey{from, to}
			if seenEdge[ek] {
				continue
			}
			seenEdge[ek] = true
			from.deps = append(from.deps, to)
			to.parents = append(to.parents, from)
		}
	}

	bg := &boxGraph{
		root:  nodeToBox[rtKey{root.Target(), root.Verb()}],
		boxes: make([]*box, 0, len(boxByID)),
	}
	seen := map[*box]bool{}
	for _, n := range nodes {
		b := nodeToBox[rtKey{n.Target(), n.Verb()}]
		if !seen[b] {
			seen[b] = true
			bg.boxes = append(bg.boxes, b)
		}
	}
	return bg
}

// computeContent fills contentLines + contentStyles + w/h based on the
// aggregated status of this box's members.
func (b *box) computeContent(statusOf StatusFn, useUnicode bool) {
	var pending, running, done, skipped, failed int
	for _, k := range b.keys {
		t, v := splitKey(k)
		switch statusOf(t, v) {
		case StatusRunning:
			running++
		case StatusDone:
			done++
		case StatusSkipped:
			skipped++
		case StatusFailed:
			failed++
		default:
			pending++
		}
	}

	// Aggregate status: failed > running > pending > skipped/done.
	switch {
	case failed > 0:
		b.style = stFailed
	case running > 0:
		b.style = stRunning
	case pending > 0:
		b.style = stPending
	case done > 0:
		b.style = stDone
	case skipped > 0:
		b.style = stSkipped
	default:
		b.style = stPending
	}

	if b.isGroup {
		// Title line: aggregate-style.
		b.contentLines = []string{b.title}
		b.contentStyles = [][]styleID{repeatStyle(b.style, runeCount(b.title))}

		// Counts line: each part in its own color.
		var line strings.Builder
		var styles []styleID
		add := func(s string, st styleID) {
			if line.Len() > 0 {
				line.WriteByte(' ')
				styles = append(styles, stPlain)
			}
			for range s {
				styles = append(styles, st)
			}
			line.WriteString(s)
		}
		if done > 0 {
			add(fmt.Sprintf("%s%d", iconDone(useUnicode), done), stDone)
		}
		if running > 0 {
			add(fmt.Sprintf("%s%d", iconRunning(useUnicode), running), stRunning)
		}
		if failed > 0 {
			add(fmt.Sprintf("%s%d", iconFailed(useUnicode), failed), stFailed)
		}
		if skipped > 0 {
			add(fmt.Sprintf("%s%d", iconSkipped(useUnicode), skipped), stSkipped)
		}
		if pending > 0 {
			add(fmt.Sprintf("%s%d", iconPending(useUnicode), pending), stPending)
		}
		if line.Len() > 0 {
			b.contentLines = append(b.contentLines, line.String())
			b.contentStyles = append(b.contentStyles, styles)
		}
	} else {
		// Single box: icon + label, both in aggregate status colour.
		var icon string
		switch b.style {
		case stFailed:
			icon = iconFailed(useUnicode)
		case stRunning:
			icon = iconRunning(useUnicode)
		case stSkipped:
			icon = iconSkipped(useUnicode)
		case stDone:
			icon = iconDone(useUnicode)
		default:
			icon = iconPending(useUnicode)
		}
		line := icon + " " + b.title
		b.contentLines = []string{line}
		b.contentStyles = [][]styleID{repeatStyle(b.style, runeCount(line))}
	}

	w := 0
	for _, ln := range b.contentLines {
		if l := runeCount(ln); l > w {
			w = l
		}
	}
	b.w = w + 4
	b.h = len(b.contentLines) + 2
}

func repeatStyle(s styleID, n int) []styleID {
	out := make([]styleID, n)
	for i := range out {
		out[i] = s
	}
	return out
}

// --- Layout: row + column assignment (deps above, dependents below) --------

// assignRows lays the graph out so leaves (boxes with no deps) sit at row 0
// and a box's row is one greater than the deepest of its deps. The build
// flow then reads downward in the rendered output.
func assignRows(g *boxGraph) {
	indeg := map[*box]int{}
	for _, b := range g.boxes {
		indeg[b] = len(b.deps)
		b.row = 0
	}
	var queue []*box
	for _, b := range g.boxes {
		if indeg[b] == 0 {
			queue = append(queue, b)
		}
	}
	for len(queue) > 0 {
		b := queue[0]
		queue = queue[1:]
		for _, parent := range b.parents {
			if b.row+1 > parent.row {
				parent.row = b.row + 1
			}
			indeg[parent]--
			if indeg[parent] == 0 {
				queue = append(queue, parent)
			}
		}
	}
}

// splitLongEdges replaces each edge that spans more than one layer with a
// chain of single-cell dummy boxes, one per intermediate row. After this
// pass every remaining edge connects boxes in adjacent rows, so the layered
// renderer can route within one corridor instead of dragging a vertical
// line across every row in between (which used to cut through any real box
// that happened to share a column with the source/target).
func splitLongEdges(g *boxGraph) {
	var newDummies []*box
	for _, b := range g.boxes {
		if b.dummy {
			continue
		}
		original := append([]*box{}, b.deps...)
		var keep []*box
		for _, dep := range original {
			if b.row-dep.row <= 1 {
				keep = append(keep, dep)
				continue
			}
			// Sever direct dep→b edge.
			for i, p := range dep.parents {
				if p == b {
					dep.parents = append(dep.parents[:i], dep.parents[i+1:]...)
					break
				}
			}
			// Build the chain dep → d_dep.row+1 → … → d_{b.row-1} → b.
			prev := dep
			for r := dep.row + 1; r < b.row; r++ {
				d := &box{
					id:    fmt.Sprintf("dummy\x00%d\x00%s\x00%s", r, dep.id, b.id),
					dummy: true,
					row:   r,
					w:     1,
					h:     1,
					deps:  []*box{prev},
				}
				prev.parents = append(prev.parents, d)
				newDummies = append(newDummies, d)
				prev = d
			}
			keep = append(keep, prev)
			prev.parents = append(prev.parents, b)
		}
		b.deps = keep
	}
	g.boxes = append(g.boxes, newDummies...)
}

// assignCols orders boxes within each row to reduce edge crossings via a
// few alternating median-heuristic sweeps. After the orientation flip,
// row r-1 holds your deps (visually above) and row r+1 your dependents
// (visually below).
func assignCols(g *boxGraph) {
	rows := map[int][]*box{}
	maxRow := 0
	for _, b := range g.boxes {
		rows[b.row] = append(rows[b.row], b)
		if b.row > maxRow {
			maxRow = b.row
		}
	}
	for r := range rows {
		sort.Slice(rows[r], func(i, j int) bool {
			return rows[r][i].id < rows[r][j].id
		})
		for i, b := range rows[r] {
			b.col = i
		}
	}

	for sweep := range 4 {
		if sweep%2 == 0 {
			// Top-down sweep: align row r under its deps in row r-1.
			for r := 1; r <= maxRow; r++ {
				sortByMedian(rows[r], func(b *box) []int {
					var cs []int
					for _, d := range b.deps {
						if d.row == r-1 {
							cs = append(cs, d.col)
						}
					}
					return cs
				})
				for i, b := range rows[r] {
					b.col = i
				}
			}
		} else {
			// Bottom-up sweep: align row r over its dependents in row r+1.
			for r := maxRow - 1; r >= 0; r-- {
				sortByMedian(rows[r], func(b *box) []int {
					var cs []int
					for _, p := range b.parents {
						if p.row == r+1 {
							cs = append(cs, p.col)
						}
					}
					return cs
				})
				for i, b := range rows[r] {
					b.col = i
				}
			}
		}
	}

	g.rows = rows
	g.maxRow = maxRow
}

func sortByMedian(row []*box, neigh func(*box) []int) {
	type sk struct {
		b      *box
		median float64
	}
	keys := make([]sk, len(row))
	for i, b := range row {
		ns := neigh(b)
		if len(ns) == 0 {
			keys[i] = sk{b, float64(b.col)}
			continue
		}
		sort.Ints(ns)
		var m float64
		if len(ns)%2 == 1 {
			m = float64(ns[len(ns)/2])
		} else {
			m = float64(ns[len(ns)/2-1]+ns[len(ns)/2]) / 2
		}
		keys[i] = sk{b, m}
	}
	sort.SliceStable(keys, func(i, j int) bool {
		return keys[i].median < keys[j].median
	})
	for i, k := range keys {
		row[i] = k.b
	}
}

// --- Canvas + drawing ------------------------------------------------------

type canvas struct {
	cells  [][]rune
	styles [][]styleID
	w, h   int
}

func newCanvas(g *boxGraph, opts Options) *canvas {
	rowTotalW := make([]int, g.maxRow+1)
	rowMaxH := make([]int, g.maxRow+1)
	for r := 0; r <= g.maxRow; r++ {
		boxes := g.rows[r]
		totalW := 0
		maxH := 0
		for i, b := range boxes {
			if i > 0 {
				totalW += opts.HPad
			}
			totalW += b.w
			if b.h > maxH {
				maxH = b.h
			}
		}
		rowTotalW[r] = totalW
		rowMaxH[r] = maxH
	}
	canvasW := 0
	for _, w := range rowTotalW {
		if w > canvasW {
			canvasW = w
		}
	}

	// Stretch dummies to fill the row height. Otherwise their edge
	// attachments (top / bottom) sit at row-top while regular boxes' sit
	// at row-bottom, leaving gaps in the rendered chain.
	for r := 0; r <= g.maxRow; r++ {
		for _, b := range g.rows[r] {
			if b.dummy {
				b.h = rowMaxH[r]
			}
		}
	}

	yCursor := 0
	for r := 0; r <= g.maxRow; r++ {
		xCursor := max((canvasW-rowTotalW[r])/2, 0)
		for _, b := range g.rows[r] {
			b.x = xCursor
			b.y = yCursor
			xCursor += b.w + opts.HPad
		}
		yCursor += rowMaxH[r] + opts.VPad
	}
	canvasH := yCursor - opts.VPad

	c := &canvas{w: canvasW, h: canvasH}
	c.cells = make([][]rune, c.h)
	c.styles = make([][]styleID, c.h)
	for i := range c.cells {
		c.cells[i] = make([]rune, c.w)
		c.styles[i] = make([]styleID, c.w)
		for j := range c.cells[i] {
			c.cells[i][j] = ' '
		}
	}
	return c
}

func (c *canvas) put(x, y int, r rune, st styleID) {
	if y < 0 || y >= c.h || x < 0 || x >= c.w {
		return
	}
	c.cells[y][x] = r
	c.styles[y][x] = st
}

func (c *canvas) get(x, y int) rune {
	if y < 0 || y >= c.h || x < 0 || x >= c.w {
		return ' '
	}
	return c.cells[y][x]
}

// putEdge writes a line/junction char, merging with whatever's already
// there so two crossing edges produce a `+`/`┼` instead of clobbering.
func (c *canvas) putEdge(x, y int, r rune, ascii bool) {
	cur := c.get(x, y)
	c.put(x, y, mergeJunction(cur, r, ascii), stPlain)
}

// --- Box + edge drawing ----------------------------------------------------

type chars struct {
	tl, tr, bl, br, h, v rune
	tDown, tUp           rune
	arrow                rune
}

var unicodeChars = chars{
	tl: '┌', tr: '┐', bl: '└', br: '┘',
	h: '─', v: '│',
	tDown: '┬', tUp: '┴',
	arrow: 'v',
}

var asciiChars = chars{
	tl: '+', tr: '+', bl: '+', br: '+',
	h: '-', v: '|',
	tDown: '+', tUp: '+',
	arrow: 'v',
}

func (c *canvas) draw(g *boxGraph, opts Options) {
	ch := unicodeChars
	if !opts.UseUnicode {
		ch = asciiChars
	}
	for _, b := range g.boxes {
		if b.dummy {
			// Draw the dummy column as a continuous vertical line so the
			// edges flowing in (top) and out (bottom) merge into one
			// straight-through path.
			for y := b.y; y < b.y+b.h; y++ {
				c.putEdge(b.x, y, ch.v, !opts.UseUnicode)
			}
			continue
		}
		c.drawBox(b, ch)
	}
	// Draw edges from dep (above) → dependent (below). Source is dep,
	// target is dependent; the arrow head points down.
	for _, b := range g.boxes {
		for _, dep := range b.deps {
			c.drawEdge(dep, b, ch, !opts.UseUnicode)
		}
	}
}

func (c *canvas) drawBox(b *box, ch chars) {
	x0, y0 := b.x, b.y
	x1, y1 := b.x+b.w-1, b.y+b.h-1
	c.put(x0, y0, ch.tl, b.style)
	c.put(x1, y0, ch.tr, b.style)
	c.put(x0, y1, ch.bl, b.style)
	c.put(x1, y1, ch.br, b.style)
	for x := x0 + 1; x < x1; x++ {
		c.put(x, y0, ch.h, b.style)
		c.put(x, y1, ch.h, b.style)
	}
	for y := y0 + 1; y < y1; y++ {
		c.put(x0, y, ch.v, b.style)
		c.put(x1, y, ch.v, b.style)
	}
	for i, line := range b.contentLines {
		col := x0 + 2
		stylesRow := b.contentStyles[i]
		k := 0
		for _, r := range line {
			st := stPlain
			if k < len(stylesRow) {
				st = stylesRow[k]
			}
			c.put(col, y0+1+i, r, st)
			col++
			k++
		}
	}
}

// drawEdge routes from-bottom → corridor row → into-top. `from` must sit
// above `to` in canvas coordinates. Dummy endpoints are drawn as plain
// continuation lines (no ┬/┴/arrow glyphs) so a chain of dep → d1 → … → dn
// → child reads as one continuous edge.
func (c *canvas) drawEdge(from, to *box, ch chars, ascii bool) {
	sx := from.x + from.w/2
	sy := from.y + from.h - 1
	tx := to.x + to.w/2
	ty := to.y

	if from.dummy {
		c.putEdge(sx, sy, ch.v, ascii)
	} else {
		c.put(sx, sy, ch.tDown, from.style)
	}

	corridor := sy + 1
	if ty-1 > corridor {
		corridor = (sy + ty) / 2
	}

	for y := sy + 1; y < corridor; y++ {
		c.putEdge(sx, y, ch.v, ascii)
	}

	if sx == tx {
		c.putEdge(sx, corridor, ch.v, ascii)
	} else {
		var leftCorner, rightCorner rune
		if sx < tx {
			leftCorner = ifAscii(ascii, '+', '└')
			rightCorner = ifAscii(ascii, '+', '┐')
		} else {
			leftCorner = ifAscii(ascii, '+', '┘')
			rightCorner = ifAscii(ascii, '+', '┌')
		}
		c.putEdge(sx, corridor, leftCorner, ascii)
		c.putEdge(tx, corridor, rightCorner, ascii)
		x0, x1 := sx, tx
		if x0 > x1 {
			x0, x1 = x1, x0
		}
		for x := x0 + 1; x < x1; x++ {
			c.putEdge(x, corridor, ch.h, ascii)
		}
	}

	for y := corridor + 1; y < ty-1; y++ {
		c.putEdge(tx, y, ch.v, ascii)
	}

	if to.dummy {
		// Dummy targets don't get an arrowhead; that row needs a `│`
		// instead, otherwise the chain shows a one-cell gap right above
		// each dummy.
		c.putEdge(tx, ty-1, ch.v, ascii)
		c.putEdge(tx, ty, ch.v, ascii)
		return
	}
	if ty-1 > corridor {
		c.put(tx, ty-1, ch.arrow, stPlain)
	}
	c.put(tx, ty, ch.tUp, to.style)
}

func ifAscii(ascii bool, a, u rune) rune {
	if ascii {
		return a
	}
	return u
}

// --- Junction merging ------------------------------------------------------

const (
	mN = 1 << iota
	mE
	mS
	mW
)

func edgeMask(r rune) int {
	switch r {
	case '│', '|':
		return mN | mS
	case '─', '-':
		return mE | mW
	case '┌':
		return mE | mS
	case '┐':
		return mW | mS
	case '└':
		return mN | mE
	case '┘':
		return mN | mW
	case '├':
		return mN | mE | mS
	case '┤':
		return mN | mW | mS
	case '┬':
		return mE | mS | mW
	case '┴':
		return mN | mE | mW
	case '┼', '+':
		return mN | mE | mS | mW
	}
	return 0
}

func maskToRune(mask int, ascii bool) rune {
	if ascii {
		switch mask {
		case mN | mS:
			return '|'
		case mE | mW:
			return '-'
		case 0:
			return ' '
		}
		return '+'
	}
	switch mask {
	case mN | mS:
		return '│'
	case mE | mW:
		return '─'
	case mE | mS:
		return '┌'
	case mW | mS:
		return '┐'
	case mN | mE:
		return '└'
	case mN | mW:
		return '┘'
	case mN | mE | mS:
		return '├'
	case mN | mW | mS:
		return '┤'
	case mE | mS | mW:
		return '┬'
	case mN | mE | mW:
		return '┴'
	case mN | mE | mS | mW:
		return '┼'
	}
	return ' '
}

func mergeJunction(cur, incoming rune, ascii bool) rune {
	if cur == ' ' {
		return incoming
	}
	cm := edgeMask(cur)
	im := edgeMask(incoming)
	if cm == 0 || im == 0 {
		return incoming
	}
	merged := cm | im
	if r := maskToRune(merged, ascii); r != ' ' {
		return r
	}
	return incoming
}

// --- Icons -----------------------------------------------------------------

func iconPending(unicode bool) string {
	if unicode {
		return "○"
	}
	return "."
}
func iconRunning(unicode bool) string {
	if unicode {
		return "●"
	}
	return "*"
}
func iconDone(unicode bool) string {
	if unicode {
		return "✓"
	}
	return "v"
}
func iconSkipped(unicode bool) string {
	if unicode {
		return "≡"
	}
	return "="
}
func iconFailed(unicode bool) string {
	if unicode {
		return "✗"
	}
	return "x"
}

// --- Helpers ---------------------------------------------------------------

func nodeKey(target, verb string) string { return verb + "\x00" + target }

func splitKey(k string) (target, verb string) {
	i := strings.IndexByte(k, '\x00')
	if i < 0 {
		return k, ""
	}
	return k[i+1:], k[:i]
}

// matrixBase parses a target like "[downstream @ x=1 y=a]" and returns
// "downstream". Returns "" for any name that doesn't match the combo
// format produced by runtime.comboTargetName. The matching is conservative
// (`[...]` plus a literal " @ ") so non-combo names with brackets in them
// don't get accidentally grouped.
func matrixBase(target string) string {
	if len(target) < 2 || target[0] != '[' || target[len(target)-1] != ']' {
		return ""
	}
	inner := target[1 : len(target)-1]
	i := strings.Index(inner, " @ ")
	if i <= 0 {
		return ""
	}
	return inner[:i]
}

// matrixDimNames extracts the dim keys (e.g. ["x", "y"]) from a combo
// target name. Returns nil if the format doesn't match.
func matrixDimNames(target string) []string {
	if len(target) < 2 || target[0] != '[' || target[len(target)-1] != ']' {
		return nil
	}
	inner := target[1 : len(target)-1]
	i := strings.Index(inner, " @ ")
	if i < 0 {
		return nil
	}
	var names []string
	for kv := range strings.FieldsSeq(inner[i+3:]) {
		eq := strings.IndexByte(kv, '=')
		if eq > 0 {
			names = append(names, kv[:eq])
		}
	}
	return names
}

// matrixGroupTitle formats the label for a matrix-collapsed box. The dim
// values are intentionally blank to communicate "all combinations folded
// into this box".
func matrixGroupTitle(verb, base string, dims []string, count int) string {
	var sb strings.Builder
	sb.WriteByte('[')
	if verb != "" {
		sb.WriteString(verb)
		sb.WriteByte(' ')
	}
	sb.WriteString(base)
	if len(dims) > 0 {
		sb.WriteString(" @ ")
		for i, d := range dims {
			if i > 0 {
				sb.WriteByte(' ')
			}
			sb.WriteString(d)
			sb.WriteByte('=')
		}
	}
	sb.WriteString("] x ")
	sb.WriteString(fmt.Sprintf("%d", count))
	return sb.String()
}

func nodeLabel(n *runtime.TargetNode) string {
	if n.Verb() != "" {
		return "[" + n.Verb() + " " + n.Target() + "]"
	}
	return n.Target()
}

func runeCount(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}
