package runtime

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
)

// FailureRecord captures one target's failure context for end-of-build
// reporting. Both the non-TUI Execute path and the TUI fill this in via
// the dag OnFinish hook and render it through WriteFailureSummary so the
// two surfaces stay in sync.
type FailureRecord struct {
	Target string
	Verb   string
	Err    error
	Output string
}

// FailureLabel returns "[verb target]" if verb is set, else target.
func FailureLabel(f FailureRecord) string {
	if f.Verb != "" {
		return "[" + f.Verb + " " + f.Target + "]"
	}
	return f.Target
}

// WriteFailureSummary writes the standard end-of-build failure block to w:
// a "─── N failure(s) ───" header, the first failure's label + captured
// output verbatim, then a one-line "rerun for details" pointer per
// remaining failure. emph wraps headers/labels for emphasis (e.g. ANSI
// red); pass nil for plain text. Body output is always written unstyled.
func WriteFailureSummary(w io.Writer, fs []FailureRecord, emph func(string) string) {
	if len(fs) == 0 {
		return
	}
	if emph == nil {
		emph = func(s string) string { return s }
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, emph(fmt.Sprintf("─── %d failure(s) ───", len(fs))))
	first := fs[0]
	fmt.Fprintln(w, emph(FailureLabel(first)))
	if out := strings.TrimRight(first.Output, "\n"); out != "" {
		fmt.Fprintln(w, out)
	}
	if first.Err != nil {
		fmt.Fprintln(w, emph(fmt.Sprintf("error: %v", first.Err)))
	}
	for _, f := range fs[1:] {
		fmt.Fprintf(w, "%s — rerun for details\n", emph("  - "+FailureLabel(f)))
	}
}

// captures holds a per-(target,verb) bytes.Buffer protected by its own
// mutex. The non-TUI Execute path installs an OutputWriter that tees node
// stdout/stderr into these buffers so a failure can be replayed verbatim
// at the end of the build.
type captures struct {
	mu      sync.Mutex
	entries map[string]*captureEntry
}

type captureEntry struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func newCaptures() *captures {
	return &captures{entries: map[string]*captureEntry{}}
}

func (c *captures) get(target, verb string) *captureEntry {
	key := verb + "\x00" + target
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok {
		return e
	}
	e := &captureEntry{}
	c.entries[key] = e
	return e
}

func (c *captures) take(target, verb string) string {
	key := verb + "\x00" + target
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return ""
	}
	delete(c.entries, key)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.buf.String()
}

// teeCapture is the io.Writer the non-TUI OutputWriter hands to bodies.
// Writes go to the underlying stream (os.Stdout/Stderr) live AND to the
// per-node capture buffer for failure replay.
type teeCapture struct {
	out io.Writer
	e   *captureEntry
}

func (t *teeCapture) Write(p []byte) (int, error) {
	t.e.mu.Lock()
	t.e.buf.Write(p)
	t.e.mu.Unlock()
	return t.out.Write(p)
}
