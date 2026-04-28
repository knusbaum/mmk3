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
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/knusbaum/mmk3/cmd/mmk/runtime"
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

	m := initialModel(tree, ring)
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
	label  string // node label (target / [verb target])
	key    string // nodeKey for status lookup
}

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
	for i, dep := range root.DisplayDeps() {
		appendChild(&t, dep, "", i == len(root.DisplayDeps())-1, visited)
	}
	return t
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
	deps := n.DisplayDeps()
	for i, dep := range deps {
		appendChild(t, dep, childPrefix, i == len(deps)-1, visited)
	}
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

	width, height int
	buildErr      error
	buildDone     bool
}

func initialModel(t treeData, ring *logRing) model {
	m := model{
		tree:     t,
		statuses: map[string]status{},
		ring:     ring,
	}
	m.statuses[t.rootKey] = statusPending
	for _, ln := range t.lines {
		m.statuses[ln.key] = statusPending
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
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			if m.buildDone {
				return m, tea.Quit
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
		if m.buildDone {
			return m, nil
		}
		return m, tick()
	case buildDoneMsg:
		m.buildDone = true
		m.buildErr = msg.err
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

	// Tree.
	rootIcon, rootStyle := iconAndStyle(m.statuses[m.tree.rootKey])
	fmt.Fprintf(&b, "%s %s\n", rootIcon, rootStyle.Render(m.tree.rootLabel))
	for _, ln := range m.tree.lines {
		icon, st := iconAndStyle(m.statuses[ln.key])
		fmt.Fprintf(&b, "%s%s %s\n", ln.prefix, icon, st.Render(ln.label))
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
