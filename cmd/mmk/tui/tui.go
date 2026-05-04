// Package tui renders mmk's build progress as a live tree + scrolling log.
//
// Design (experimental):
//   - The tree is computed once at start (mirrors `mmk -graph`'s layout) and
//     re-rendered each tick with per-node status icons.
//   - Each node's body stdout/stderr is teed to (a) a per-node capture buffer
//     for failure replay, and (b) a global ring of recent lines for the log
//     panel at the bottom.
//   - On exit: leave the final tree on screen. If anything failed, dump the
//     first failure's full captured output below; list remaining failures by
//     name with a "rerun for details" hint.
package tui

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/knusbaum/mmk3/cmd/mmk/runtime"
	"github.com/knusbaum/mmk3/cmd/mmk/tui/dagview"
	"github.com/knusbaum/mmk3/dag"
)

// Run resolves target+verb, builds the dep graph, and runs it under a
// bubbletea TUI. Returns when the build finishes; the returned error matches
// what Build.Execute would have returned.
func Run(b *runtime.Build, target, verb string, parallelism int) error {
	root, err := resolve(b, target, verb)
	if err != nil {
		return err
	}
	tree := buildTree(root)

	caps := &captures{m: map[string]*bytes.Buffer{}}
	ring := newLogRing(2000)
	b.OutputWriter = func(target, verb string) (io.Writer, io.Writer) {
		w := &lineWriter{
			key:     nodeKey(target, verb),
			caps:    caps,
			ring:    ring,
			lineBuf: &bytes.Buffer{},
		}
		return w, w
	}

	m := initialModel(tree, ring, b)
	prog := tea.NewProgram(m, tea.WithAltScreen())

	// Drive the build from a goroutine. Hooks publish events to the program.
	hooks := dag.Hooks[*runtime.TargetNode]{
		OnRun:  func(n *runtime.TargetNode) { prog.Send(runMsg{key: nodeKey(n.Target(), n.Verb())}) },
		OnSkip: func(n *runtime.TargetNode) { prog.Send(skipMsg{key: nodeKey(n.Target(), n.Verb())}) },
		OnFinish: func(n *runtime.TargetNode, err error) {
			out := caps.take(nodeKey(n.Target(), n.Verb()))
			prog.Send(finishMsg{key: nodeKey(n.Target(), n.Verb()), target: n.Target(), verb: n.Verb(), err: err, output: out})
		},
	}

	go func() {
		err := dag.Execute(root, parallelism, hooks)
		prog.Send(buildDoneMsg{err: err})
	}()

	finalModel, runErr := prog.Run()
	if runErr != nil {
		return runErr
	}
	if fm, ok := finalModel.(model); ok {
		// Bubbletea has exited altscreen — the live render is gone. Print the
		// final state to the regular terminal so the user can see it in
		// scrollback.
		fmt.Print(fm.FinalView())
		if fm.buildErr != nil {
			return fm.buildErr
		}
	}
	return nil
}

func resolve(b *runtime.Build, target, verb string) (*runtime.TargetNode, error) {
	if verb == "" {
		return b.Resolve(target)
	}
	return b.ResolveVerb(target, verb)
}

// --- node identity ---

func nodeKey(target, verb string) string { return verb + "\x00" + target }

// --- per-node output capture ---

type captures struct {
	mu sync.Mutex
	m  map[string]*bytes.Buffer
}

func (c *captures) buf(key string) *bytes.Buffer {
	c.mu.Lock()
	defer c.mu.Unlock()
	if b, ok := c.m[key]; ok {
		return b
	}
	b := &bytes.Buffer{}
	c.m[key] = b
	return b
}

func (c *captures) take(key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if b, ok := c.m[key]; ok {
		return b.String()
	}
	return ""
}

// logRing is a thread-safe bounded list of recent log lines. The TUI reads
// the tail at render time; producers write at body-execution speed. We avoid
// shipping each line through bubbletea's message queue because high-volume
// output (e.g. `set -x` + parallel jobs) triggers thousands of redraws per
// second, which causes flicker.
type logRing struct {
	mu     sync.Mutex
	lines  []string
	maxLen int
}

func newLogRing(max int) *logRing { return &logRing{maxLen: max} }

func (r *logRing) push(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	if len(r.lines) > r.maxLen {
		r.lines = r.lines[len(r.lines)-r.maxLen:]
	}
}

func (r *logRing) tail(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.lines) <= n {
		out := make([]string, len(r.lines))
		copy(out, r.lines)
		return out
	}
	out := make([]string, n)
	copy(out, r.lines[len(r.lines)-n:])
	return out
}

// lineWriter captures bytes for failure replay and pushes complete lines
// into the shared log ring.
type lineWriter struct {
	key     string
	caps    *captures
	ring    *logRing
	lineBuf *bytes.Buffer
}

func (lw *lineWriter) Write(p []byte) (int, error) {
	lw.caps.buf(lw.key).Write(p)
	lw.lineBuf.Write(p)
	for {
		i := bytes.IndexByte(lw.lineBuf.Bytes(), '\n')
		if i < 0 {
			break
		}
		line := string(bytes.TrimRight(lw.lineBuf.Bytes()[:i], "\r"))
		lw.lineBuf.Next(i + 1)
		lw.ring.push(line)
	}
	return len(p), nil
}

// --- tree precomputed at start ---

type treeLine struct {
	prefix string // ASCII tree prefix to print before the icon
	label  string // node label; for groups, the pattern source for display
	key    string // nodeKey for status lookup; empty for groups

	// When non-empty, this line represents a collapsed group of nodes that
	// all came from the same pattern rule. The View aggregates statuses by
	// looking these up in m.statuses and rendering counts.
	groupKeys []string
}

// collapseThreshold is the minimum group size that triggers collapsing a set
// of pattern-instantiated siblings into a single line.
const collapseThreshold = 10

type treeData struct {
	rootLabel string
	rootKey   string
	lines     []treeLine
	rootNode  *runtime.TargetNode
}

func buildTree(root *runtime.TargetNode) treeData {
	t := treeData{
		rootLabel: nodeLabel(root),
		rootKey:   nodeKey(root.Target(), root.Verb()),
		rootNode:  root,
	}
	visited := map[string]bool{t.rootKey: true}
	emitChildren(&t, root.DisplayDeps(), "", visited)
	return t
}

// emitChildren renders the deps of a node, collapsing any group of >10
// pattern-instantiated siblings (sharing pattern + verb) into a single line.
func emitChildren(t *treeData, children []*runtime.TargetNode, prefix string, visited map[string]bool) {
	items := groupChildren(children)
	for i, it := range items {
		isLast := i == len(items)-1
		if it.isGroup {
			emitGroup(t, it, prefix, isLast)
			continue
		}
		appendChild(t, it.node, prefix, isLast, visited)
	}
}

type childItem struct {
	node    *runtime.TargetNode // individual line if non-nil
	isGroup bool
	pattern string
	verb    string
	members []*runtime.TargetNode // group members
}

// groupChildren returns the display order of `children`: individual nodes
// where they come, and one entry per group of >threshold pattern-siblings
// (placed at the position of the first sibling). Mixed pattern + concrete
// siblings preserve their original order; the group collapses in place.
func groupChildren(children []*runtime.TargetNode) []childItem {
	counts := map[string]int{}
	for _, c := range children {
		if p := c.SourcePattern(); p != "" {
			counts[c.Verb()+"\x00"+p]++
		}
	}
	rendered := map[string]bool{}
	var out []childItem
	for _, c := range children {
		p := c.SourcePattern()
		if p == "" || counts[c.Verb()+"\x00"+p] <= collapseThreshold {
			out = append(out, childItem{node: c})
			continue
		}
		key := c.Verb() + "\x00" + p
		if rendered[key] {
			continue
		}
		rendered[key] = true
		var members []*runtime.TargetNode
		for _, c2 := range children {
			if c2.SourcePattern() == p && c2.Verb() == c.Verb() {
				members = append(members, c2)
			}
		}
		out = append(out, childItem{
			isGroup: true,
			pattern: p,
			verb:    c.Verb(),
			members: members,
		})
	}
	return out
}

func emitGroup(t *treeData, it childItem, prefix string, isLast bool) {
	connector := "├── "
	if isLast {
		connector = "└── "
	}
	keys := make([]string, len(it.members))
	for i, m := range it.members {
		keys[i] = nodeKey(m.Target(), m.Verb())
	}
	label := fmt.Sprintf("'%s' × %d", it.pattern, len(it.members))
	if it.verb != "" {
		label = fmt.Sprintf("[%s '%s'] × %d", it.verb, it.pattern, len(it.members))
	}
	t.lines = append(t.lines, treeLine{
		prefix:    prefix + connector,
		label:     label,
		groupKeys: keys,
	})
}

func appendChild(t *treeData, n *runtime.TargetNode, prefix string, isLast bool, visited map[string]bool) {
	connector := "├── "
	childPrefix := prefix + "│   "
	if isLast {
		connector = "└── "
		childPrefix = prefix + "    "
	}
	key := nodeKey(n.Target(), n.Verb())
	already := visited[key]
	label := nodeLabel(n)
	if already {
		label += " (*)"
	}
	t.lines = append(t.lines, treeLine{
		prefix: prefix + connector,
		label:  label,
		key:    key,
	})
	if already {
		return
	}
	visited[key] = true
	emitChildren(t, n.DisplayDeps(), childPrefix, visited)
}

func nodeLabel(n *runtime.TargetNode) string {
	if n.Verb() != "" {
		return "[" + n.Verb() + " " + n.Target() + "]"
	}
	return n.Target()
}

// --- bubbletea model ---

type status int

const (
	statusPending status = iota
	statusRunning
	statusDone
	statusSkipped
	statusFailed
)

type failure struct {
	target string
	verb   string
	err    error
	output string
}

type model struct {
	tree     treeData
	statuses map[string]status
	ring     *logRing
	failures []failure
	build    *runtime.Build

	// cancelStage advances on each Ctrl+C while the build is running:
	//   0 normal, 1 graceful (Cancel), 2 SIGTERM in-flight, 3 SIGKILL in-flight.
	// After buildDone is true Ctrl+C just exits the TUI.
	cancelStage int

	width, height int
	buildErr      error
	buildDone     bool

	// graphView is the experimental top-down DAG view, toggled with 'g'.
	graphView bool
	// dagX, dagY are pan offsets (in cells) into the rendered DAG. Only
	// meaningful when graphView is true. Clamped against the cached
	// drawing's size whenever it changes.
	dagX, dagY int
	// dagResult caches the rendered DAG between ticks so panning doesn't
	// re-run layout, and so key handlers can clamp offsets against the
	// drawing size without recomputing it.
	dagResult  *dagview.Result
	dagW, dagH int
	// dagGroupMatrix flips matrix-base grouping in the DAG view (toggled
	// with 'm'). Off by default; turn on for huge matrix fan-outs.
	dagGroupMatrix bool
}

func initialModel(t treeData, ring *logRing, b *runtime.Build) model {
	m := model{
		tree:     t,
		statuses: map[string]status{},
		ring:     ring,
		build:    b,
		graphView: true,
		dagGroupMatrix: true,
	}
	m.statuses[t.rootKey] = statusPending
	for _, ln := range t.lines {
		if ln.key != "" {
			m.statuses[ln.key] = statusPending
		}
		for _, gk := range ln.groupKeys {
			m.statuses[gk] = statusPending
		}
	}
	return m
}

// Messages from the build goroutine.

type runMsg struct{ key string }
type skipMsg struct{ key string }
type finishMsg struct {
	key, target, verb, output string
	err                       error
}
type buildDoneMsg struct{ err error }
type tickMsg struct{}

// tick triggers a redraw on a slow cadence so the log panel reflects new
// lines from the ring buffer without each line being a bubbletea message.
func tick() tea.Cmd {
	return tea.Tick(80*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m model) Init() tea.Cmd { return tick() }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if m.graphView {
			m.clampDagOffsets()
		}
	case tea.KeyMsg:
		key := msg.String()
		if m.graphView {
			// Viewport panning. Steps are coarse (5 cols / 2 rows) because the
			// drawing has lots of whitespace; finer steps feel sluggish.
			const hStep, vStep = 5, 2
			panned := true
			switch key {
			case "left", "h":
				m.dagX -= hStep
			case "right", "l":
				m.dagX += hStep
			case "up", "k":
				m.dagY -= vStep
			case "down", "j":
				m.dagY += vStep
			case "home", "0":
				m.dagX = 0
			case "pgup":
				m.dagY -= 10
			case "pgdown":
				m.dagY += 10
			case "m":
				m.dagGroupMatrix = !m.dagGroupMatrix
				m.refreshDag()
				return m, nil
			default:
				panned = false
			}
			if panned {
				m.clampDagOffsets()
				return m, nil
			}
		}
		switch key {
		case "g":
			m.graphView = !m.graphView
			if m.graphView {
				m.refreshDag()
			}
		case "q", "esc":
			if m.buildDone {
				return m, tea.Quit
			}
		case "ctrl+c":
			if m.buildDone {
				return m, tea.Quit
			}
			m.cancelStage++
			switch m.cancelStage {
			case 1:
				m.build.Cancel()
			case 2:
				m.build.SignalAll(syscall.SIGTERM)
			case 3:
				m.build.SignalAll(syscall.SIGKILL)
			}
		}
	case runMsg:
		m.statuses[msg.key] = statusRunning
	case skipMsg:
		m.statuses[msg.key] = statusSkipped
	case finishMsg:
		if msg.err != nil {
			m.statuses[msg.key] = statusFailed
			m.failures = append(m.failures, failure{
				target: msg.target,
				verb:   msg.verb,
				err:    msg.err,
				output: msg.output,
			})
		} else {
			m.statuses[msg.key] = statusDone
		}
	case tickMsg:
		if m.graphView {
			m.refreshDag()
		}
		if m.buildDone {
			return m, nil
		}
		return m, tick()
	case buildDoneMsg:
		m.buildDone = true
		m.buildErr = msg.err
		// Refresh the cached DAG drawing one last time so the post-exit
		// dump reflects the final statuses. The tick-driven refresh runs
		// every 80ms; if the last finishMsg arrived between ticks and
		// buildDoneMsg arrived right after, the cache would otherwise
		// still show the pre-final state.
		if m.graphView {
			m.refreshDag()
		}
		return m, tea.Quit
	}
	return m, nil
}

// --- styling ---

var (
	pendingStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	runningStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Bold(true)
	doneStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	// Skipped uses bright cyan + a distinct glyph so it doesn't read as
	// "still pending" the way a dim-gray arrow did.
	skippedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Bold(true)
	failedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	logStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	headerStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true)
)

func (m model) View() string {
	return m.render(false)
}

// FinalView returns the same content as View but without the interactive
// "(press q to exit)" hint. The caller writes this to the regular terminal
// after bubbletea exits altscreen, so the user sees the final state in
// scrollback rather than nothing at all.
func (m model) FinalView() string {
	return m.render(true)
}

func (m model) render(final bool) string {
	var b strings.Builder

	// Cancel-stage banner: tells the user what each additional Ctrl+C does.
	// Hidden once the build is fully done.
	if !m.buildDone && m.cancelStage > 0 {
		var line string
		switch m.cancelStage {
		case 1:
			line = "cancelling — waiting for in-flight tasks (ctrl+c again: SIGTERM)"
		case 2:
			line = "SIGTERM sent (ctrl+c again: SIGKILL)"
		default:
			line = "SIGKILL sent"
		}
		b.WriteString(failedStyle.Render(line))
		b.WriteString("\n")
	}

	if m.graphView {
		b.WriteString(m.renderDagWindow())
	} else {
		// Tree.
		rootIcon, rootStyle := iconAndStyle(m.statuses[m.tree.rootKey])
		fmt.Fprintf(&b, "%s %s\n", rootIcon, rootStyle.Render(m.tree.rootLabel))
		for _, ln := range m.tree.lines {
			if len(ln.groupKeys) > 0 {
				icon, label := m.renderGroup(ln)
				fmt.Fprintf(&b, "%s%s %s\n", ln.prefix, icon, label)
				continue
			}
			icon, st := iconAndStyle(m.statuses[ln.key])
			fmt.Fprintf(&b, "%s%s %s\n", ln.prefix, icon, st.Render(ln.label))
		}
	}

	// Log panel: last N lines that fit. Skip on the scrollback final dump —
	// the user will have already seen the live log; only the failure summary
	// is worth preserving.
	if !final {
		logRows := 8
		if m.height > 0 {
			logRows = max(4, m.height/4)
		}
		tail := m.ring.tail(logRows)
		if len(tail) > 0 {
			b.WriteString("\n")
			b.WriteString(headerStyle.Render("── log ──"))
			b.WriteString("\n")
			for _, line := range tail {
				b.WriteString(logStyle.Render(line))
				b.WriteString("\n")
			}
		}
	}

	// Failure summary.
	if m.buildDone && len(m.failures) > 0 {
		b.WriteString("\n")
		b.WriteString(failedStyle.Render(fmt.Sprintf("─── %d failure(s) ───", len(m.failures))))
		b.WriteString("\n")
		first := m.failures[0]
		b.WriteString(failedStyle.Render(failureLabel(first)))
		b.WriteString("\n")
		b.WriteString(strings.TrimRight(first.output, "\n"))
		b.WriteString("\n")
		if first.err != nil {
			b.WriteString(failedStyle.Render(fmt.Sprintf("error: %v", first.err)))
			b.WriteString("\n")
		}
		for _, f := range m.failures[1:] {
			b.WriteString(failedStyle.Render("  - " + failureLabel(f)))
			b.WriteString(" — rerun for details\n")
		}
	}

	if !final && m.buildDone {
		b.WriteString("\n")
		b.WriteString(headerStyle.Render("(press q to exit)"))
		b.WriteString("\n")
	}

	return b.String()
}

func failureLabel(f failure) string {
	if f.verb != "" {
		return fmt.Sprintf("[%s %s]", f.verb, f.target)
	}
	return f.target
}

// renderGroup returns the leading icon and the styled label for a collapsed
// group line. The aggregate icon reflects the "worst" state — failed wins
// over running wins over pending wins over done/skipped — and the label
// appends counts for each non-zero state.
func (m model) renderGroup(ln treeLine) (string, string) {
	var pending, running, done, skipped, failed int
	for _, k := range ln.groupKeys {
		switch m.statuses[k] {
		case statusRunning:
			running++
		case statusDone:
			done++
		case statusSkipped:
			skipped++
		case statusFailed:
			failed++
		default:
			pending++
		}
	}
	agg := statusPending
	switch {
	case failed > 0:
		agg = statusFailed
	case running > 0:
		agg = statusRunning
	case pending == 0 && (done > 0 || skipped > 0):
		if done == 0 {
			agg = statusSkipped
		} else {
			agg = statusDone
		}
	}
	icon, _ := iconAndStyle(agg)

	parts := []string{ln.label}
	if done > 0 {
		parts = append(parts, doneStyle.Render(fmt.Sprintf("✓%d", done)))
	}
	if running > 0 {
		parts = append(parts, runningStyle.Render(fmt.Sprintf("●%d", running)))
	}
	if failed > 0 {
		parts = append(parts, failedStyle.Render(fmt.Sprintf("✗%d", failed)))
	}
	if skipped > 0 {
		parts = append(parts, skippedStyle.Render(fmt.Sprintf("≡%d", skipped)))
	}
	if pending > 0 {
		parts = append(parts, pendingStyle.Render(fmt.Sprintf("○%d", pending)))
	}
	return icon, strings.Join(parts, " ")
}

// renderDagWindow returns the visible portion of the cached drawing,
// cropped to dagX/dagY + viewport size, plus a footer line showing scroll
// position so the user knows there's more to pan to.
func (m *model) renderDagWindow() string {
	if m.dagResult == nil || m.dagW == 0 {
		return "(empty graph)\n"
	}
	visW, visH := m.dagVisibleSize()
	if visW < 1 || visH < 1 {
		return ""
	}
	// Reserve one row for the footer; let dagview render the rest.
	gridH := max(visH-1, 1)
	var sb strings.Builder
	sb.WriteString(m.dagResult.Render(m.dagX, m.dagY, visW, gridH, true))
	matrixState := "off"
	if m.dagGroupMatrix {
		matrixState = "on"
	}
	if m.dagW > visW || m.dagH > gridH {
		fmt.Fprintf(&sb, "[%d,%d / %dx%d  arrows/hjkl pan · m matrix-group:%s · g tree]",
			m.dagX, m.dagY, m.dagW, m.dagH, matrixState)
	} else {
		fmt.Fprintf(&sb, "[m matrix-group:%s · g tree]", matrixState)
	}
	sb.WriteByte('\n')
	return sb.String()
}

// refreshDag re-renders the DAG into the model's cache. Cheap enough at
// 80ms cadence for typical graphs; we'll add invalidation triggers (status
// changes only, not every tick) if this ever shows up in profiles.
func (m *model) refreshDag() {
	if m.tree.rootNode == nil {
		m.dagResult = nil
		m.dagW, m.dagH = 0, 0
		return
	}
	res := dagview.View(m.tree.rootNode, m.dagStatusOf, dagview.Options{
		UseUnicode:  true,
		GroupMatrix: m.dagGroupMatrix,
	})
	m.dagResult = res
	m.dagW = res.W()
	m.dagH = res.H()
	m.clampDagOffsets()
}

// clampDagOffsets pulls dagX/dagY back into the legal range given the
// current drawing size and last known terminal size.
func (m *model) clampDagOffsets() {
	visW, visH := m.dagVisibleSize()
	maxX := max(m.dagW-visW, 0)
	maxY := max(m.dagH-visH, 0)
	if m.dagX < 0 {
		m.dagX = 0
	}
	if m.dagX > maxX {
		m.dagX = maxX
	}
	if m.dagY < 0 {
		m.dagY = 0
	}
	if m.dagY > maxY {
		m.dagY = maxY
	}
}

// dagVisibleSize returns the (width, height) cells available for the DAG
// viewport. Reserves rows for the log panel + footer + cancel banner.
func (m *model) dagVisibleSize() (int, int) {
	w := m.width
	if w <= 0 {
		w = 80
	}
	h := m.height
	if h <= 0 {
		h = 24
	}
	logRows := 8
	if m.height > 0 {
		logRows = max(4, m.height/4)
	}
	// Reserved rows: 1 (header info) + logRows + 2 (── log ──, blank) + 1
	// (footer hint). Conservative; better to clip log than the graph.
	reserved := logRows + 4
	if h-reserved < 5 {
		reserved = h - 5
	}
	if reserved < 0 {
		reserved = 0
	}
	return w, h - reserved
}

// dagStatusOf adapts the model's status map to dagview.StatusFn. The dagview
// package owns its own enum to avoid importing this package's internals.
func (m model) dagStatusOf(target, verb string) dagview.Status {
	switch m.statuses[nodeKey(target, verb)] {
	case statusRunning:
		return dagview.StatusRunning
	case statusDone:
		return dagview.StatusDone
	case statusSkipped:
		return dagview.StatusSkipped
	case statusFailed:
		return dagview.StatusFailed
	}
	return dagview.StatusPending
}

func iconAndStyle(s status) (string, lipgloss.Style) {
	switch s {
	case statusRunning:
		return runningStyle.Render("●"), runningStyle
	case statusDone:
		return doneStyle.Render("✓"), doneStyle
	case statusSkipped:
		return skippedStyle.Render("≡"), skippedStyle
	case statusFailed:
		return failedStyle.Render("✗"), failedStyle
	default:
		return pendingStyle.Render("○"), pendingStyle
	}
}
