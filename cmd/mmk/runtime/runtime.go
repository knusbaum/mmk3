// Package runtime wires the parsed mmk AST to the dag executor.
// It resolves concrete and pattern targets, generates the bash function
// script on demand, and implements dag.Node via TargetNode.
package runtime

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/knusbaum/mmk3/cmd/mmk/gen"
	"github.com/knusbaum/mmk3/cmd/mmk/parse"
	"github.com/knusbaum/mmk3/dag"
)

const (
	envSuppressFailureSummary = "MMK_SUPPRESS_FAILURE_SUMMARY"
)

// ErrCancelled is returned from a node's Run when the build was cancelled
// (Build.Cancel) before the node started executing. dag treats it like any
// other failure — downstream nodes propagate it and skip their own work.
var ErrCancelled = errors.New("build cancelled")

// runnerDefInfo records which optional phases a runner type has defined.
// The run phase is always assumed present for any type in Build.runnerDefs.
type runnerDefInfo struct {
	hasSetup   bool
	hasCleanup bool
}

// verbNodeKey is the map key for verb-qualified nodes and rules.
type verbNodeKey struct {
	target string
	verb   string
}

// defVerbBodyKey is the map key for verb-specific defbody declarations.
type defVerbBodyKey struct {
	typeName string
	verb     string
}

// matrixCombo is a set of variable assignments for one matrix combination.
type matrixCombo map[string]string

// matrixRuleInfo tracks the variables and combos for a matrix rule.
type matrixRuleInfo struct {
	vars   []string      // variable names in declaration order
	combos []matrixCombo // valid combos after applying excludes
	names  []string      // DAG target name for combos[i]; parallel array. May be
	// nil for info built outside computeComboTargetNames (group-projection
	// cascading), which still always uses comboTargetName(base, combo) itself.
}

// groupMember is one registered member of a group.
type groupMember struct {
	internalName string      // concrete combo target name, e.g. "test_case[os=linux input=in1]"
	combo        matrixCombo // full combo of this member; empty for non-matrix members
}

// groupData holds the registered members of a group, plus any description
// from the original `group` directive's `##` docstring (so PrintList can
// surface it on the synthetic group aggregator).
type groupData struct {
	members     []groupMember
	description string
}

// subprojectInfo is what the runtime tracks about each `subproject` directive
// after expansion: the target name (also the registered top-level target),
// the runner clause to wrap delegations in, and the directory containing the
// sub-mmkfile.
type subprojectInfo struct {
	target string
	runner string
	path   string
}

// Build holds the indexed rules and the generated bash script.
// Create one with NewBuild; call Close when done to remove the temp script
// and run cleanup for any runners that were started during the build.
// Set Verbose = true before calling Execute to log each target as it runs or is skipped.
// Set Why = true to print the dependency chain from root → target on every OnRun.
type Build struct {
	Verbose bool
	// Why prints the dep chain from the build root down to each target as
	// it starts running, so the user can see how a running node relates to
	// the target they asked for.
	Why bool
	// ReplayFailureOutput makes the non-TUI failure summary include the first
	// failed target's captured stdout/stderr. By default, command output is only
	// shown live to avoid duplicating diagnostics at the end of the build.
	ReplayFailureOutput bool
	// OutputWriter, if non-nil, supplies the stdout/stderr writers for a
	// node's body execution. Used by the TUI to capture per-node output for
	// later replay on failure. If nil, body output goes to os.Stdout/Stderr.
	OutputWriter func(target string, verb string) (stdout, stderr io.Writer)
	// SubprocessPgroups asks the runtime to launch every task subprocess
	// (bash for body execution, runner binaries) with Setpgid:true so
	// each subprocess and its descendants form their own process group.
	// Set this when the caller plans to drive cancellation through
	// SignalAll(sig) — `kill(-pgid, sig)` needs the subprocess to be a
	// pgroup leader so the signal reaches descendants like cc, docker
	// exec, or long-running dev servers without also affecting mmk
	// itself.
	//
	// Leave false (the default) for interactive runs where the user's
	// terminal Ctrl+C should naturally cascade to the subprocess via
	// the kernel's foreground-pgroup broadcast — that only works if
	// the subprocess shares mmk's pgroup.
	//
	// The TUI sets this to true because bubbletea's raw mode swallows
	// Ctrl+C as a keystroke event (so the foreground-pgroup broadcast
	// path never fires), making SignalAll the only way to kill the
	// subprocess tree. The flag is deliberately separate from
	// OutputWriter — OutputWriter is also installed by the failure-
	// capture tee in Build.Execute even in interactive mode, and
	// piggy-backing the pgroup choice on it would (and did) cause
	// interactive Ctrl+C to leak subprocess trees.
	SubprocessPgroups bool
	concretes         map[string]*parse.TargetRule
	verbConcretes     map[verbNodeKey]*parse.TargetRule
	patterns          []*patternEntry
	// nodes / verbNodes / runnerNodes are populated lazily by Resolve,
	// ResolveVerb, and runnerNode. dag.Build kicks off most of the writes,
	// but in the TUI those happen in the executor goroutine while the View
	// goroutine reads via NodeFor — so all four operations take nodesMu.
	nodesMu            sync.RWMutex
	nodes              map[string]*TargetNode
	verbNodes          map[verbNodeKey]*TargetNode
	runnerNodes        map[string]*TargetNode // runner target name → synthetic runner init node
	runnerDefs         map[string]runnerDefInfo
	defBodies          map[string]bool // type name → has default body (built-in or user defbody)
	defVerbBodies      map[defVerbBodyKey]bool
	userDefBodyOptions map[defVerbBodyKey][]parse.Option // user defbody options keyed by (type, verb)
	defBodyDeps        map[defVerbBodyKey][]string       // user defbody dep tokens keyed by (type, verb)
	defRunnerDeps      map[string][]string               // defrunner dep tokens keyed by runner type name (nil means: type does not customize, use default)
	defRunnerDepsCache map[string][]string               // resolved runner-instance dep names, keyed by runner target name; cached so the dep-clause bash runs once per runner instance
	subprojects        map[string]*subprojectInfo        // subproject target → metadata for sub-path delegation
	matrixInfo         map[string]*matrixRuleInfo        // base target → matrix info (for aggregators and dep resolution)
	matrixVars         map[string]matrixCombo            // internal combo target name → its variable assignments
	declaredGroups     map[string]bool                   // group names from `group` directives
	groups             map[string]*groupData             // populated during expansion
	genPath            string
	genFile            *os.File

	runnerStatesMu sync.Mutex
	runnerStates   map[string]string // runner target name → state from setup stdout

	// whyMu serializes -why hook prints across concurrent OnRun callbacks
	// so a multi-line chain renders atomically rather than interleaving
	// with a sibling node's chain.
	whyMu sync.Mutex

	// Cancellation: the TUI (and any other supervisor) can stop scheduling
	// new task bodies via Cancel, then escalate via SignalAll(SIGTERM/SIGKILL)
	// to interrupt in-flight bash subprocesses. A node already past the
	// IsCancelled check at entry continues until its bash returns or is signaled.
	cancelled     atomic.Bool
	cancelCh      chan struct{} // closed once by cancelOnce when Cancel is called
	cancelOnce    sync.Once
	runningCmdsMu sync.Mutex
	runningCmds   map[*exec.Cmd]struct{}
}

// Cancel marks the build as cancelled. Subsequent calls to TargetNode.Run
// return ErrCancelled before spawning a new subprocess. Goroutines blocked
// waiting for a semaphore slot are unblocked immediately via CancelCh.
// In-flight bash processes are not affected; use SignalAll to terminate them.
func (b *Build) Cancel() {
	b.cancelled.Store(true)
	b.cancelOnce.Do(func() { close(b.cancelCh) })
}

// IsCancelled reports whether Cancel has been called.
func (b *Build) IsCancelled() bool { return b.cancelled.Load() }

// CancelCh returns a channel that is closed when Cancel is called. It can be
// passed to dag.Execute so that goroutines blocked on the parallelism semaphore
// are unblocked immediately rather than draining one slot at a time.
func (b *Build) CancelCh() <-chan struct{} { return b.cancelCh }

// SignalAll sends sig to the process group of every currently-running task
// subprocess. Each cmd is launched with Setpgid so the signal reaches bash
// and any descendants it has spawned (e.g. cc, docker exec). Errors from
// individual signal calls are ignored — a process may have just exited.
func (b *Build) SignalAll(sig syscall.Signal) {
	b.runningCmdsMu.Lock()
	defer b.runningCmdsMu.Unlock()
	for c := range b.runningCmds {
		if c.Process == nil {
			continue
		}
		_ = syscall.Kill(-c.Process.Pid, sig)
	}
}

// registerCmd / unregisterCmd track in-flight subprocesses so SignalAll can
// reach them. Callers must invoke unregisterCmd via defer once cmd.Run returns.
func (b *Build) registerCmd(c *exec.Cmd) {
	b.runningCmdsMu.Lock()
	defer b.runningCmdsMu.Unlock()
	if b.runningCmds == nil {
		b.runningCmds = make(map[*exec.Cmd]struct{})
	}
	b.runningCmds[c] = struct{}{}
}

func (b *Build) unregisterCmd(c *exec.Cmd) {
	b.runningCmdsMu.Lock()
	defer b.runningCmdsMu.Unlock()
	delete(b.runningCmds, c)
}

type patternEntry struct {
	rule *parse.TargetRule
	re   *regexp.Regexp
}

// validateDirectives checks the AST against runtime constraints:
//   - Each TargetRule's type is either built-in or has a deftype.
//   - Each defrunner with a setup or cleanup phase also has a run phase.
//   - Each TargetRule's `on` clause names an existing concrete target whose
//     type has a defrunner run definition (built-in or user-defined).
func validateDirectives(f *parse.File) error {
	type defBodyKey struct{ typ, verb string }
	deftypes := make(map[string]bool)
	defbodies := make(map[defBodyKey]bool)
	concretes := make(map[string]*parse.TargetRule)

	// Collect user defrunner phases to validate completeness and build valid runner set.
	type runnerPhases struct{ hasRun, hasSetup, hasCleanup bool }
	userRunners := make(map[string]runnerPhases)

	for _, d := range f.Directives {
		switch d := d.(type) {
		case *parse.DefRunner:
			info := userRunners[d.Name]
			switch d.Phase {
			case "":
				info.hasRun = true
			case "setup":
				info.hasSetup = true
			case "cleanup":
				info.hasCleanup = true
			}
			userRunners[d.Name] = info
		case *parse.DefType:
			deftypes[d.Name] = true
		case *parse.DefBody:
			key := defBodyKey{d.Type, d.Verb}
			if defbodies[key] {
				if d.Verb != "" {
					return fmt.Errorf("duplicate defbody for type %q verb %q", d.Type, d.Verb)
				}
				return fmt.Errorf("duplicate defbody for type %q", d.Type)
			}
			defbodies[key] = true
			// Verb-specific defbody dep clauses parse but aren't yet honored
			// by the runtime (verb deps inherit from the build defbody via
			// existing verb-rule semantics). Reject up front rather than
			// silently dropping deps the user wrote.
			if d.Verb != "" && len(d.Deps) > 0 {
				return fmt.Errorf("defbody %q verb %q: dep clause is only supported on the build-verb defbody", d.Type, d.Verb)
			}
		case *parse.TargetRule:
			if d.Pattern == "" && d.Verb == "" {
				concretes[d.Target] = d
			}
		}
	}

	// A defrunner with setup/cleanup but no run body is always a mistake.
	for typeName, info := range userRunners {
		if !info.hasRun && (info.hasSetup || info.hasCleanup) {
			return fmt.Errorf("defrunner %q has setup or cleanup but no run body (add: defrunner %s { ... })", typeName, typeName)
		}
	}

	for key := range defbodies {
		if gen.BuiltinDefTypes[key.typ] == "" && !deftypes[key.typ] {
			return fmt.Errorf("defbody %q: unknown type (define with deftype)", key.typ)
		}
	}

	// Valid runner types = built-in runner types + user types with a run body.
	validRunnerTypes := make(map[string]bool)
	for typeName := range gen.BuiltinRunnerDefs {
		validRunnerTypes[typeName] = true
	}
	for typeName, info := range userRunners {
		if info.hasRun {
			validRunnerTypes[typeName] = true
		}
	}

	for _, d := range f.Directives {
		r, ok := d.(*parse.TargetRule)
		if !ok {
			continue
		}
		name := r.Target
		if r.Pattern != "" {
			name = "'" + r.Pattern + "'"
		}
		if r.Type != "" && gen.BuiltinDefTypes[r.Type] == "" && !deftypes[r.Type] {
			return fmt.Errorf("target %q uses unknown type %q (define with deftype)", name, r.Type)
		}
		if r.Runner != "" {
			runner, ok := concretes[r.Runner]
			if !ok {
				return fmt.Errorf("target %q uses unknown runner target %q", name, r.Runner)
			}
			if !validRunnerTypes[runner.Type] {
				return fmt.Errorf("target %q uses runner %q of type %q, which cannot be used as a runner", name, r.Runner, runner.Type)
			}
		}
		if err := validateOrderOption(r.Options, fmt.Sprintf("target %q", name), r.Type, validRunnerTypes); err != nil {
			return err
		}
	}

	// `order=` on a defbody is meaningful only when the type has a defrunner —
	// otherwise no target can `on <T>` and there are no consumers to order
	// relative to. Reject up front so users don't get silent no-ops.
	for _, d := range f.Directives {
		db, ok := d.(*parse.DefBody)
		if !ok {
			continue
		}
		ctx := fmt.Sprintf("defbody %q", db.Type)
		if db.Verb != "" {
			ctx = fmt.Sprintf("defbody %q verb %q", db.Type, db.Verb)
		}
		if err := validateOrderOption(db.Options, ctx, db.Type, validRunnerTypes); err != nil {
			return err
		}
	}
	return nil
}

// validateOrderOption checks that an `order=` option (if present) has a known
// value AND that the type is a runner. Other option keys are not validated here.
func validateOrderOption(options []parse.Option, ctx, typeName string, validRunnerTypes map[string]bool) error {
	for _, opt := range options {
		if opt.Key != "order" {
			continue
		}
		switch opt.Value {
		case "before-consumers", "after-consumers":
		default:
			return fmt.Errorf("%s: invalid order=%q (must be before-consumers or after-consumers)", ctx, opt.Value)
		}
		if typeName == "" || !validRunnerTypes[typeName] {
			return fmt.Errorf("%s: order=%s requires a defrunner for type %q", ctx, opt.Value, typeName)
		}
	}
	return nil
}

// NewBuild parses src (without resolving include directives), validates
// names, generates the initial bash script, and returns a Build ready for
// Execute. Use NewBuildFromFile when you want include resolution; this
// byte-based entry point exists for tests and for callers that have
// already produced an mmkfile in memory.
func NewBuild(src []byte) (*Build, error) {
	f, err := parse.Parse(src)
	if err != nil {
		return nil, err
	}
	return newBuildFromAST(f)
}

// NewBuildFromFile reads the mmkfile at path, parses it (recursively
// resolving any `include` directives relative to the file's directory),
// and returns a Build ready for Execute.
func NewBuildFromFile(path string) (*Build, error) {
	f, err := parse.ParseFile(path)
	if err != nil {
		return nil, err
	}
	return newBuildFromAST(f)
}

// newBuildFromAST is the shared implementation behind NewBuild and
// NewBuildFromFile. It assumes any Include directives have already been
// resolved (or were never present); an Include surviving to this point is
// a programming error and produces an error here.
func newBuildFromAST(f *parse.File) (*Build, error) {
	for _, d := range f.Directives {
		if inc, ok := d.(*parse.Include); ok {
			return nil, fmt.Errorf("unresolved include %q at line %d (use NewBuildFromFile to resolve includes)", inc.Path, inc.Line)
		}
	}

	b := &Build{
		concretes:          make(map[string]*parse.TargetRule),
		verbConcretes:      make(map[verbNodeKey]*parse.TargetRule),
		nodes:              make(map[string]*TargetNode),
		verbNodes:          make(map[verbNodeKey]*TargetNode),
		runnerNodes:        make(map[string]*TargetNode),
		runnerDefs:         make(map[string]runnerDefInfo),
		defBodies:          make(map[string]bool),
		defVerbBodies:      make(map[defVerbBodyKey]bool),
		userDefBodyOptions: make(map[defVerbBodyKey][]parse.Option),
		defBodyDeps:        make(map[defVerbBodyKey][]string),
		defRunnerDeps:      make(map[string][]string),
		defRunnerDepsCache: make(map[string][]string),
		subprojects:        make(map[string]*subprojectInfo),
		matrixInfo:         make(map[string]*matrixRuleInfo),
		matrixVars:         make(map[string]matrixCombo),
		declaredGroups:     make(map[string]bool),
		groups:             make(map[string]*groupData),
		runnerStates:       make(map[string]string),
		cancelCh:           make(chan struct{}),
	}

	// Populate runnerDefs from built-in definitions.
	for typeName, info := range gen.BuiltinRunnerDefs {
		b.runnerDefs[typeName] = runnerDefInfo{hasSetup: info.HasSetup, hasCleanup: info.HasCleanup}
		// Built-in runner dep clause (e.g. image's skip-aware $(__mmk_runner_deps_image)).
		// A user-supplied run-stage defrunner for this type takes over below.
		if len(info.Deps) > 0 {
			b.defRunnerDeps[typeName] = info.Deps
		}
	}
	// Layer user defrunner phases on top.
	for _, d := range f.Directives {
		if dr, ok := d.(*parse.DefRunner); ok {
			info := b.runnerDefs[dr.Name]
			switch dr.Phase {
			case "setup":
				info.hasSetup = true
			case "cleanup":
				info.hasCleanup = true
			default:
				// Run-stage form. A user run-stage defrunner replaces any
				// built-in dep clause for this type — even if the user
				// omitted the `:` (no clause = historical "auto-add R"
				// default), that's still an opt-in choice that overrides
				// what mmk shipped for the type.
				if dr.HasDeps {
					b.defRunnerDeps[dr.Name] = dr.Deps
				} else {
					delete(b.defRunnerDeps, dr.Name)
				}
			}
			b.runnerDefs[dr.Name] = info
		}
	}

	// Pre-populate with built-in default bodies.
	for typeName := range gen.BuiltinDefBodies {
		b.defBodies[typeName] = true
	}

	// Pre-populate with built-in verb bodies; user defbody entries below may override.
	for typeName, verbs := range gen.BuiltinVerbBodies {
		for verb := range verbs {
			b.defVerbBodies[defVerbBodyKey{typeName, verb}] = true
		}
	}

	// Register defbodies first; they don't depend on target-name expansion.
	for _, d := range f.Directives {
		if d, ok := d.(*parse.DefBody); ok {
			if len(d.Options) > 0 {
				b.userDefBodyOptions[defVerbBodyKey{d.Type, d.Verb}] = d.Options
			}
			if len(d.Deps) > 0 {
				b.defBodyDeps[defVerbBodyKey{d.Type, d.Verb}] = d.Deps
			}
			if d.Verb != "" {
				b.defVerbBodies[defVerbBodyKey{d.Type, d.Verb}] = true
			} else {
				b.defBodies[d.Type] = true
			}
		}
	}

	// Generate the bash script first so target-name and runner expansion can
	// source it for $VAR lookups.
	genf, err := os.CreateTemp("", "mmk-generated-*.sh")
	if err != nil {
		return nil, err
	}
	b.genPath = genf.Name()
	b.genFile = genf

	frozen, err := evalPassthroughs(f)
	if err != nil {
		frozen = nil // fall back to verbatim passthroughs on error
	}
	if err := gen.Generate(genf, f, frozen); err != nil {
		genf.Close()
		os.Remove(b.genPath)
		return nil, err
	}

	// Expand $VAR in concrete target names and runner clauses before validation
	// and concrete-rule registration, so the rest of the build sees resolved
	// names.
	if err := b.expandRuleNames(f); err != nil {
		b.genFile.Close()
		os.Remove(b.genPath)
		return nil, err
	}

	// Collect declared groups from `group` directives.
	for _, d := range f.Directives {
		if g, ok := d.(*parse.Group); ok {
			b.declaredGroups[g.Name] = true
			b.groups[g.Name] = &groupData{description: g.Description}
		}
	}

	// Propagate `deftype TYPE into GROUP` membership onto every concrete or
	// matrix rule of that type (not pattern rules or verb rules), merging
	// with (not replacing) any `into` clauses the rule itself declares.
	typeGroups := make(map[string][]string)
	for _, d := range f.Directives {
		if dt, ok := d.(*parse.DefType); ok {
			for _, groupName := range dt.Groups {
				if !b.declaredGroups[groupName] {
					b.genFile.Close()
					os.Remove(b.genPath)
					return nil, fmt.Errorf("deftype %s: into %s: group %q is not declared (add: group %s)", dt.Name, groupName, groupName, groupName)
				}
			}
			typeGroups[dt.Name] = dt.Groups
		}
	}
	if len(typeGroups) > 0 {
		for _, d := range f.Directives {
			r, ok := d.(*parse.TargetRule)
			if !ok || r.Pattern != "" || r.Verb != "" {
				continue
			}
			for _, groupName := range typeGroups[r.Type] {
				if !slices.Contains(r.Groups, groupName) {
					r.Groups = append(r.Groups, groupName)
				}
			}
		}
	}

	// Check whether any group features are used.
	hasGroupFeatures := b.hasGroupFeatures(f)

	if !hasGroupFeatures {
		// No group features: use the original single-pass expansion.
		if err := b.expandMatrixRules(f); err != nil {
			b.genFile.Close()
			os.Remove(b.genPath)
			return nil, err
		}
	} else {
		// Multi-pass group expansion.
		if err := b.validateGroups(f); err != nil {
			b.genFile.Close()
			os.Remove(b.genPath)
			return nil, err
		}
		if err := b.expandExplicitMatrixRules(f); err != nil {
			b.genFile.Close()
			os.Remove(b.genPath)
			return nil, err
		}
		// Fixed-point iteration for cascading groups.
		const maxIter = 100
		for iter := 0; iter < maxIter; iter++ {
			prevMemberCount := b.totalGroupMembers()
			b.registerGroupMembers(f)
			if err := b.createGroupAggregators(f); err != nil {
				b.genFile.Close()
				os.Remove(b.genPath)
				return nil, err
			}
			if err := b.expandGroupConsumers(f); err != nil {
				b.genFile.Close()
				os.Remove(b.genPath)
				return nil, err
			}
			if b.totalGroupMembers() == prevMemberCount {
				break
			}
			if iter == maxIter-1 {
				b.genFile.Close()
				os.Remove(b.genPath)
				return nil, fmt.Errorf("group expansion did not converge after %d iterations", maxIter)
			}
		}
		if err := b.validateGroupConsumersExpanded(f); err != nil {
			b.genFile.Close()
			os.Remove(b.genPath)
			return nil, err
		}
	}

	// Expand each `subproject` directive: read its sub-mmkfile, harvest verbs,
	// and append synthetic TargetRules to f.Directives so the existing
	// validation + registration loop picks them up.
	if err := b.expandSubprojects(f); err != nil {
		b.genFile.Close()
		os.Remove(b.genPath)
		return nil, err
	}

	if err := gen.ValidateDuplicates(f); err != nil {
		b.genFile.Close()
		os.Remove(b.genPath)
		return nil, err
	}
	if err := validateDirectives(f); err != nil {
		b.genFile.Close()
		os.Remove(b.genPath)
		return nil, err
	}

	// Register concrete and pattern rules.
	for _, d := range f.Directives {
		r, ok := d.(*parse.TargetRule)
		if !ok {
			continue
		}
		if r.Pattern != "" {
			re, err := regexp.Compile(`^(?:` + r.Pattern + `)$`)
			if err != nil {
				b.genFile.Close()
				os.Remove(b.genPath)
				return nil, fmt.Errorf("pattern %q: %w", r.Pattern, err)
			}
			b.patterns = append(b.patterns, &patternEntry{rule: r, re: re})
		} else if r.Verb != "" {
			b.verbConcretes[verbNodeKey{r.Target, r.Verb}] = r
		} else {
			b.concretes[r.Target] = r
		}
	}

	return b, nil
}

// Close runs cleanup for any runners that were started and removes the
// temporary generated script.
func (b *Build) Close() {
	b.runnerStatesMu.Lock()
	states := b.runnerStates
	b.runnerStates = nil
	b.runnerStatesMu.Unlock()

	for runnerTarget, state := range states {
		rule := b.concretes[runnerTarget]
		if rule == nil {
			continue
		}
		info := b.runnerDefs[rule.Type]
		if !info.hasCleanup {
			continue
		}
		script := `. "$MMK_GENFILE"; ` + gen.RunnerCleanupFunc(rule.Type)
		cmd := exec.Command("bash", "-c", script)
		cmd.Env = append(os.Environ(),
			"MMK_GENFILE="+b.genPath,
			"target="+runnerTarget,
			"MMK_RUNNER_STATE="+state,
		)
		cmd.Env = appendRuleOptions(cmd.Env, rule)
		cmd.Run() //nolint — best-effort cleanup
	}
	b.genFile.Close()
	os.Remove(b.genPath)
}

// runnerNode returns (creating once) the synthetic node that runs setup for
// the given runner target. Multiple targets with `on runnerTarget` share a
// single runner node so setup executes only once. Safe to call from any
// goroutine — guarded by nodesMu like Resolve.
func (b *Build) runnerNode(runnerTarget *TargetNode) *TargetNode {
	b.nodesMu.RLock()
	if n, ok := b.runnerNodes[runnerTarget.target]; ok {
		b.nodesMu.RUnlock()
		return n
	}
	b.nodesMu.RUnlock()
	b.nodesMu.Lock()
	defer b.nodesMu.Unlock()
	if n, ok := b.runnerNodes[runnerTarget.target]; ok {
		return n
	}
	n := &TargetNode{
		build:     b,
		target:    "__runner__" + runnerTarget.target,
		kind:      kindRunner,
		runnerFor: runnerTarget,
	}
	b.runnerNodes[runnerTarget.target] = n
	return n
}

// GenPath returns the path of the generated bash script.
func (b *Build) GenPath() string { return b.genPath }

// NodeFor returns the TargetNode for the given target and verb, or nil if it
// has not been resolved. Safe to call from any goroutine — the maps are
// guarded by nodesMu so concurrent Resolve/ResolveVerb writes are serialized.
func (b *Build) NodeFor(target, verb string) *TargetNode {
	b.nodesMu.RLock()
	defer b.nodesMu.RUnlock()
	if verb == "" {
		return b.nodes[target]
	}
	return b.verbNodes[verbNodeKey{target, verb}]
}

// HasVerb reports whether the given verb is defined anywhere visible from
// this Build — top-level verb rules, verb patterns, defbody verbs, or any
// (recursively reachable) subproject's verb set. Used to distinguish a typo
// from a verb that happens not to fire on a particular target.
func (b *Build) HasVerb(verb string) bool {
	if verb == "" {
		return false
	}
	for _, v := range b.Verbs() {
		if v == verb {
			return true
		}
	}
	for _, s := range b.walkSubprojects() {
		for _, v := range s.verbs {
			if v == verb {
				return true
			}
		}
	}
	return false
}

// HasTarget reports whether name is a known concrete or pattern-matched target.
func (b *Build) HasTarget(name string) bool {
	if _, ok := b.concretes[name]; ok {
		return true
	}
	for _, pe := range b.patterns {
		if pe.re.MatchString(name) {
			return true
		}
	}
	return false
}

// Prepare resolves all transitive dependencies of target+verb, fully populating
// the generated bash script, without running any nodes.
func (b *Build) Prepare(target, verb string) error {
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
	_, err = dag.Build(root)
	return err
}

// checkVerbHasTargets returns an error when invoking [verb target] would
// produce a no-op: nothing in the dependency graph has a body to run. This
// catches typos like `mmk chekc all` — `chekc` isn't defined anywhere, verb
// inheritance creates a graph of empty verb nodes, and without this guard
// mmk would silently exit 0 having done nothing.
//
// A non-verb node in the graph always counts as work: non-verb deps only
// appear in a verb node's graph via the user's explicit `:` or `:+` clause
// (inherited deps are always verb-applied via inheritedVerbDeps), so they
// are direct user intent.
func checkVerbHasTargets(root *TargetNode) error {
	if root.verb == "" {
		return nil
	}
	if subgraphHasBody(root, make(map[*TargetNode]bool)) {
		return nil
	}
	return fmt.Errorf("verb %q on %q has no targets with bodies in its dependency graph", root.verb, root.target)
}

func subgraphHasBody(n *TargetNode, seen map[*TargetNode]bool) bool {
	if seen[n] {
		return false
	}
	seen[n] = true
	if _, has := n.executeScript(); has {
		return true
	}
	for _, dep := range n.Dependencies() {
		if subgraphHasBody(dep, seen) {
			return true
		}
	}
	return false
}

// Execute builds the DAG rooted at target (optionally qualified by verb) and
// runs it with the given parallelism. parallelism <= 0 means unlimited.
// When b.Verbose is true, each target is logged as it runs or is skipped.
// When b.Why is true, the dep chain from root → target is printed on each
// OnRun, so the user can see how a running node relates to the requested
// target.
func (b *Build) Execute(target, verb string, parallelism int) error {
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
	var pi *parentIndex
	if b.Why {
		pi = buildParentIndex(root)
	}

	// Tee body output through a per-node capture buffer so failures can be
	// replayed at the end of the build. Only installed when the caller
	// hasn't pre-set OutputWriter (the TUI installs its own capture-only
	// writer and renders failures itself).
	var caps *captures
	if b.OutputWriter == nil {
		caps = newCaptures()
		b.OutputWriter = func(target, verb string) (io.Writer, io.Writer) {
			e := caps.get(target, verb)
			return &teeCapture{out: os.Stdout, e: e}, &teeCapture{out: os.Stderr, e: e}
		}
		defer func() { b.OutputWriter = nil }()
	}

	var failuresMu sync.Mutex
	var failures []FailureRecord
	// ran tracks which nodes actually had OnRun fire — i.e. their own body
	// got a chance to execute. A node whose upstream failed gets OnFinish
	// with the propagated err but never sees OnRun (dag/dag.go skips
	// NeedsRun/Run in that case). Filtering on this set keeps aggregator
	// nodes like `test : <fanned-out test files>` out of the failure
	// summary — they're red because their deps are, not because their
	// own body broke.
	var ran sync.Map

	hooks := dag.Hooks[*TargetNode]{
		OnRun: func(n *TargetNode) {
			ran.Store(n, struct{}{})
			if b.Why {
				chain := pi.path(n)
				if len(chain) == 0 {
					return
				}
				out := renderWhyPath(chain)
				b.whyMu.Lock()
				fmt.Print(out)
				b.whyMu.Unlock()
				return
			}
			if !b.Verbose {
				return
			}
			if n.kind == kindRunner {
				fmt.Printf("starting runner: %s\n", n.runnerFor.target)
			} else if n.verb != "" {
				fmt.Printf("running: [%s %s]\n", n.verb, n.target)
			} else {
				fmt.Printf("running: %s\n", n.target)
			}
		},
		OnSkip: func(n *TargetNode) {
			// -why focuses on what's running, not what's skipped. Skip
			// output only when the user also passed -v.
			if !b.Verbose {
				return
			}
			if n.kind == kindRunner {
				return // runner setup dedup is an internal detail, not user-visible
			}
			fmt.Printf("skipping: %s (up to date)\n", n.target)
		},
		OnFinish: func(n *TargetNode, err error) {
			if err == nil {
				return
			}
			if errors.Is(err, dag.ErrCancelled) || errors.Is(err, ErrCancelled) {
				return
			}
			if _, ok := ran.Load(n); !ok {
				return // propagated upstream failure; don't double-count
			}
			var out string
			if caps != nil {
				out = caps.take(n.Target(), n.Verb())
			}
			failuresMu.Lock()
			failures = append(failures, FailureRecord{
				Target: n.Target(),
				Verb:   n.Verb(),
				Err:    err,
				Output: out,
			})
			failuresMu.Unlock()
		},
	}
	execErr := dag.Execute(root, parallelism, b.cancelCh, hooks)
	// Only render the summary when we installed capture ourselves. Callers
	// like the TUI handle their own failure surface and don't want a second
	// copy spilled to stderr. Nested mmk processes run from recipe bodies are
	// suppressed so parent builds don't compose multiple summaries.
	if caps != nil && os.Getenv(envSuppressFailureSummary) != "1" {
		WriteFailureSummary(os.Stderr, failureSummaryRecords(failures, b.ReplayFailureOutput), nil)
	}
	return execErr
}

// Resolve returns (creating if necessary) the TargetNode for the named target.
// Safe to call from any goroutine — the read-then-write on b.nodes is
// serialized by nodesMu. Multiple calls with the same name return the same
// node (the second wins-the-race caller picks up its sibling's node via the
// double-checked read inside the write lock).
func (b *Build) Resolve(name string) (*TargetNode, error) {
	b.nodesMu.RLock()
	if n, ok := b.nodes[name]; ok {
		b.nodesMu.RUnlock()
		return n, nil
	}
	b.nodesMu.RUnlock()
	b.nodesMu.Lock()
	defer b.nodesMu.Unlock()
	if n, ok := b.nodes[name]; ok {
		return n, nil
	}
	rule, err := b.findRule(name)
	if err != nil {
		return nil, err
	}
	n := &TargetNode{build: b, target: name, rule: rule}
	b.nodes[name] = n
	return n, nil
}

// ResolveVerb returns (creating if necessary) the verb-qualified TargetNode for
// [verb target]. An explicit verb rule takes precedence; otherwise an inherited
// node is created as long as the target has a default rule. Safe to call from
// any goroutine — the read-then-write is serialized by nodesMu.
func (b *Build) ResolveVerb(target, verb string) (*TargetNode, error) {
	key := verbNodeKey{target, verb}
	b.nodesMu.RLock()
	if n, ok := b.verbNodes[key]; ok {
		b.nodesMu.RUnlock()
		return n, nil
	}
	b.nodesMu.RUnlock()
	b.nodesMu.Lock()
	defer b.nodesMu.Unlock()
	if n, ok := b.verbNodes[key]; ok {
		return n, nil
	}
	rule := b.findRuleForVerb(target, verb)
	if rule == nil {
		// Inherit from the target's default rule. Make sure that default rule
		// exists in b.concretes (findRule infers a source-typed rule for
		// previously-unknown names). findRule writes to b.concretes; we're
		// already holding the write lock, so it's safe to call inline.
		if _, ok := b.concretes[target]; !ok {
			b.findRule(target) //nolint — side effect: populates concretes
		}
		if _, ok := b.concretes[target]; !ok {
			found := false
			for _, pe := range b.patterns {
				if pe.rule.Verb == "" && pe.re.MatchString(target) {
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("no rule to build [%s %s]", verb, target)
			}
		}
	}
	n := &TargetNode{build: b, target: target, verb: verb, rule: rule}
	b.verbNodes[key] = n
	return n, nil
}

// findRule returns the rule for the bare target name, instantiating a pattern
// rule if needed. Falls back to inferring a source-typed rule when nothing
// matches, so real input files (e.g. `.c` sources) don't need declarations.
func (b *Build) findRule(name string) (*parse.TargetRule, error) {
	if r := b.findRuleForVerb(name, ""); r != nil {
		return r, nil
	}
	// Before treating this as a source file, check whether it's a path into
	// a subproject (e.g. "cmd/bas/test" when "cmd/bas" is a subproject).
	// ResolveSubpath registers a delegating rule in b.concretes if it matches;
	// callers hold b.nodesMu write lock so the write is safe.
	if b.ResolveSubpath(name, "") {
		if r := b.findRuleForVerb(name, ""); r != nil {
			return r, nil
		}
	}
	inferred := &parse.TargetRule{
		Type:   "source",
		Target: name,
		Body:   "\n\t" + gen.DefaultFunc("source") + "\n",
	}
	b.concretes[name] = inferred
	return inferred, nil
}

// findRuleForVerb returns the cached or pattern-instantiated rule for the
// [verb target] pair (verb="" means a bare lookup), or nil if no rule and no
// pattern matches. Callers add their own fallback: bare lookups infer a
// source-typed rule; verb lookups treat absence as inheritance from the
// target's default rule.
func (b *Build) findRuleForVerb(target, verb string) *parse.TargetRule {
	if verb == "" {
		if r, ok := b.concretes[target]; ok {
			return r
		}
	} else {
		if r, ok := b.verbConcretes[verbNodeKey{target, verb}]; ok {
			return r
		}
	}
	for _, pe := range b.patterns {
		if pe.rule.Verb != verb {
			continue
		}
		m := pe.re.FindStringSubmatch(target)
		if m == nil {
			continue
		}
		captures := m[1:]
		instantiated := &parse.TargetRule{
			Type:      pe.rule.Type,
			Target:    target,
			Verb:      verb,
			Runner:    pe.rule.Runner,
			HasDepSep: pe.rule.HasDepSep,
			Body:      substituteCaptures(pe.rule.Body, captures),
			// Preserve the source pattern regex so display layers can group
			// children that came from the same pattern (the typical "huge .o
			// fan-out" case worth collapsing in a tree view).
			Pattern: pe.rule.Pattern,
		}
		for _, dep := range pe.rule.Deps {
			instantiated.Deps = append(instantiated.Deps, parse.Dep{
				Target: substituteCaptures(dep.Target, captures),
				Verb:   dep.Verb,
			})
		}
		if verb == "" {
			b.concretes[target] = instantiated
		} else {
			b.verbConcretes[verbNodeKey{target, verb}] = instantiated
		}
		return instantiated
	}
	return nil
}

// substituteCaptures replaces $1..$9 in s with the corresponding capture group.
// Substitution goes from highest index to lowest to avoid $1 matching inside $10.
func substituteCaptures(s string, captures []string) string {
	for i := len(captures); i >= 1; i-- {
		s = strings.ReplaceAll(s, fmt.Sprintf("$%d", i), captures[i-1])
	}
	return s
}

// targetKind distinguishes a normal user-declared target from internal
// synthetic nodes the runtime adds to the DAG (runner setup nodes).
type targetKind int

const (
	kindRule   targetKind = iota // backed by a parse.TargetRule
	kindRunner                   // synthetic: runs the setup phase for a runner target
)

// TargetNode is a dag.Node[*TargetNode]. Most TargetNodes are kindRule and
// model a user-declared target. Synthetic nodes (runner setup) use kindRunner
// and dispatch on `kind` inside the Date/NeedsRun/Run methods.
// When verb is non-empty the node represents a verb rule (e.g. [clean executable]).
// NodeState is the live execution state of a TargetNode. The TUI reads this
// directly on each tick rather than receiving per-node push events.
type NodeState int32

const (
	NodePending NodeState = iota
	NodeRunning
	NodeSkipped
	NodeDone
	NodeFailed
	NodeCancelled
)

type TargetNode struct {
	build      *Build
	target     string
	verb       string            // non-empty for verb nodes
	rule       *parse.TargetRule // nil when kind != kindRule, or for inherited verb nodes
	kind       targetKind
	runnerFor  *TargetNode // set when kind == kindRunner
	deps       []*TargetNode
	depsBuilt  bool
	resolveErr error

	dateMu    sync.Mutex
	dateVal   time.Time
	dateErr   error
	dateValid bool

	nodeState atomic.Int32 // NodeState; read by TUI via State()
}

// Target returns the target name for this node.
func (n *TargetNode) Target() string { return n.target }

// Verb returns the verb name for this node, or "" for a non-verb node.
func (n *TargetNode) Verb() string { return n.verb }

// State returns the current execution state of this node. Safe to call from
// any goroutine; updated atomically by the DAG worker.
func (n *TargetNode) State() NodeState { return NodeState(n.nodeState.Load()) }

// SetState updates the node's execution state. Called from TUI hooks so the
// TUI tick can read state directly without going through the event loop.
func (n *TargetNode) SetState(s NodeState) { n.nodeState.Store(int32(s)) }

// SourcePattern returns the source pattern regex string if this node was
// instantiated from a pattern rule, or "" if it has a concrete rule. Used
// by display layers to group nodes that came from the same pattern.
func (n *TargetNode) SourcePattern() string {
	if n.rule == nil {
		// Verb node with no explicit verb rule: fall back to the default
		// rule's pattern so display layers can group it with its siblings.
		if n.verb != "" {
			if defaultRule := n.build.concretes[n.target]; defaultRule != nil {
				return defaultRule.Pattern
			}
		}
		return ""
	}
	return n.rule.Pattern
}

// DisplayDeps returns the regular deps of this node minus runner setup nodes
// and verb subtrees that contain no executable body. Used by display layers
// (TUI, future graph variants) to walk the tree the way it'd actually run.
func (n *TargetNode) DisplayDeps() []*TargetNode {
	var out []*TargetNode
	for _, d := range n.Dependencies() {
		if d.kind == kindRunner {
			continue
		}
		if shouldPruneVerbSubgraph(d) {
			continue
		}
		out = append(out, d)
	}
	return out
}

// Dependencies resolves each named dep to a TargetNode, instantiating pattern
// rules as needed. Targets with `on R` get two implicit deps appended: the
// runner target R itself (for freshness) and the synthetic runner node for R
// (for setup ordering). Any resolution error is stored and returned by Run.
func (n *TargetNode) Dependencies() []*TargetNode {
	if n.depsBuilt {
		return n.deps
	}
	n.depsBuilt = true

	if n.kind == kindRunner {
		// The synthetic runner node's deps come from the runner type's dep
		// clause (default: the runner target itself). If the clause elides
		// the runner target — e.g., image's skip-aware clause when we're
		// already inside a container — the setup phase has nothing to wait
		// on and won't attempt the docker build / pull either.
		injected, err := n.build.runnerInjectedDepNames(n.runnerFor.target)
		if err != nil {
			n.resolveErr = err
			return n.deps
		}
		for _, name := range injected {
			depNode, err := n.build.Resolve(name)
			if err != nil {
				n.resolveErr = err
				return n.deps
			}
			n.deps = append(n.deps, depNode)
		}
		return n.deps
	}

	if n.verb != "" {
		return n.verbDependencies()
	}

	for _, dep := range n.rule.Deps {
		// Explicit combo specifier: [target @ k=v ...], values may contain $var.
		if len(dep.Combo) > 0 {
			currentCombo := n.build.matrixVars[n.target]
			constraints := make(matrixCombo, len(dep.Combo))
			for _, opt := range dep.Combo {
				constraints[opt.Key] = substituteMatrixVars(opt.Value, currentCombo)
			}
			var err error
			if info, ok := n.build.matrixInfo[dep.Target]; ok {
				// Fan-out over combos that satisfy all constraints.
				preFanOut := len(n.deps)
				for ci, combo := range info.combos {
					if !comboMatchesConstraints(combo, constraints) {
						continue
					}
					internalName := comboTargetName(dep.Target, combo)
					if info.names != nil {
						internalName = info.names[ci]
					}
					var depNode *TargetNode
					if dep.Verb != "" {
						depNode, err = n.build.ResolveVerb(internalName, dep.Verb)
					} else {
						depNode, err = n.build.Resolve(internalName)
					}
					if err != nil {
						n.resolveErr = err
						return n.deps
					}
					n.deps = append(n.deps, depNode)
				}
				if len(n.deps) == preFanOut {
					n.resolveErr = fmt.Errorf("target %q: dep [%s @ %v] matched no combos", n.target, dep.Target, constraints)
					return n.deps
				}
			} else {
				// Non-matrix dep: resolve the exact combo-named target.
				internalName := comboTargetName(dep.Target, constraints)
				var depNode *TargetNode
				if dep.Verb != "" {
					depNode, err = n.build.ResolveVerb(internalName, dep.Verb)
				} else {
					depNode, err = n.build.Resolve(internalName)
				}
				if err != nil {
					n.resolveErr = err
					return n.deps
				}
				n.deps = append(n.deps, depNode)
			}
			continue
		}

		targets, err := n.build.expandDep(dep.Target)
		if err != nil {
			n.resolveErr = err
			return n.deps
		}
		for _, target := range targets {
			// Plain dep: resolve as-is. Matrix targets have an aggregator registered
			// under the base name, so this naturally depends on all combos.
			var depNode *TargetNode
			if dep.Verb != "" {
				depNode, err = n.build.ResolveVerb(target, dep.Verb)
			} else {
				depNode, err = n.build.Resolve(target)
			}
			if err != nil {
				n.resolveErr = err
				return n.deps
			}
			n.deps = append(n.deps, depNode)
		}
	}
	// Type-level computed deps from defbody dep clause. These augment whatever
	// explicit deps the rule already declared and become real DAG edges so verb
	// inheritance, -graph, and incremental rebuild all see them.
	if n.rule.Type != "" {
		if err := n.appendDefBodyDeps(); err != nil {
			n.resolveErr = err
			return n.deps
		}
	}
	if n.rule.Runner != "" {
		if err := n.appendRunnerDeps(); err != nil {
			n.resolveErr = err
			return n.deps
		}
	}
	return n.deps
}

// appendRunnerDeps resolves the deps a target acquires from its `on R` clause
// — the runner-type's defrunner dep clause (default: R itself) plus the
// synthetic runner setup node. Used by both the default-build and verb-rule
// dep-resolution paths so the rules see consistent edges.
func (n *TargetNode) appendRunnerDeps() error {
	runnerNode, err := n.build.Resolve(n.rule.Runner)
	if err != nil {
		return err
	}
	injected, err := n.build.runnerInjectedDepNames(n.rule.Runner)
	if err != nil {
		return err
	}
	for _, name := range injected {
		depNode, err := n.build.Resolve(name)
		if err != nil {
			return err
		}
		n.deps = append(n.deps, depNode)
	}
	// The synthetic runner setup node always fires so the runner's setup
	// phase can establish state (e.g. emit the skip sentinel) before the
	// consumer's body runs through the runner's run phase.
	n.deps = append(n.deps, n.build.runnerNode(runnerNode))
	return nil
}

// appendDefBodyDeps evaluates the dep clause of the build-verb defbody for
// this target's type (if any) and appends the resulting nodes to n.deps. Each
// dep token is run through bash with target options, $target, and ${dep[@]}
// (the explicit deps already resolved) bound; the output is word-split and
// each name resolved as a regular dep.
func (n *TargetNode) appendDefBodyDeps() error {
	tokens := n.build.defBodyDeps[defVerbBodyKey{n.rule.Type, ""}]
	if len(tokens) == 0 {
		return nil
	}
	explicit := make([]string, 0, len(n.deps))
	for _, d := range n.deps {
		explicit = append(explicit, d.target)
	}
	for _, tok := range tokens {
		names, err := n.build.expandDefBodyDep(tok, n.target, n.rule.Options, explicit)
		if err != nil {
			return fmt.Errorf("target %q: %w", n.target, err)
		}
		for _, name := range names {
			depNode, err := n.build.Resolve(name)
			if err != nil {
				return err
			}
			n.deps = append(n.deps, depNode)
		}
	}
	return nil
}

// verbDependencies resolves deps for a verb node.
//
//   - With ':' (HasDepSep, AugmentDeps=false): the explicit deps replace any
//     inherited list.
//   - With ':+' (HasDepSep, AugmentDeps=true): the explicit deps are followed
//     by the default rule's deps with the verb applied. Lets a verb rule add
//     a few extras to whatever the target's normal deps are.
//   - With no separator: the default rule's deps are inherited, verb-applied.
//
// Runner deps come only from the verb rule's own `on` clause, never from the
// default rule's runner. The runner is build infrastructure shared across many
// targets; auto-propagating verb-on-runner from the default rule causes
// surprise (and races, when consumers and the runner-verb both end up in the
// same DAG). Users who want a runner verb-applied as part of an aggregator
// should list it explicitly via ':+' or in the relevant deps.
func (n *TargetNode) verbDependencies() []*TargetNode {
	if n.rule != nil && n.rule.HasDepSep {
		for _, dep := range n.rule.Deps {
			// Explicit combo specifier: [target @ k=v ...], values may contain $var.
			if len(dep.Combo) > 0 {
				currentCombo := n.build.matrixVars[n.target]
				constraints := make(matrixCombo, len(dep.Combo))
				for _, opt := range dep.Combo {
					constraints[opt.Key] = substituteMatrixVars(opt.Value, currentCombo)
				}
				var err error
				if info, ok := n.build.matrixInfo[dep.Target]; ok {
					preFanOut := len(n.deps)
					for ci, combo := range info.combos {
						if !comboMatchesConstraints(combo, constraints) {
							continue
						}
						internalName := comboTargetName(dep.Target, combo)
						if info.names != nil {
							internalName = info.names[ci]
						}
						var depNode *TargetNode
						if dep.Verb != "" {
							depNode, err = n.build.ResolveVerb(internalName, dep.Verb)
						} else {
							depNode, err = n.build.Resolve(internalName)
						}
						if err != nil {
							n.resolveErr = err
							return n.deps
						}
						n.deps = append(n.deps, depNode)
					}
					if len(n.deps) == preFanOut {
						n.resolveErr = fmt.Errorf("target %q: dep [%s @ %v] matched no combos", n.target, dep.Target, constraints)
						return n.deps
					}
				} else {
					internalName := comboTargetName(dep.Target, constraints)
					var depNode *TargetNode
					if dep.Verb != "" {
						depNode, err = n.build.ResolveVerb(internalName, dep.Verb)
					} else {
						depNode, err = n.build.Resolve(internalName)
					}
					if err != nil {
						n.resolveErr = err
						return n.deps
					}
					n.deps = append(n.deps, depNode)
				}
				continue
			}

			targets, err := n.build.expandDep(dep.Target)
			if err != nil {
				n.resolveErr = err
				return n.deps
			}
			for _, target := range targets {
				// Plain dep: resolve as-is. Matrix targets have an aggregator registered
				// under the base name, so this naturally depends on all combos.
				var depNode *TargetNode
				if dep.Verb != "" {
					depNode, err = n.build.ResolveVerb(target, dep.Verb)
				} else {
					depNode, err = n.build.Resolve(target)
				}
				if err != nil {
					n.resolveErr = err
					return n.deps
				}
				n.deps = append(n.deps, depNode)
			}
		}
		if n.rule.AugmentDeps {
			n.deps = append(n.deps, n.inheritedVerbDeps()...)
		}
		if n.rule.Runner != "" {
			if err := n.appendRunnerDeps(); err != nil {
				n.resolveErr = err
				return n.deps
			}
		}
		return n.deps
	}

	n.deps = append(n.deps, n.inheritedVerbDeps()...)
	if n.rule != nil && n.rule.Runner != "" {
		if err := n.appendRunnerDeps(); err != nil {
			n.resolveErr = err
			return n.deps
		}
	}
	return n.deps
}

// inheritedVerbDeps returns the default rule's deps with this node's verb
// applied to each. Used both for no-colon inheritance and for ':+' augment.
//
// When the default rule has `on R` and the verb-rule's own runner doesn't
// match R, [verb R] is appended too — so cleaning a target that was built on
// an image also reaches the image's verb. The own-runner-matches-default case
// is skipped because the verb body is already running in R; the user chose
// that, and adding [verb R] there would cycle against R's after-consumers
// order edge.
func (n *TargetNode) inheritedVerbDeps() []*TargetNode {
	defaultRule := n.build.concretes[n.target]
	if defaultRule == nil {
		// Try to instantiate a pattern rule first.
		n.build.Resolve(n.target) //nolint — side effect: populates concretes
		defaultRule = n.build.concretes[n.target]
	}
	if defaultRule == nil {
		return nil
	}
	var deps []*TargetNode
	var explicitNames []string
	for _, dep := range defaultRule.Deps {
		targets, err := n.build.expandDep(dep.Target)
		if err != nil {
			n.resolveErr = err
			return deps
		}
		explicitNames = append(explicitNames, targets...)
		for _, target := range targets {
			depNode, err := n.build.ResolveVerb(target, n.verb)
			if err != nil {
				n.resolveErr = err
				return deps
			}
			deps = append(deps, depNode)
		}
	}
	// Type-level computed deps from the defbody dep clause inherit verb-applied,
	// just like explicit deps. Evaluating here (rather than reading from a
	// separately-resolved default node) avoids ordering/state coupling.
	if defaultRule.Type != "" {
		tokens := n.build.defBodyDeps[defVerbBodyKey{defaultRule.Type, ""}]
		for _, tok := range tokens {
			names, err := n.build.expandDefBodyDep(tok, n.target, defaultRule.Options, explicitNames)
			if err != nil {
				n.resolveErr = err
				return deps
			}
			for _, name := range names {
				depNode, err := n.build.ResolveVerb(name, n.verb)
				if err != nil {
					n.resolveErr = err
					return deps
				}
				deps = append(deps, depNode)
			}
		}
	}
	if defaultRule.Runner != "" && (n.rule == nil || n.rule.Runner != defaultRule.Runner) {
		runnerVerb, err := n.build.ResolveVerb(defaultRule.Runner, n.verb)
		if err != nil {
			n.resolveErr = err
			return deps
		}
		deps = append(deps, runnerVerb)
	}
	return deps
}

// OrderDependencies returns order-only deps for this node — edges that
// constrain scheduling but don't pull nodes into the DAG. The dag library
// honors them only when the referenced node is independently in the graph.
//
// Two cases produce order-only edges:
//
//   - This node is `[verb T]` where T has order=after-consumers for verb.
//     The verb-on-runner runs after every consumer whose body actually
//     executes inside T — i.e., default rules with `on T` (the bare build
//     node) and verb rules with their own `on T` clause (the [r.Verb r.Target]
//     node). Inherited verb nodes whose body runs locally are NOT consumers
//     of T under the new "verbs don't inherit `on`" model.
//
//   - This node is `[verb T]` where T has its own runner R (n.rule.Runner),
//     and R has order=before-consumers for verb. T runs after `[verb R]`.
//     Only fires for explicit-on verb rules; inherited verbs auto-propagate
//     [verb default.Runner] as a regular dep, which already provides the
//     "before" ordering through normal upstream edges.
func (n *TargetNode) OrderDependencies() []*TargetNode {
	if n.verb == "" || n.kind == kindRunner {
		return nil
	}
	var deps []*TargetNode

	// Case 1: this is [verb T] with order=after-consumers. Walk both maps
	// and add the consumer node itself (not [n.verb consumer]) so the order
	// edge points at whichever node actually executes inside T.
	if order := n.build.effectiveVerbOption(n.target, n.verb, "order"); order == "after-consumers" {
		for _, r := range n.build.concretes {
			if r.Runner == n.target {
				cn, err := n.build.Resolve(r.Target)
				if err == nil {
					deps = append(deps, cn)
				}
			}
		}
		// Verb-rule consumers run inside T regardless of which verb they are
		// (e.g. [lint t] on T must finish before [clean T] removes the image).
		for _, r := range n.build.verbConcretes {
			if r.Runner == n.target {
				cn, err := n.build.ResolveVerb(r.Target, r.Verb)
				if err == nil {
					deps = append(deps, cn)
				}
			}
		}
	}

	// Case 2: this node has its own `on R` (body runs in R), and R has
	// order=before-consumers for this verb. Inherited verbs don't reach
	// here — n.rule.Runner is empty for those, and auto-propagation already
	// gives them a regular dep on [verb default.Runner].
	if n.rule != nil && n.rule.Runner != "" {
		if order := n.build.effectiveVerbOption(n.rule.Runner, n.verb, "order"); order == "before-consumers" {
			rn, err := n.build.ResolveVerb(n.rule.Runner, n.verb)
			if err == nil {
				deps = append(deps, rn)
			}
		}
	}
	return deps
}

// effectiveVerbOption returns the value of the named option for [verb target].
// Verb-rule options take precedence over the type's defbody options.
func (b *Build) effectiveVerbOption(target, verb, key string) string {
	if r, ok := b.verbConcretes[verbNodeKey{target, verb}]; ok {
		for _, opt := range r.Options {
			if opt.Key == key {
				return opt.Value
			}
		}
	}
	r, ok := b.concretes[target]
	if !ok {
		return ""
	}
	for _, opt := range b.userDefBodyOptions[defVerbBodyKey{r.Type, verb}] {
		if opt.Key == key {
			return opt.Value
		}
	}
	for _, opt := range gen.BuiltinDefBodyOptions[gen.DefBodyOptionsKey{Type: r.Type, Verb: verb}] {
		if opt.Key == key {
			return opt.Value
		}
	}
	return ""
}

// Date returns when the target artifact was last successfully built.
//
//   - Phony (no type): always returns time.Now() — downstream typed targets
//     will always see this dep as newer than themselves.
//   - file: returns the file's mtime, or zero if the file doesn't exist.
//   - image: returns the docker image's creation time, or zero if not found.
//   - user-defined (deftype): runs __mmk_type_<name>, parses stdout as a
//     timestamp (epoch seconds, epoch seconds with nanosecond fraction, or
//     RFC3339); non-zero exit returns zero time.
func (n *TargetNode) Date() (time.Time, error) {
	n.dateMu.Lock()
	defer n.dateMu.Unlock()
	if n.dateValid {
		return n.dateVal, n.dateErr
	}
	n.dateVal, n.dateErr = n.computeDate()
	n.dateValid = true
	return n.dateVal, n.dateErr
}

// invalidateDate clears the cached Date so the next Date() call re-evaluates
// against the artifact's current state. Called after Run() rewrites the
// artifact so a downstream consumer's NeedsRun sees the new mtime instead of
// the pre-rebuild value the cache otherwise holds. The dag executor closes
// step.done only after Run returns, so the invalidation (deferred in Run)
// happens-before any consumer's NeedsRun reads this node's Date.
func (n *TargetNode) invalidateDate() {
	n.dateMu.Lock()
	n.dateValid = false
	n.dateMu.Unlock()
}

func (n *TargetNode) computeDate() (time.Time, error) {
	if n.kind == kindRunner {
		// Runner nodes are pure setup. Zero time means they never look
		// "newer" than artifacts that depend on them.
		return time.Time{}, nil
	}
	switch n.rule.Type {
	case "":
		return time.Now(), nil
	case "file", "source":
		return n.fileTypeDate()
	default:
		return n.userTypeDate()
	}
}

// fileTypeDate returns the mtime of the target file, or zero time if the file
// does not exist. It is a fast path for the built-in "file" and "source" types
// that avoids spawning a bash subprocess.
func (n *TargetNode) fileTypeDate() (time.Time, error) {
	info, err := os.Stat(n.target)
	if err != nil {
		if os.IsNotExist(err) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	return info.ModTime(), nil
}

// NeedsRun reports whether the target needs to run.
// Phony targets (no type) always need to run.
// Typed targets compare their own Date() against each dependency's Date();
// if the artifact doesn't exist (zero Date) or any dep is newer, they run.
// Verb nodes always need to run (they are imperative actions, not artifacts).
// Returns an error if a date check fails (e.g. bash failed to spawn).
func (n *TargetNode) NeedsRun() (bool, error) {
	if n.build.IsCancelled() {
		return false, ErrCancelled
	}
	if n.verb != "" {
		return true, nil
	}
	if n.kind == kindRunner {
		// Runner nodes run setup at most once per build per runner target.
		n.build.runnerStatesMu.Lock()
		_, started := n.build.runnerStates[n.runnerFor.target]
		n.build.runnerStatesMu.Unlock()
		return !started, nil
	}
	if n.rule.Type == "" {
		return true, nil
	}
	myDate, err := n.Date()
	if err != nil {
		return false, fmt.Errorf("date check for %q: %w", n.target, err)
	}
	if myDate.IsZero() {
		return true, nil // artifact doesn't exist yet
	}
	for _, dep := range n.deps {
		depDate, err := dep.Date()
		if err != nil {
			return false, fmt.Errorf("date check for dep %q of %q: %w", dep.target, n.target, err)
		}
		if depDate.After(myDate) {
			return true, nil
		}
	}
	return false, nil
}

// userTypeDate runs the deftype bash function for this node's type and parses
// its stdout as a timestamp (epoch seconds, epoch seconds with a nanosecond
// fraction matching GNU `stat -c %.Y`, or RFC3339/RFC3339Nano).
// Non-zero exit means the artifact is absent (returns zero time, nil error).
// Failure to spawn bash or parse output is a real error.
func (n *TargetNode) userTypeDate() (time.Time, error) {
	out, err := n.runBashOutput(gen.TypeFunc(n.rule.Type))
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return time.Time{}, nil // non-zero exit = artifact absent
		}
		return time.Time{}, err
	}
	return parseTimestamp(strings.TrimSpace(out))
}

// parseTimestamp parses s as epoch seconds (all digits, e.g. "1780111599"),
// epoch seconds with a fractional component (e.g. "1780111599.362179295",
// matching GNU `stat -c %.Y`), or RFC3339/RFC3339Nano.
func parseTimestamp(s string) (time.Time, error) {
	if isAllDigits(s) {
		epoch, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse epoch %q: %w", s, err)
		}
		return time.Unix(epoch, 0), nil
	}
	if i := strings.IndexByte(s, '.'); i > 0 && i < len(s)-1 {
		intPart, fracPart := s[:i], s[i+1:]
		if isAllDigits(intPart) && isAllDigits(fracPart) {
			secs, err := strconv.ParseInt(intPart, 10, 64)
			if err == nil {
				// Normalize fracPart to exactly 9 digits (nanoseconds).
				if len(fracPart) > 9 {
					fracPart = fracPart[:9]
				} else if len(fracPart) < 9 {
					fracPart = fracPart + strings.Repeat("0", 9-len(fracPart))
				}
				nanos, err := strconv.ParseInt(fracPart, 10, 64)
				if err == nil {
					return time.Unix(secs, nanos), nil
				}
			}
		}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("cannot parse timestamp %q (want epoch seconds, epoch.nanos, or RFC3339)", s)
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// wrapExecute wraps body in a self-contained bash snippet:
// "__mmk_exec() { BODY }; __mmk_exec". The wrapper preserves `return` semantics
// and lets runners execute the body with `eval "$MMK_EXECUTE"` without needing
// to know about function naming.
//
// `set -x` for verbose mode is enabled inside the function so the trace starts
// at the user's first command rather than the eval/define noise. Setting it
// after the function call would be too late; setting it before would trace the
// eval boundary itself, which is mmk infrastructure the user doesn't care about.
func wrapExecute(body string) string {
	return `__mmk_exec() { [ -n "${MMK_VERBOSE:-}" ] && set -x;` + body + "}; __mmk_exec"
}

// nonVerbBody returns the bash body for a non-verb node: the explicit body, a
// call to the type's default function, or empty (phony no-op).
func (n *TargetNode) nonVerbBody() string {
	if n.rule != nil && n.rule.Body != "" {
		return n.rule.Body
	}
	if n.rule != nil && n.rule.Type != "" && n.build.defBodies[n.rule.Type] {
		return "\n\t" + gen.DefaultFunc(n.rule.Type) + "\n"
	}
	return ""
}

// verbBody returns the bash body for a verb node: the explicit body, a call to
// the type's default verb function, or empty string meaning no-op.
func (n *TargetNode) verbBody() string {
	if n.rule != nil && n.rule.Body != "" {
		return n.rule.Body
	}
	defaultRule := n.build.concretes[n.target]
	if defaultRule != nil && defaultRule.Type != "" {
		key := defVerbBodyKey{defaultRule.Type, n.verb}
		if n.build.defVerbBodies[key] {
			return "\n\t" + gen.DefaultVerbFunc(defaultRule.Type, n.verb) + "\n"
		}
	}
	return ""
}

// executeScript returns a self-contained bash snippet for MMK_EXECUTE and
// whether there is anything to run. Returns (_, false) when there is no body.
func (n *TargetNode) executeScript() (string, bool) {
	if n.verb == "" {
		body := n.nonVerbBody()
		if body == "" {
			return "", false
		}
		return wrapExecute(gen.NormalizeBody(body)), true
	}
	body := n.verbBody()
	if body == "" {
		return "", false
	}
	return wrapExecute(gen.NormalizeBody(body)), true
}

// Run executes the target's body. For kindRunner it runs the setup phase of
// the runner type. If the rule has an `on` runner, the body is dispatched
// through the runner's run function. Otherwise it runs the body locally.
func (n *TargetNode) Run() error {
	// Drop the cached Date once the body finishes — the artifact may have
	// been rewritten, and a stale cache is what causes the cascade-after-
	// in-run-rebuild skip. See invalidateDate's comment.
	defer n.invalidateDate()
	if n.resolveErr != nil {
		return n.resolveErr
	}
	if n.build.IsCancelled() {
		return ErrCancelled
	}
	if n.kind == kindRunner {
		return n.runnerSetup()
	}
	execute, ok := n.executeScript()
	if !ok {
		return nil
	}
	runner := ""
	if n.rule != nil {
		runner = n.rule.Runner
	}
	if runner != "" {
		return n.runWithRunner(execute)
	}
	return n.runBash(execute, true)
}

// runWithRunner executes the given snippet through the runner type's run bash
// function. The snippet and task context are passed as environment variables.
func (n *TargetNode) runWithRunner(execute string) error {
	if n.build.IsCancelled() {
		return ErrCancelled
	}
	runnerNode, err := n.build.Resolve(n.rule.Runner)
	if err != nil {
		return err
	}
	runnerType := runnerNode.rule.Type

	n.build.runnerStatesMu.Lock()
	state := n.build.runnerStates[runnerNode.target]
	n.build.runnerStatesMu.Unlock()

	script := `. "$MMK_GENFILE"; ` + gen.RunnerRunFunc(runnerType)
	cmd := exec.Command("bash", "-c", script)
	// See Build.SubprocessPgroups for the rationale: tree-kill via
	// SignalAll(-pgid, sig) needs the subprocess to be its own pgroup
	// leader, but Setpgid breaks the interactive-Ctrl+C cascade. The
	// caller picks via the flag.
	if n.build.SubprocessPgroups {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	// For verb nodes, the default rule's options are layered in below the
	// verb-rule's own options (matches the verb-body inheritance applied in
	// runBash for the non-runner path). The combined set determines both
	// the env passed to the runner script and the MMK_RULE_OPT_KEYS list
	// runner scripts use to forward values into the body's exec environment.
	var defaultRule *parse.TargetRule
	if n.verb != "" {
		defaultRule = n.build.concretes[n.target]
	}

	cmd.Env = append(os.Environ(),
		"MMK_GENFILE="+n.build.genPath,
		envSuppressFailureSummary+"=1",
		"target="+runnerNode.target,
		"deps="+strings.Join(runnerNode.explicitDepNames(), " "),
		"MMK_RUNNER_STATE="+state,
		"MMK_EXECUTE="+execute,
		"MMK_TARGET="+n.target,
		"MMK_DEPS="+strings.Join(n.explicitDepNames(), " "),
		// Consumer rule's option keys, space-separated. Runner scripts that
		// exec the body in a separated environment (the built-in image
		// runner's `docker exec` is the canonical case) iterate this list to
		// forward the corresponding values. Without this, rule options like
		// `c_library libfoo.a source=./foo : on someimage` are visible in
		// the runner's bash env but never reach the body's bash inside the
		// container.
		"MMK_RULE_OPT_KEYS="+mergedRuleOptionKeys(defaultRule, n.rule),
	)
	if n.build.Verbose {
		cmd.Env = append(cmd.Env, "MMK_VERBOSE=1")
	}
	// Image (runner) options first; then default-rule options (verb only);
	// then target/verb options. On collision Go's os/exec resolves duplicate
	// keys by last-write-wins, so target overrides default, default
	// overrides runner.
	cmd.Env = appendRuleOptions(cmd.Env, runnerNode.rule)
	if defaultRule != nil {
		cmd.Env = appendRuleOptions(cmd.Env, defaultRule)
	}
	cmd.Env = appendRuleOptions(cmd.Env, n.rule)
	cmd.Env = appendMatrixVars(cmd.Env, n.build.matrixVars[n.target])
	stdout, stderr := n.bodyWriters()
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Forward host stdin only when the rule (or its runner image) opts in via
	// tty=true. Default is /dev/null, so docker exec without -t doesn't try to
	// read the host terminal. This avoids parallel runner tasks fighting over
	// terminal state, and keeps interactive shells (tty=true) working.
	if n.build.OutputWriter == nil && (ttyEnabled(n.rule) || ttyEnabled(runnerNode.rule)) {
		cmd.Stdin = os.Stdin
	}
	n.build.registerCmd(cmd)
	defer n.build.unregisterCmd(cmd)
	return cmd.Run()
}

// ttyEnabled reports whether the rule has a truthy tty= option. A missing or
// falsy (0/false/no/empty) value returns false; anything else is truthy.
func ttyEnabled(rule *parse.TargetRule) bool {
	if rule == nil {
		return false
	}
	for _, opt := range rule.Options {
		if opt.Key != "tty" {
			continue
		}
		switch opt.Value {
		case "", "0", "false", "no":
			return false
		default:
			return true
		}
	}
	return false
}

// runnerSetup runs the setup phase (if any) for the runner type associated
// with this kindRunner node. The setup function's stdout is captured and stored
// as the runner state, which is passed to subsequent run and cleanup calls.
func (n *TargetNode) runnerSetup() error {
	runnerNode := n.runnerFor
	runnerType := runnerNode.rule.Type

	info := n.build.runnerDefs[runnerType]
	if !info.hasSetup {
		// No setup phase: record empty state so NeedsRun returns false next time.
		n.build.runnerStatesMu.Lock()
		n.build.runnerStates[runnerNode.target] = ""
		n.build.runnerStatesMu.Unlock()
		return nil
	}

	out, err := runnerNode.runBashOutput(gen.RunnerSetupFunc(runnerType))
	if err != nil {
		return fmt.Errorf("runner setup for %q: %w", runnerNode.target, err)
	}

	n.build.runnerStatesMu.Lock()
	n.build.runnerStates[runnerNode.target] = strings.TrimSpace(out)
	n.build.runnerStatesMu.Unlock()
	return nil
}

// explicitDepNames returns the dep names a body should see in `$deps` /
// `${dep[@]}`: the rule's explicit deps plus any names produced by the type's
// defbody dep clause. Implicit deps (runner target, container node) that
// Dependencies() appends are excluded — they're build infrastructure, not
// content the body asked for.
func (n *TargetNode) explicitDepNames() []string {
	if n.rule == nil {
		return nil
	}
	var names []string
	for _, dep := range n.rule.Deps {
		expanded, err := n.build.expandDep(dep.Target)
		if err != nil {
			names = append(names, dep.Target)
			continue
		}
		names = append(names, expanded...)
	}
	// Append type-computed deps (defbody dep clause). These are real DAG edges
	// and so should be visible to the body the same way explicit deps are.
	if n.rule.Type != "" {
		tokens := n.build.defBodyDeps[defVerbBodyKey{n.rule.Type, ""}]
		for _, tok := range tokens {
			computed, err := n.build.expandDefBodyDep(tok, n.target, n.rule.Options, names)
			if err != nil {
				continue
			}
			names = append(names, computed...)
		}
	}
	return names
}

// expandDep returns the target names a dep resolves to. Tokens that start
// with '$' are expanded via bash; the result is word-split, so a variable
// holding multiple space-separated names produces multiple deps.
func (b *Build) expandDep(dep string) ([]string, error) {
	return b.expandToken(dep, "dep")
}

// runnerInjectedDepNames returns the names that should be added to a target's
// dep list as a consequence of `on runnerTarget`. The result is determined by
// the runner type's defrunner dep clause:
//
//   - No clause registered for the type → return just the runner target name
//     (historical default — `on R` adds R as a dep).
//   - Clause registered → evaluate each token in bash with the runner's
//     options and $target=runnerTarget in scope; word-split stdout.
//
// Evaluation is memoized per runnerTarget — multiple consumers pointing at the
// same runner instance produce the same dep set, so the bash subprocess only
// runs once.
func (b *Build) runnerInjectedDepNames(runnerTarget string) ([]string, error) {
	if cached, ok := b.defRunnerDepsCache[runnerTarget]; ok {
		return cached, nil
	}
	rule := b.concretes[runnerTarget]
	if rule == nil {
		// Pattern-defined or otherwise not yet materialized — fall back to default.
		b.defRunnerDepsCache[runnerTarget] = []string{runnerTarget}
		return b.defRunnerDepsCache[runnerTarget], nil
	}
	tokens, ok := b.defRunnerDeps[rule.Type]
	if !ok {
		// Type didn't register a dep clause: historical default.
		out := []string{runnerTarget}
		b.defRunnerDepsCache[runnerTarget] = out
		return out, nil
	}
	if len(tokens) == 0 {
		// `defrunner T : { ... }` — explicit empty clause means no deps.
		b.defRunnerDepsCache[runnerTarget] = nil
		return nil, nil
	}
	var names []string
	for _, tok := range tokens {
		got, err := b.expandDefRunnerDep(tok, runnerTarget, rule.Options)
		if err != nil {
			return nil, fmt.Errorf("runner %q (type %q): %w", runnerTarget, rule.Type, err)
		}
		names = append(names, got...)
	}
	b.defRunnerDepsCache[runnerTarget] = names
	return names, nil
}

// expandDefRunnerDep evaluates a single defrunner dep clause token. Parallel
// to expandDefBodyDep: sources the genfile, sets `$target` to the runner
// instance's name, binds the runner's options as bash variables, then echoes
// the token. The output is word-split.
func (b *Build) expandDefRunnerDep(token, target string, options []parse.Option) ([]string, error) {
	var script strings.Builder
	script.WriteString(`. "$MMK_GENFILE"` + "\n")
	script.WriteString("target=" + shellQuote(target) + "\n")
	for _, opt := range options {
		script.WriteString(opt.Key + "=" + shellQuote(opt.Value) + "\n")
	}
	script.WriteString("echo " + token + "\n")
	cmd := exec.Command("bash", "-c", script.String())
	cmd.Env = append(os.Environ(), "MMK_GENFILE="+b.genPath)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("expand defrunner dep %q: %w", token, err)
	}
	return strings.Fields(string(out)), nil
}

// expandDefBodyDep evaluates a defbody dep clause token. Unlike expandDep, the
// evaluation environment includes per-target options as bash variables (so
// `$source` etc. resolve), plus `$target` and `${dep[@]}` (the explicit deps
// already accumulated). Any token (not just $-prefixed) is run through bash so
// command substitutions like `$(find ...)` are interpreted.
func (b *Build) expandDefBodyDep(token, target string, options []parse.Option, explicitDeps []string) ([]string, error) {
	var script strings.Builder
	script.WriteString(`. "$MMK_GENFILE"` + "\n")
	script.WriteString("target=" + shellQuote(target) + "\n")
	script.WriteString("dep=(")
	for i, d := range explicitDeps {
		if i > 0 {
			script.WriteByte(' ')
		}
		script.WriteString(shellQuote(d))
	}
	script.WriteString(")\n")
	for _, opt := range options {
		script.WriteString(opt.Key + "=" + shellQuote(opt.Value) + "\n")
	}
	script.WriteString("echo " + token + "\n")

	cmd := exec.Command("bash", "-c", script.String())
	cmd.Env = append(os.Environ(), "MMK_GENFILE="+b.genPath)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("expand defbody dep %q: %w", token, err)
	}
	return strings.Fields(string(out)), nil
}

// shellQuote wraps a value in single quotes, escaping any embedded single
// quotes for safe inclusion in a generated bash script.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// expandToken evaluates a single token by sourcing the genfile in bash and
// echoing the token, then word-splitting the output. Tokens that don't start
// with '$' are returned as-is in a single-element slice. Used for dep names,
// target names, and runner references.
func (b *Build) expandToken(token, kind string) ([]string, error) {
	if !strings.HasPrefix(token, "$") {
		return []string{token}, nil
	}
	cmd := exec.Command("bash", "-c", `. "$MMK_GENFILE"; echo `+token)
	cmd.Env = append(os.Environ(), "MMK_GENFILE="+b.genPath)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("expand %s %q: %w", kind, token, err)
	}
	names := strings.Fields(string(out))
	if len(names) == 0 {
		return nil, fmt.Errorf("expand %s %q: empty expansion", kind, token)
	}
	return names, nil
}

// expandRuleNames mutates each TargetRule in f, expanding $VAR references in
// its concrete target name, runner clause, and option values using bash.
// Pattern targets are left untouched (their string is a regex). Target and
// Runner must expand to exactly one word; option values pass through verbatim
// (spaces preserved) and are only expanded if they contain a '$'.
func (b *Build) expandRuleNames(f *parse.File) error {
	for _, d := range f.Directives {
		r, ok := d.(*parse.TargetRule)
		if !ok {
			continue
		}
		if r.Pattern == "" && strings.HasPrefix(r.Target, "$") {
			names, err := b.expandToken(r.Target, "target name")
			if err != nil {
				return err
			}
			if len(names) != 1 {
				return fmt.Errorf("target name %q expanded to %d words; must be exactly one", r.Target, len(names))
			}
			r.Target = names[0]
		}
		if strings.HasPrefix(r.Runner, "$") {
			names, err := b.expandToken(r.Runner, "runner")
			if err != nil {
				return err
			}
			if len(names) != 1 {
				return fmt.Errorf("runner %q expanded to %d words; must be exactly one", r.Runner, len(names))
			}
			r.Runner = names[0]
		}
		for i, opt := range r.Options {
			if !strings.Contains(opt.Value, "$") {
				continue
			}
			expanded, err := b.expandOptionValue(opt.Value)
			if err != nil {
				return fmt.Errorf("expand option %s=%q: %w", opt.Key, opt.Value, err)
			}
			r.Options[i].Value = expanded
		}
	}
	return nil
}

// expandOptionValue runs an option value through bash so that $VAR and
// $(...) get expanded against passthrough state, then returns the result
// verbatim (no word-splitting). Used to make `key=./linux/$ARCH`-style
// values work — without this, the value is bound to bash with single quotes
// and never expands.
func (b *Build) expandOptionValue(value string) (string, error) {
	cmd := exec.Command("bash", "-c", `. "$MMK_GENFILE"; printf '%s' `+value)
	cmd.Env = append(os.Environ(), "MMK_GENFILE="+b.genPath)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// comboTargetName returns the DAG name for a specific matrix combo.
// Keys are sorted alphabetically for determinism.
// Non-empty combo: "[base @ k=v k2=v2]"   e.g. "[build @ go=1.20 os=linux]"
// Empty combo:     base unchanged          (aggregator name, no suffix needed)
// Empty base:      "[@ k=v]"              (internal lookup key only)
func comboTargetName(base string, combo matrixCombo) string {
	if len(combo) == 0 {
		return base
	}
	keys := make([]string, 0, len(combo))
	for k := range combo {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteByte('[')
	sb.WriteString(base)
	sb.WriteString(" @ ")
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(combo[k])
	}
	sb.WriteByte(']')
	return sb.String()
}

// computeComboTargetNames substitutes matrix vars into the base target name
// for every combo in the set, then decides names for the whole set as a unit:
// if the substituted names are already pairwise-unique — and none of them
// collides with the literal, unsubstituted base itself, which the aggregator
// rule always keeps as its own name (e.g. a base like "bin/myapp-$goos-$arch"
// that embeds every matrix var) — they're used directly, so the DAG key and
// the body's $target are the same string, with no synthetic wrapping needed.
// If any collision exists (e.g. a base that doesn't vary, or only partially
// varies, across combos — including the degenerate single-combo case, where
// the substituted name would otherwise trivially look "unique" while still
// colliding with the aggregator), every combo in the set falls back to
// bracket-notation naming — uniformly, so a single matrix rule never mixes
// bare and bracketed names across its own combos.
func computeComboTargetNames(base string, combos []matrixCombo) []string {
	substituted := make([]string, len(combos))
	seen := map[string]int{base: 1} // aggregator reserves the literal base name
	for i, combo := range combos {
		substituted[i] = substituteMatrixVars(base, combo)
		seen[substituted[i]]++
	}
	unique := true
	for _, count := range seen {
		if count > 1 {
			unique = false
			break
		}
	}
	if unique {
		return substituted
	}
	names := make([]string, len(combos))
	for i, combo := range combos {
		names[i] = comboTargetName(substituted[i], combo)
	}
	return names
}

// comboTemplateName returns the display name for a matrix aggregator in -list,
// showing the dim names with blank values: "[base @ k= k2=]"
func comboTemplateName(base string, info *matrixRuleInfo) string {
	if len(info.combos) == 0 {
		return base
	}
	keys := make([]string, 0, len(info.combos[0]))
	for k := range info.combos[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteByte('[')
	sb.WriteString(base)
	sb.WriteString(" @ ")
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(k)
		sb.WriteByte('=')
	}
	sb.WriteByte(']')
	return sb.String()
}

// evalForExpr evaluates a bash expression from a ForClause by sourcing the
// generated script and echoing the expression, then word-splitting the output.
func (b *Build) evalForExpr(expr string) ([]string, error) {
	cmd := exec.Command("bash", "-c", `. "$MMK_GENFILE"; echo `+expr)
	cmd.Env = append(os.Environ(), "MMK_GENFILE="+b.genPath)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("eval for-expr %q: %w", expr, err)
	}
	vals := strings.Fields(string(out))
	if len(vals) == 0 {
		return nil, fmt.Errorf("for-expr %q expanded to empty list", expr)
	}
	return vals, nil
}

// substituteMatrixVars replaces $VAR references in s with values from combo.
// Only substitutes variables present in combo; other $VAR references are left alone.
func substituteMatrixVars(s string, combo matrixCombo) string {
	for k, v := range combo {
		s = strings.ReplaceAll(s, "$"+k, v)
	}
	return s
}

// expandMatrixRules processes all TargetRules with ForClauses, generating
// synthetic combo rules and an aggregator rule for each.
func (b *Build) expandMatrixRules(f *parse.File) error {
	var toAppend []parse.Directive
	// Track indices to replace in f.Directives (original matrix rules → aggregators)
	type replacement struct {
		idx  int
		rule *parse.TargetRule
	}
	var replacements []replacement

	for i, d := range f.Directives {
		r, ok := d.(*parse.TargetRule)
		if !ok || len(r.ForClauses) == 0 {
			continue
		}
		if r.Pattern != "" {
			return fmt.Errorf("matrix 'for' clauses are not supported on pattern rules")
		}
		if r.Verb != "" {
			return fmt.Errorf("matrix 'for' clauses are not supported directly on verb rules; declare the matrix on the base rule")
		}

		// Evaluate each ForClause to get its values.
		varNames := make([]string, len(r.ForClauses))
		varValues := make([][]string, len(r.ForClauses))
		for j, fc := range r.ForClauses {
			vals, err := b.evalForExpr(fc.Expr)
			if err != nil {
				return fmt.Errorf("target %q: for %s: %w", r.Target, fc.Var, err)
			}
			varNames[j] = fc.Var
			varValues[j] = vals
		}

		// Generate cross-product of all variable combinations.
		allCombos := crossProduct(varNames, varValues)

		// Apply excludes.
		var validCombos []matrixCombo
		for _, combo := range allCombos {
			if !matchesAnyExclude(combo, r.Excludes) {
				validCombos = append(validCombos, combo)
			}
		}
		if len(validCombos) == 0 {
			return fmt.Errorf("target %q: all matrix combinations excluded", r.Target)
		}

		// Generate one synthetic TargetRule per combo.
		names := computeComboTargetNames(r.Target, validCombos)

		// Store matrix metadata.
		info := &matrixRuleInfo{vars: varNames, combos: validCombos, names: names}
		b.matrixInfo[r.Target] = info

		aggDeps := make([]parse.Dep, 0, len(validCombos))
		for ci, combo := range validCombos {
			name := names[ci]
			b.matrixVars[name] = combo

			// Substitute matrix vars in runner and deps.
			runner := substituteMatrixVars(r.Runner, combo)

			var deps []parse.Dep
			for _, dep := range r.Deps {
				deps = append(deps, parse.Dep{
					Target: substituteMatrixVars(dep.Target, combo),
					Verb:   dep.Verb,
					Combo:  dep.Combo,
				})
			}

			synth := &parse.TargetRule{
				Type:        r.Type,
				Target:      name,
				Runner:      runner,
				HasDepSep:   r.HasDepSep,
				Options:     r.Options,
				Body:        r.Body,
				Description: r.Description,
				Deps:        deps,
			}
			toAppend = append(toAppend, synth)
			aggDeps = append(aggDeps, parse.Dep{Target: name})
		}

		// Build the aggregator rule (replaces the original).
		agg := &parse.TargetRule{
			Target:      r.Target,
			HasDepSep:   true,
			Deps:        aggDeps,
			Description: r.Description,
		}
		replacements = append(replacements, replacement{idx: i, rule: agg})
	}

	// Apply replacements.
	for _, rep := range replacements {
		f.Directives[rep.idx] = rep.rule
	}
	// Append combo rules.
	f.Directives = append(f.Directives, toAppend...)
	return nil
}

// hasGroupFeatures reports whether f uses any group-related features:
// declared groups, `into` clauses, or group projection deps.
func (b *Build) hasGroupFeatures(f *parse.File) bool {
	if len(b.declaredGroups) > 0 {
		return true
	}
	for _, d := range f.Directives {
		r, ok := d.(*parse.TargetRule)
		if !ok {
			continue
		}
		if len(r.Groups) > 0 {
			return true
		}
		for _, dep := range r.Deps {
			if len(dep.GroupDims) > 0 {
				return true
			}
		}
	}
	return false
}

// totalGroupMembers returns the total number of members across all groups.
// Used to detect fixed-point convergence.
func (b *Build) totalGroupMembers() int {
	n := 0
	for _, gd := range b.groups {
		n += len(gd.members)
	}
	return n
}

// validateGroups checks that all `into` references and group projection deps
// name a declared group.
func (b *Build) validateGroups(f *parse.File) error {
	for _, d := range f.Directives {
		r, ok := d.(*parse.TargetRule)
		if !ok {
			continue
		}
		for _, groupName := range r.Groups {
			if !b.declaredGroups[groupName] {
				return fmt.Errorf("target %q: `into %s`: group %q is not declared (add: group %s)", r.Target, groupName, groupName, groupName)
			}
		}
		for _, dep := range r.Deps {
			if len(dep.GroupDims) > 0 && !b.declaredGroups[dep.Target] {
				return fmt.Errorf("target %q: dep [%s @ %s]: group %q is not declared", r.Target, dep.Target, strings.Join(dep.GroupDims, " "), dep.Target)
			}
		}
	}
	return nil
}

// expandExplicitMatrixRules is like expandMatrixRules but skips rules that
// have any group projection deps (those are handled in expandGroupConsumers).
// It also propagates the Groups field to the aggregator rule.
func (b *Build) expandExplicitMatrixRules(f *parse.File) error {
	var toAppend []parse.Directive
	type replacement struct {
		idx  int
		rule *parse.TargetRule
	}
	var replacements []replacement

	for i, d := range f.Directives {
		r, ok := d.(*parse.TargetRule)
		if !ok || len(r.ForClauses) == 0 {
			continue
		}
		if r.Pattern != "" {
			return fmt.Errorf("matrix 'for' clauses are not supported on pattern rules")
		}
		if r.Verb != "" {
			return fmt.Errorf("matrix 'for' clauses are not supported directly on verb rules; declare the matrix on the base rule")
		}

		// Skip rules that have group projection deps — handled in expandGroupConsumers.
		hasGroupDep := false
		for _, dep := range r.Deps {
			if len(dep.GroupDims) > 0 {
				hasGroupDep = true
				break
			}
		}
		if hasGroupDep {
			continue
		}

		varNames := make([]string, len(r.ForClauses))
		varValues := make([][]string, len(r.ForClauses))
		for j, fc := range r.ForClauses {
			vals, err := b.evalForExpr(fc.Expr)
			if err != nil {
				return fmt.Errorf("target %q: for %s: %w", r.Target, fc.Var, err)
			}
			varNames[j] = fc.Var
			varValues[j] = vals
		}

		allCombos := crossProduct(varNames, varValues)
		var validCombos []matrixCombo
		for _, combo := range allCombos {
			if !matchesAnyExclude(combo, r.Excludes) {
				validCombos = append(validCombos, combo)
			}
		}
		if len(validCombos) == 0 {
			return fmt.Errorf("target %q: all matrix combinations excluded", r.Target)
		}

		names := computeComboTargetNames(r.Target, validCombos)

		info := &matrixRuleInfo{vars: varNames, combos: validCombos, names: names}
		b.matrixInfo[r.Target] = info

		aggDeps := make([]parse.Dep, 0, len(validCombos))
		for ci, combo := range validCombos {
			name := names[ci]
			b.matrixVars[name] = combo

			runner := substituteMatrixVars(r.Runner, combo)
			var deps []parse.Dep
			for _, dep := range r.Deps {
				deps = append(deps, parse.Dep{
					Target: substituteMatrixVars(dep.Target, combo),
					Verb:   dep.Verb,
					Combo:  dep.Combo,
				})
			}

			synth := &parse.TargetRule{
				Type:        r.Type,
				Target:      name,
				Runner:      runner,
				HasDepSep:   r.HasDepSep,
				Options:     r.Options,
				Body:        r.Body,
				Description: r.Description,
				Deps:        deps,
				// Do NOT propagate Groups to individual combo rules — groups are
				// registered from the aggregator's Groups field in registerGroupMembers.
			}
			toAppend = append(toAppend, synth)
			aggDeps = append(aggDeps, parse.Dep{Target: name})
		}

		// Aggregator retains Groups so registerGroupMembers can find them.
		agg := &parse.TargetRule{
			Target:      r.Target,
			HasDepSep:   true,
			Deps:        aggDeps,
			Description: r.Description,
			Groups:      r.Groups,
		}
		replacements = append(replacements, replacement{idx: i, rule: agg})
	}

	for _, rep := range replacements {
		f.Directives[rep.idx] = rep.rule
	}
	f.Directives = append(f.Directives, toAppend...)
	return nil
}

// registerGroupMembers scans f for TargetRules with Groups set and registers
// their combo members (or themselves, if non-matrix) into those groups.
func (b *Build) registerGroupMembers(f *parse.File) {
	for _, d := range f.Directives {
		r, ok := d.(*parse.TargetRule)
		if !ok || len(r.Groups) == 0 {
			continue
		}
		if info, ok := b.matrixInfo[r.Target]; ok {
			// Matrix rule: register each combo.
			for ci, combo := range info.combos {
				internalName := comboTargetName(r.Target, combo)
				if info.names != nil {
					internalName = info.names[ci]
				}
				for _, groupName := range r.Groups {
					gd := b.groups[groupName]
					if gd == nil {
						continue
					}
					// Avoid duplicates.
					if !groupHasMember(gd, internalName) {
						gd.members = append(gd.members, groupMember{
							internalName: internalName,
							combo:        combo,
						})
					}
				}
			}
		} else {
			// Non-matrix: register the target itself with empty combo.
			for _, groupName := range r.Groups {
				gd := b.groups[groupName]
				if gd == nil {
					continue
				}
				if !groupHasMember(gd, r.Target) {
					gd.members = append(gd.members, groupMember{
						internalName: r.Target,
						combo:        matrixCombo{},
					})
				}
			}
		}
	}
}

// groupHasMember reports whether gd already contains a member with the given internal name.
func groupHasMember(gd *groupData, internalName string) bool {
	for _, m := range gd.members {
		if m.internalName == internalName {
			return true
		}
	}
	return false
}

// createGroupAggregators creates or updates a synthetic aggregator TargetRule
// for each declared group, regardless of member count. A consumer that
// depends on a group shouldn't need to know or care how many producers (if
// any) currently register into it — that's the point of the group/consumer
// contract — so even a zero-member group gets a (zero-dep) aggregator.
// It replaces any existing aggregator for the group in f.Directives, or appends
// a new one if none exists.
func (b *Build) createGroupAggregators(f *parse.File) error {
	for groupName, gd := range b.groups {
		deps := make([]parse.Dep, len(gd.members))
		for i, m := range gd.members {
			deps[i] = parse.Dep{Target: m.internalName}
		}
		agg := &parse.TargetRule{
			Target:      groupName,
			HasDepSep:   true,
			Deps:        deps,
			Description: gd.description,
		}
		// Replace existing aggregator if present, or append new one.
		replaced := false
		for i, d := range f.Directives {
			if r, ok := d.(*parse.TargetRule); ok && r.Target == groupName && r.HasDepSep && r.Body == "" && len(r.ForClauses) == 0 && len(r.Groups) == 0 {
				f.Directives[i] = agg
				replaced = true
				break
			}
		}
		if !replaced {
			f.Directives = append(f.Directives, agg)
		}
	}
	return nil
}

// expandGroupConsumers expands rules that have at least one group projection dep.
// For each such rule, each group dep contributes one axis of distinct dim-tuples;
// all axes are cross-producted with any explicit for-clause combos.
// Rules whose group deps are not yet populated are skipped (deferred to the next
// fixed-point iteration). Use validateGroupConsumersExpanded after the loop to
// error on any that remain unexpanded.
func (b *Build) expandGroupConsumers(f *parse.File) error {
	var toAppend []parse.Directive
	type replacement struct {
		idx  int
		rule *parse.TargetRule
	}
	var replacements []replacement

	for i, d := range f.Directives {
		r, ok := d.(*parse.TargetRule)
		if !ok {
			continue
		}

		// Check if any dep is a group projection dep.
		hasGroupDep := false
		for _, dep := range r.Deps {
			if len(dep.GroupDims) > 0 {
				hasGroupDep = true
				break
			}
		}
		if !hasGroupDep {
			continue
		}
		if r.Pattern != "" {
			return fmt.Errorf("group projection deps are not supported on pattern rules")
		}
		if r.Verb != "" {
			return fmt.Errorf("group projection deps are not supported on verb rules")
		}

		// Compute explicit combos from ForClauses (may be empty → one "empty" combo).
		var explicitCombos []matrixCombo
		if len(r.ForClauses) > 0 {
			varNames := make([]string, len(r.ForClauses))
			varValues := make([][]string, len(r.ForClauses))
			for j, fc := range r.ForClauses {
				vals, err := b.evalForExpr(fc.Expr)
				if err != nil {
					return fmt.Errorf("target %q: for %s: %w", r.Target, fc.Var, err)
				}
				varNames[j] = fc.Var
				varValues[j] = vals
			}
			allCombos := crossProduct(varNames, varValues)
			for _, combo := range allCombos {
				if !matchesAnyExclude(combo, r.Excludes) {
					explicitCombos = append(explicitCombos, combo)
				}
			}
			if len(explicitCombos) == 0 {
				return fmt.Errorf("target %q: all matrix combinations excluded", r.Target)
			}
		} else {
			explicitCombos = []matrixCombo{{}}
		}

		// For each group projection dep, build:
		//   depEntries[di] — dim→members lookup index for dep resolution
		//   groupAxes[N]   — one axis of distinct dim-tuples per group projection dep
		// Members that lack any of the requested dims are silently excluded.
		type depEntry struct {
			dims  []string
			byKey map[string][]string // comboTargetName("", dimVals) → []internalName
		}
		depEntries := make([]depEntry, len(r.Deps))
		var groupAxes [][]matrixCombo
		skipRule := false
		for di, dep := range r.Deps {
			if len(dep.GroupDims) == 0 {
				continue
			}
			gd, ok := b.groups[dep.Target]
			if !ok {
				return fmt.Errorf("target %q: dep references unknown group %q", r.Target, dep.Target)
			}
			dims := dep.GroupDims
			byKey := make(map[string][]string)
			axisSet := make(map[string]matrixCombo)
			var axisSlice []matrixCombo
			for _, m := range gd.members {
				dimVals := make(matrixCombo, len(dims))
				allPresent := true
				for _, d := range dims {
					if v, has := m.combo[d]; has {
						dimVals[d] = v
					} else {
						allPresent = false
						break
					}
				}
				if !allPresent {
					// Member lacks one or more of the projected dims — silently exclude.
					continue
				}
				key := comboTargetName("", dimVals)
				byKey[key] = append(byKey[key], m.internalName)
				if _, exists := axisSet[key]; !exists {
					axisSet[key] = dimVals
					axisSlice = append(axisSlice, dimVals)
				}
			}
			depEntries[di] = depEntry{dims: dims, byKey: byKey}
			if len(axisSlice) == 0 {
				// Group has no qualifying members yet — defer to next iteration.
				skipRule = true
				break
			}
			groupAxes = append(groupAxes, axisSlice)
		}
		if skipRule {
			continue
		}

		// Cross-product: start with explicitCombos, then fold in each group axis.
		allCombos := make([]matrixCombo, len(explicitCombos))
		copy(allCombos, explicitCombos)
		for _, axis := range groupAxes {
			var next []matrixCombo
			for _, base := range allCombos {
				for _, ext := range axis {
					merged := make(matrixCombo, len(base)+len(ext))
					for k, v := range base {
						merged[k] = v
					}
					for k, v := range ext {
						merged[k] = v
					}
					next = append(next, merged)
				}
			}
			allCombos = next
		}

		// Generate synthetic rules, one per combined combo.
		aggDeps := make([]parse.Dep, 0, len(allCombos))
		for _, combo := range allCombos {
			name := comboTargetName(r.Target, combo)

			// Skip if already generated (idempotent for cascading).
			if _, exists := b.matrixVars[name]; exists {
				aggDeps = append(aggDeps, parse.Dep{Target: name})
				continue
			}

			b.matrixVars[name] = combo

			runner := substituteMatrixVars(r.Runner, combo)

			// Build deps for this combo.
			var deps []parse.Dep
			for di, dep := range r.Deps {
				if len(dep.GroupDims) == 0 {
					// Regular dep: substitute matrix vars.
					deps = append(deps, parse.Dep{
						Target: substituteMatrixVars(dep.Target, combo),
						Verb:   dep.Verb,
						Combo:  dep.Combo,
					})
				} else {
					// Group projection dep: resolve members matching this combo's group dims.
					de := depEntries[di]
					dimVals := make(matrixCombo, len(de.dims))
					for _, d := range de.dims {
						if v, has := combo[d]; has {
							dimVals[d] = v
						}
					}
					key := comboTargetName("", dimVals)
					for _, memberName := range de.byKey[key] {
						deps = append(deps, parse.Dep{Target: memberName})
					}
				}
			}

			synth := &parse.TargetRule{
				Type:        r.Type,
				Target:      name,
				Runner:      runner,
				HasDepSep:   r.HasDepSep || len(deps) > 0,
				Options:     r.Options,
				Body:        r.Body,
				Description: r.Description,
				Deps:        deps,
				Groups:      r.Groups, // propagate group membership for cascading
			}
			toAppend = append(toAppend, synth)
			aggDeps = append(aggDeps, parse.Dep{Target: name})
		}

		// Build the aggregator rule.
		// Store matrix info for the base rule.
		if len(r.ForClauses) > 0 || len(allCombos) > 1 || (len(allCombos) == 1 && len(allCombos[0]) > 0) {
			// Only set matrixInfo if we generated multiple combos or the combo has dims.
			combosForInfo := make([]matrixCombo, len(allCombos))
			copy(combosForInfo, allCombos)
			b.matrixInfo[r.Target] = &matrixRuleInfo{combos: combosForInfo}
		}

		agg := &parse.TargetRule{
			Target:      r.Target,
			HasDepSep:   true,
			Deps:        aggDeps,
			Description: r.Description,
			Groups:      r.Groups,
		}
		replacements = append(replacements, replacement{idx: i, rule: agg})
	}

	for _, rep := range replacements {
		f.Directives[rep.idx] = rep.rule
	}
	f.Directives = append(f.Directives, toAppend...)
	return nil
}

// validateGroupConsumersExpanded checks that no rules still have unexpanded group
// projection deps. Called after the fixed-point loop to catch groups that never
// received any members.
func (b *Build) validateGroupConsumersExpanded(f *parse.File) error {
	for _, d := range f.Directives {
		r, ok := d.(*parse.TargetRule)
		if !ok {
			continue
		}
		for _, dep := range r.Deps {
			if len(dep.GroupDims) == 0 {
				continue
			}
			gd := b.groups[dep.Target]
			dims := dep.GroupDims
			dimStr := strings.Join(dims, " ")
			if gd == nil || len(gd.members) == 0 {
				return fmt.Errorf("target %q: [%s @ %s]: group %q has no members",
					r.Target, dep.Target, dimStr, dep.Target)
			}
			// Group has members but none had all the requested dims together.
			// Collect which dims actually appear in the group to help diagnose.
			seenDims := make(map[string]bool)
			for _, m := range gd.members {
				for k := range m.combo {
					seenDims[k] = true
				}
			}
			missingDims := []string{}
			for _, d := range dims {
				if !seenDims[d] {
					missingDims = append(missingDims, d)
				}
			}
			if len(missingDims) > 0 {
				return fmt.Errorf("target %q: [%s @ %s]: no member of group %q has dimension(s) %s",
					r.Target, dep.Target, dimStr, dep.Target, strings.Join(missingDims, ", "))
			}
			// All dims exist in the group, but no single member has all of them.
			// This is the "disjoint dims" case: producers contribute separate dims.
			sep := make([]string, len(dims))
			for i, d := range dims {
				sep[i] = fmt.Sprintf("[%s @ %s]", dep.Target, d)
			}
			return fmt.Errorf("target %q: [%s @ %s]: no member of group %q has all dimensions %s together; "+
				"if producers contribute these dimensions separately, project each dim individually: %s",
				r.Target, dep.Target, dimStr, dep.Target, dimStr, strings.Join(sep, " "))
		}
	}
	return nil
}

// crossProduct generates the full cross-product of variable values.
func crossProduct(varNames []string, varValues [][]string) []matrixCombo {
	result := []matrixCombo{make(matrixCombo)}
	for i, vals := range varValues {
		var next []matrixCombo
		for _, existing := range result {
			for _, v := range vals {
				combo := make(matrixCombo, len(existing)+1)
				for k, val := range existing {
					combo[k] = val
				}
				combo[varNames[i]] = v
				next = append(next, combo)
			}
		}
		result = next
	}
	return result
}

// matchesAnyExclude reports whether combo matches any of the exclude patterns.
// A combo matches an exclude pattern if the combo's value for every key in the
// pattern equals the pattern's value for that key.
func matchesAnyExclude(combo matrixCombo, excludes [][]parse.Option) bool {
	for _, exc := range excludes {
		if matchesExclude(combo, exc) {
			return true
		}
	}
	return false
}

func matchesExclude(combo matrixCombo, exc []parse.Option) bool {
	for _, opt := range exc {
		if combo[opt.Key] != opt.Value {
			return false
		}
	}
	return true
}

// appendMatrixVars exports the matrix variable assignments for a combo node as
// environment variables. Returns env unchanged when combo is nil (non-matrix node).
func appendMatrixVars(env []string, combo matrixCombo) []string {
	for k, v := range combo {
		env = append(env, k+"="+v)
	}
	return env
}

// comboMatchesConstraints reports whether combo satisfies all constraints.
// Every key in constraints must be present in combo with the same value.
// Keys in combo not present in constraints are unconstrained (fan-out).
func comboMatchesConstraints(combo, constraints matrixCombo) bool {
	for k, v := range constraints {
		if dv, ok := combo[k]; !ok || dv != v {
			return false
		}
	}
	return true
}

// expandSubprojects processes each `subproject` directive in f. For each, it
// expands $VAR in the target/runner names, reads the subproject's mmkfile,
// harvests its top-level verbs, and appends synthetic TargetRule directives
// to f so the regular registration loop picks them up.
//
// The default-build rule's body is `(cd <path> && mmk)`. Each [verb T] rule's
// body is `(cd <path> && mmk <verb>)`. The runner clause is propagated.
//
// If the subproject's mmkfile is missing, this returns an error — `subproject`
// declares a contract that there's a sub-mmkfile to delegate to.
func (b *Build) expandSubprojects(f *parse.File) error {
	for _, d := range f.Directives {
		sp, ok := d.(*parse.Subproject)
		if !ok {
			continue
		}
		if strings.HasPrefix(sp.Target, "$") {
			names, err := b.expandToken(sp.Target, "subproject target")
			if err != nil {
				return err
			}
			if len(names) != 1 {
				return fmt.Errorf("subproject target %q expanded to %d words; must be exactly one", sp.Target, len(names))
			}
			sp.Target = names[0]
		}
		if strings.HasPrefix(sp.Runner, "$") {
			names, err := b.expandToken(sp.Runner, "subproject runner")
			if err != nil {
				return err
			}
			if len(names) != 1 {
				return fmt.Errorf("subproject runner %q expanded to %d words; must be exactly one", sp.Runner, len(names))
			}
			sp.Runner = names[0]
		}

		// Path defaults to target name; allow override via path= option.
		path := sp.Target
		for _, opt := range sp.Options {
			if opt.Key == "path" {
				path = opt.Value
			}
		}

		subFile, err := readSubMmkfile(path)
		if err != nil {
			return fmt.Errorf("subproject %q: %w", sp.Target, err)
		}

		b.subprojects[sp.Target] = &subprojectInfo{
			target: sp.Target,
			runner: sp.Runner,
			path:   path,
		}

		verbs := harvestVerbsRecursive(subFile, path)

		// Default-build rule. Use HasDepSep=true to suppress verb dep-inheritance.
		f.Directives = append(f.Directives, &parse.TargetRule{
			Target:      sp.Target,
			Runner:      sp.Runner,
			HasDepSep:   true,
			Body:        fmt.Sprintf("\n\t(cd %q && MMK_SUPPRESS_FAILURE_SUMMARY= mmk)\n", path),
			Description: sp.Description,
		})

		// One [verb target] rule per harvested verb.
		for _, verb := range verbs {
			f.Directives = append(f.Directives, &parse.TargetRule{
				Target:    sp.Target,
				Runner:    sp.Runner,
				Verb:      verb,
				HasDepSep: true,
				Body:      fmt.Sprintf("\n\t(cd %q && MMK_SUPPRESS_FAILURE_SUMMARY= mmk %s)\n", path, verb),
			})
		}
	}
	return nil
}

// ResolveSubpath checks whether `target` is of the form "<subproject>/<rest>"
// and, if so, registers a synthetic rule that delegates to the subproject by
// recursively invoking mmk in the sub-directory. Returns ok=true when a rule
// was registered (and the caller should continue with normal Resolve(target)).
//
// If `target` doesn't have a slash, or its prefix isn't a known subproject,
// returns ok=false and the caller falls through to normal lookup. Top-level
// targets that already exist take precedence — registering the same target
// twice is a duplicate, so we only synthesize when nothing is registered.
//
// Subproject names may themselves contain slashes (e.g. "cmd/bas"), so we
// find the longest-matching subproject prefix rather than splitting on the
// first slash.
func (b *Build) ResolveSubpath(target, verb string) bool {
	// Find the longest matching subproject name that is a prefix of target.
	var sp *subprojectInfo
	var suffix string
	for spTarget, info := range b.subprojects {
		prefix := spTarget + "/"
		if strings.HasPrefix(target, prefix) {
			rest := target[len(prefix):]
			if sp == nil || len(spTarget) > len(sp.target) {
				sp = info
				suffix = rest
			}
		}
	}
	if sp == nil {
		return false
	}
	if verb == "" {
		if _, exists := b.concretes[target]; exists {
			return false
		}
	} else {
		if _, exists := b.verbConcretes[verbNodeKey{target, verb}]; exists {
			return false
		}
	}
	body := fmt.Sprintf("\n\t(cd %q && MMK_SUPPRESS_FAILURE_SUMMARY= mmk %s)\n", sp.path, suffix)
	if verb != "" {
		body = fmt.Sprintf("\n\t(cd %q && MMK_SUPPRESS_FAILURE_SUMMARY= mmk %s %s)\n", sp.path, verb, suffix)
	}
	rule := &parse.TargetRule{
		Target:    target,
		Verb:      verb,
		Runner:    sp.runner,
		HasDepSep: true,
		Body:      body,
	}
	if verb != "" {
		b.verbConcretes[verbNodeKey{target, verb}] = rule
	} else {
		b.concretes[target] = rule
	}
	return true
}

// readSubMmkfile reads <path>/mmkfile or <path>/Mmkfile and returns the
// parsed AST with any `include` directives resolved relative to the
// sub-mmkfile's directory.
func readSubMmkfile(path string) (*parse.File, error) {
	for _, name := range []string{"mmkfile", "Mmkfile"} {
		p := filepath.Join(path, name)
		if _, err := os.Stat(p); err == nil {
			return parse.ParseFile(p)
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
	}
	return nil, fmt.Errorf("no mmkfile or Mmkfile found in %q", path)
}

// harvestVerbs walks a parsed sub-mmkfile and returns the unique sorted set
// of verb names declared in it: explicit [verb T] rules, defbody verbs, and
// (for any type the file references) the built-in defbody verbs for that type.
func harvestVerbs(f *parse.File) []string {
	verbs := make(map[string]bool)
	collectVerbsInto(verbs, f)
	out := make([]string, 0, len(verbs))
	for v := range verbs {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// harvestVerbsRecursive returns the union of verb names declared in f and in
// every subproject reachable from f. Needed at the parent level so that a
// verb declared deep in a sub-subproject (e.g. tools/json_to_hardcoded_fb_policies's
// `[update all]`) gets a top-level [verb tools] rule generated, which then
// delegates `cd tools && mmk verb` and lets the sub-mmk further delegate.
func harvestVerbsRecursive(f *parse.File, path string) []string {
	verbs := make(map[string]bool)
	collectVerbsInto(verbs, f)
	for _, d := range f.Directives {
		sp, ok := d.(*parse.Subproject)
		if !ok {
			continue
		}
		childPath := filepath.Join(path, sp.Target)
		for _, opt := range sp.Options {
			if opt.Key == "path" {
				childPath = filepath.Join(path, opt.Value)
			}
		}
		childFile, err := readSubMmkfile(childPath)
		if err != nil {
			continue // best-effort; missing/broken sub-mmkfile shouldn't break harvest
		}
		for _, v := range harvestVerbsRecursive(childFile, childPath) {
			verbs[v] = true
		}
	}
	out := make([]string, 0, len(verbs))
	for v := range verbs {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// collectVerbsInto adds verb names from f's direct rules and defbodies (plus
// any built-in verbs implied by used types) into the given set.
func collectVerbsInto(verbs map[string]bool, f *parse.File) {
	typesUsed := make(map[string]bool)
	for _, d := range f.Directives {
		switch d := d.(type) {
		case *parse.TargetRule:
			if d.Verb != "" {
				verbs[d.Verb] = true
			}
			if d.Type != "" {
				typesUsed[d.Type] = true
			}
		case *parse.DefBody:
			if d.Verb != "" {
				verbs[d.Verb] = true
			}
		}
	}
	for typeName := range typesUsed {
		for verb := range gen.BuiltinVerbBodies[typeName] {
			verbs[verb] = true
		}
	}
}

// runBash sources generated.sh and evaluates the MMK_EXECUTE snippet.
// If output is true, stdout/stderr are forwarded to the process's own streams.
// Target and dep names are passed via environment to avoid quoting issues.
// When Verbose is set, MMK_VERBOSE=1 is exported and the script enables
// `set -x` around the eval so the user sees the bash commands being run.
func (n *TargetNode) runBash(execute string, output bool) error {
	if n.build.IsCancelled() {
		return ErrCancelled
	}
	depsFile, err := writeDepsFile(n.explicitDepNames())
	if err != nil {
		return err
	}
	defer os.Remove(depsFile)
	// MMK_DEPS is populated from the deps file for any user body that
	// reads it directly, and exported so subshells in user bodies see it
	// (matching the pre-file behaviour where MMK_DEPS was inherited via
	// execve env). The file detour exists because Linux execve has a
	// per-string limit (~128 KiB on MAX_ARG_STRLEN) that matrix
	// aggregators can blow past via the env vector.
	script := `. "$MMK_GENFILE"; target="$MMK_TARGET"; MMK_DEPS="$(cat "$MMK_DEPSFILE")"; export MMK_DEPS; deps="$MMK_DEPS"; read -ra dep <<< "$deps"; eval "$MMK_EXECUTE"`
	c := exec.Command("bash", "-c", script)
	// See Build.SubprocessPgroups: own pgroup is required when the
	// caller drives cancellation via SignalAll; shared pgroup is
	// required for interactive terminal Ctrl+C to cascade.
	if n.build.SubprocessPgroups {
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	c.Env = append(os.Environ(),
		"MMK_GENFILE="+n.build.genPath,
		envSuppressFailureSummary+"=1",
		"MMK_TARGET="+n.target,
		"MMK_DEPSFILE="+depsFile,
		"MMK_EXECUTE="+execute,
	)
	if n.build.Verbose {
		c.Env = append(c.Env, "MMK_VERBOSE=1")
	}
	// Verb nodes inherit the default rule's options before any verb-rule
	// options are layered on. This means a target's `key=value` options
	// (e.g. `c_library libio source=./libio :`) are visible to every verb
	// body that runs against the target — clean, test, install, etc. — not
	// just the build body. Verb-rule options take precedence on collision.
	if n.verb != "" {
		if defaultRule := n.build.concretes[n.target]; defaultRule != nil {
			c.Env = appendRuleOptions(c.Env, defaultRule)
		}
	}
	c.Env = appendRuleOptions(c.Env, n.rule)
	c.Env = appendMatrixVars(c.Env, n.build.matrixVars[n.target])
	if output {
		stdout, stderr := n.bodyWriters()
		c.Stdout = stdout
		c.Stderr = stderr
		// Only forward stdin in interactive (default) mode. When OutputWriter is
		// set, we're running under the TUI: bubbletea owns the terminal, and
		// passing os.Stdin causes the inner bash to see [ -t 0 ] = true, which
		// makes the image runner allocate a docker PTY (-t) and emit terminal
		// control sequences interleaved with normal output.
		if n.build.OutputWriter == nil {
			c.Stdin = os.Stdin
		}
	}
	n.build.registerCmd(c)
	defer n.build.unregisterCmd(c)
	return c.Run()
}

// writeDepsFile writes the joined dep names to a temp file and returns its
// path. Callers must os.Remove the path when done. We pass deps via file
// (not env) because Linux's execve has a per-string limit (MAX_ARG_STRLEN,
// ~128 KiB) and matrix aggregators can produce dep lists much larger than
// that.
func writeDepsFile(names []string) (string, error) {
	f, err := os.CreateTemp("", "mmk-deps-*")
	if err != nil {
		return "", fmt.Errorf("mmk: create deps file: %w", err)
	}
	if _, err := f.WriteString(strings.Join(names, " ")); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("mmk: write deps file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("mmk: close deps file: %w", err)
	}
	return f.Name(), nil
}

// bodyWriters returns the (stdout, stderr) writers for this node's body
// execution. Defaults to os.Stdout/Stderr; the TUI overrides via OutputWriter
// to capture per-node output.
func (n *TargetNode) bodyWriters() (io.Writer, io.Writer) {
	if n.build.OutputWriter != nil {
		return n.build.OutputWriter(n.target, n.verb)
	}
	return os.Stdout, os.Stderr
}

// runBashOutput is like runBash but captures and returns stdout.
// Used for deftype functions that output a timestamp.
func (n *TargetNode) runBashOutput(cmd string) (string, error) {
	script := `. "$MMK_GENFILE"; target="$MMK_TARGET"; deps="$MMK_DEPS"; ` + cmd
	c := exec.Command("bash", "-c", script)
	c.Env = append(os.Environ(),
		"MMK_GENFILE="+n.build.genPath,
		"MMK_TARGET="+n.target,
		"MMK_DEPS="+strings.Join(n.explicitDepNames(), " "),
	)
	c.Env = appendRuleOptions(c.Env, n.rule)
	out, err := c.Output()
	return string(out), err
}

// appendRuleOptions exports the rule's key=value options as environment
// variables for any bash subprocess that runs one of the rule's bodies.
// Returns env unchanged when rule is nil or has no options.
func appendRuleOptions(env []string, rule *parse.TargetRule) []string {
	if rule == nil {
		return env
	}
	for _, opt := range rule.Options {
		env = append(env, opt.Key+"="+opt.Value)
	}
	return env
}

// ruleOptionKeys returns a space-separated list of the rule's option keys,
// suitable for shell iteration. Used to plumb consumer-rule option names
// into runner scripts that need to forward the values to a separate exec
// environment (e.g. docker exec).
func ruleOptionKeys(rule *parse.TargetRule) string {
	if rule == nil || len(rule.Options) == 0 {
		return ""
	}
	keys := make([]string, len(rule.Options))
	for i, opt := range rule.Options {
		keys[i] = opt.Key
	}
	return strings.Join(keys, " ")
}

// mergedRuleOptionKeys returns the union of option keys from defaultRule
// and overlay, defaultRule's keys first, deduped (overlay's are skipped if
// already present from default). This is the set of option-name env vars
// that runner scripts should forward into the body's exec environment when
// executing a verb whose rule (overlay) and the underlying default rule
// both contribute options.
func mergedRuleOptionKeys(defaultRule, overlay *parse.TargetRule) string {
	seen := make(map[string]bool)
	var keys []string
	collect := func(r *parse.TargetRule) {
		if r == nil {
			return
		}
		for _, opt := range r.Options {
			if seen[opt.Key] {
				continue
			}
			seen[opt.Key] = true
			keys = append(keys, opt.Key)
		}
	}
	collect(defaultRule)
	collect(overlay)
	return strings.Join(keys, " ")
}

// subprojectSummary captures what's in a (sub-)mmkfile for -list display.
type subprojectSummary struct {
	path               string               // path relative to the root invocation
	prefix             string               // accumulated subproject path, e.g. "src/lib"
	targets            []string             // concrete (non-pattern, non-verb) target names
	targetDescriptions map[string]string    // target name → description
	verbs              []string             // harvested verbs
	children           []*subprojectSummary // nested subprojects
}

// firstLine returns the first line of s, or s itself if there's no newline.
// Used for one-line summaries in tabular output.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// walkSubprojects descends into each direct subproject and returns a flat
// (depth-first) list of subprojectSummary entries — one per (sub)project.
// Sub-mmkfiles that fail to parse are skipped silently; -list shouldn't
// turn into a hard error just because a sub-mmkfile is malformed.
func (b *Build) walkSubprojects() []*subprojectSummary {
	var summaries []*subprojectSummary
	for _, sp := range b.subprojects {
		summaries = append(summaries, walkSubprojectTree(sp.path, sp.target)...)
	}
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].prefix < summaries[j].prefix
	})
	return summaries
}

func walkSubprojectTree(path, prefix string) []*subprojectSummary {
	f, err := readSubMmkfile(path)
	if err != nil {
		return nil
	}
	s := &subprojectSummary{path: path, prefix: prefix, targetDescriptions: make(map[string]string)}
	var nested []*subprojectSummary
	for _, d := range f.Directives {
		switch d := d.(type) {
		case *parse.TargetRule:
			if d.Pattern == "" && d.Verb == "" && d.Target != "" {
				s.targets = append(s.targets, d.Target)
				if d.Description != "" {
					s.targetDescriptions[d.Target] = d.Description
				}
			}
		case *parse.Subproject:
			childPath := filepath.Join(path, d.Target)
			for _, opt := range d.Options {
				if opt.Key == "path" {
					childPath = filepath.Join(path, opt.Value)
				}
			}
			childPrefix := prefix + "/" + d.Target
			nested = append(nested, walkSubprojectTree(childPath, childPrefix)...)
		}
	}
	s.verbs = harvestVerbs(f)
	sort.Strings(s.targets)
	out := []*subprojectSummary{s}
	out = append(out, nested...)
	return out
}

// PrintList renders a human-readable summary of targets and verbs to w.
// Targets are grouped into sections with a brief annotation per target
// (runner, type, or dep list for aggregators). Verbs are listed with the
// targets each one applies to.
//
// Subprojects appear inline in the Targets section: the subproject root is
// annotated as `subproject (<path>/mmkfile)`, and its child targets are
// listed below it with full path-from-root prefixes. Recursion (subprojects
// of subprojects) follows the same pattern.
func (b *Build) PrintList(w io.Writer, all bool) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "Targets:")
	subSummaries := b.walkSubprojects()
	subRoots := make(map[string]string) // prefix → "subproject (path/mmkfile)" annotation
	for _, s := range subSummaries {
		subRoots[s.prefix] = fmt.Sprintf("subproject (%s/mmkfile)", s.path)
	}
	// Each target gets a single info column: the user's docstring if any,
	// else the structural annotation (`on <runner>`, `type: ...`, `→ deps`).
	// Falling back keeps the listing concise — annotation is only visible
	// when the user hasn't supplied a more meaningful description.
	type entry struct {
		name, info string
		public     bool // visible by default; non-public hidden unless -all
	}
	pickInfo := func(desc, annot string) string {
		if desc != "" {
			return desc
		}
		return annot
	}
	// A target is "public" when it has a docstring or is the literal target
	// `all` (always shown so users see what `mmk` with no args will do).
	isPublic := func(name, desc string) bool {
		return desc != "" || name == "all"
	}
	entries := make([]entry, 0, len(b.concretes))
	for _, name := range b.Targets() {
		// Individual combo instances are not directly addressed by users;
		// they're represented by their aggregator's template name.
		if b.matrixVars[name] != nil {
			continue
		}
		r := b.concretes[name]
		desc := ""
		if r != nil {
			desc = firstLine(r.Description)
		}

		var displayName, annot string
		if info, ok := b.matrixInfo[name]; ok {
			// Matrix aggregator: use dim template as display name; the
			// template itself is already informative, so no dep annotation.
			displayName = comboTemplateName(name, info)
		} else if b.declaredGroups[name] {
			// Group aggregator: suppress the long member dep list.
			displayName = name
			n := 0
			if gd := b.groups[name]; gd != nil {
				n = len(gd.members)
			}
			annot = fmt.Sprintf("(group, %d members)", n)
		} else {
			displayName = name
			annot = annotateRule(r)
		}

		if subAnnot, ok := subRoots[name]; ok {
			if annot != "" {
				annot = subAnnot + ", " + annot
			} else {
				annot = subAnnot
			}
			delete(subRoots, name)
		}
		entries = append(entries, entry{displayName, pickInfo(desc, annot), isPublic(name, desc)})
	}
	// Sub-of-sub subproject roots (not in top-level concretes), and sub-targets.
	for _, s := range subSummaries {
		if annot, ok := subRoots[s.prefix]; ok {
			// Sub-of-sub root: no Description tracked at this layer; mark
			// non-public unless -all. Surfacing the sub-mmkfile via -list
			// requires a docstring on the parent's `subproject` directive.
			entries = append(entries, entry{s.prefix, annot, false})
		}
		for _, t := range s.targets {
			desc := firstLine(s.targetDescriptions[t])
			entries = append(entries, entry{
				name:   s.prefix + "/" + t,
				info:   desc,
				public: desc != "",
			})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	hiddenTargets := 0
	for _, e := range entries {
		if !all && !e.public {
			hiddenTargets++
			continue
		}
		fmt.Fprintf(tw, "  %s\t%s\n", e.name, e.info)
	}
	tw.Flush()

	// Patterns: filter to docstring-having unless -all. Hide the whole
	// section (header included) when there's nothing to show.
	type patternEntry struct {
		pattern, desc string
		public        bool
	}
	var patterns []patternEntry
	for _, pe := range b.patterns {
		if pe.rule.Verb != "" {
			continue // verb-pattern rules surface in the Verbs section, not here
		}
		desc := firstLine(pe.rule.Description)
		patterns = append(patterns, patternEntry{
			pattern: pe.rule.Pattern,
			desc:    desc,
			public:  desc != "",
		})
	}
	sort.Slice(patterns, func(i, j int) bool { return patterns[i].pattern < patterns[j].pattern })
	hiddenPatterns := 0
	visiblePatterns := make([]patternEntry, 0, len(patterns))
	for _, p := range patterns {
		if !all && !p.public {
			hiddenPatterns++
			continue
		}
		visiblePatterns = append(visiblePatterns, p)
	}
	if len(visiblePatterns) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Patterns:")
		ptw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, p := range visiblePatterns {
			if p.desc != "" {
				fmt.Fprintf(ptw, "  '%s'\t%s\n", p.pattern, p.desc)
			} else {
				fmt.Fprintf(ptw, "  '%s'\t\n", p.pattern)
			}
		}
		ptw.Flush()
	}

	// Verbs: every verb is shown. Per-verb target list is filtered so it
	// only shows what would also appear in the Targets section above
	// (docstringed targets plus `all`). A trailing count tells the user
	// how many internal targets the verb also applies to.
	verbToTargets := b.verbToTargetsMap()
	verbPatterns := b.verbToPatternsMap()
	if len(verbToTargets) > 0 || len(verbPatterns) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Verbs (run as `mmk <verb>` or `mmk <verb> <target>`):")
		vtw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, verb := range b.Verbs() {
			var visible []string
			internal := 0
			for _, t := range verbToTargets[verb] {
				r := b.concretes[t]
				desc := ""
				if r != nil {
					desc = r.Description
				}
				if all || isPublic(t, desc) {
					visible = append(visible, t)
				} else {
					internal++
				}
			}
			for _, p := range verbPatterns[verb] {
				visible = append(visible, "'"+p+"'")
			}
			line := strings.Join(visible, ", ")
			if internal > 0 {
				if line != "" {
					line += " "
				}
				line += fmt.Sprintf("(+ %d internal targets. Use -all to see them.)", internal)
			}
			fmt.Fprintf(vtw, "  %s\t%s\n", verb, line)
		}
		vtw.Flush()
	}

	// Footer: tell users about -all when filtering hid anything.
	if !all && (hiddenTargets > 0 || hiddenPatterns > 0) {
		fmt.Fprintln(w)
		switch {
		case hiddenTargets > 0 && hiddenPatterns > 0:
			fmt.Fprintf(w, "(%s and %s hidden — use -all to see them)\n", plural(hiddenTargets, "target"), plural(hiddenPatterns, "pattern"))
		case hiddenTargets > 0:
			fmt.Fprintf(w, "(%s hidden — use -all to see them)\n", plural(hiddenTargets, "target"))
		default:
			fmt.Fprintf(w, "(%s hidden — use -all to see them)\n", plural(hiddenPatterns, "pattern"))
		}
	}
}

// plural returns "<n> <singular>" or "<n> <singular>s".
func plural(n int, singular string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %ss", n, singular)
}

// verbToPatternsMap returns each verb's pattern rules.
func (b *Build) verbToPatternsMap() map[string][]string {
	out := make(map[string][]string)
	for _, pe := range b.patterns {
		if pe.rule.Verb != "" {
			out[pe.rule.Verb] = append(out[pe.rule.Verb], pe.rule.Pattern)
		}
	}
	for verb, ps := range out {
		sort.Strings(ps)
		out[verb] = ps
	}
	return out
}

// annotateRule returns a one-line description of a rule's salient property:
// its runner, type, or (for aggregators with no body) its dep list.
func annotateRule(r *parse.TargetRule) string {
	if r == nil {
		return ""
	}
	switch {
	case r.Runner != "":
		s := "on " + r.Runner
		if r.Type != "" {
			s = r.Type + " " + s
		}
		return s
	case r.Type != "":
		return "type: " + r.Type
	case r.HasDepSep && r.Body == "" && len(r.Deps) > 0:
		// Aggregator: shows what it pulls in.
		parts := make([]string, len(r.Deps))
		for i, d := range r.Deps {
			if d.Verb != "" {
				parts[i] = "[" + d.Verb + " " + d.Target + "]"
			} else {
				parts[i] = d.Target
			}
		}
		return "→ " + strings.Join(parts, " ")
	}
	return ""
}

// verbToTargetsMap returns a map from verb name to the list of targets that
// support that verb. A target supports a verb if there's an explicit
// [verb target] rule, or if the target's type has a defbody for that verb
// (built-in or user-defined).
func (b *Build) verbToTargetsMap() map[string][]string {
	out := make(map[string][]string)
	add := func(verb, target string) {
		out[verb] = append(out[verb], target)
	}

	// Explicit [verb target] rules.
	for key := range b.verbConcretes {
		add(key.verb, key.target)
	}
	// Type-defbody verbs apply to every target of that type.
	verbsByType := make(map[string][]string)
	for vbk := range b.defVerbBodies {
		verbsByType[vbk.typeName] = append(verbsByType[vbk.typeName], vbk.verb)
	}
	for tname, r := range b.concretes {
		for _, verb := range verbsByType[r.Type] {
			add(verb, tname)
		}
	}

	// Dedupe + sort each verb's target list.
	for verb, targets := range out {
		seen := make(map[string]bool, len(targets))
		uniq := targets[:0]
		for _, t := range targets {
			if !seen[t] {
				seen[t] = true
				uniq = append(uniq, t)
			}
		}
		sort.Strings(uniq)
		out[verb] = uniq
	}
	return out
}

// Targets returns the names of all explicitly declared concrete (non-pattern)
// targets, sorted alphabetically.
func (b *Build) Targets() []string {
	names := make([]string, 0, len(b.concretes))
	for name := range b.concretes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Patterns returns the regex strings of all pattern rules (non-verb), sorted.
func (b *Build) Patterns() []string {
	var patterns []string
	for _, pe := range b.patterns {
		if pe.rule.Verb == "" {
			patterns = append(patterns, pe.rule.Pattern)
		}
	}
	sort.Strings(patterns)
	return patterns
}

// Verbs returns all known verb names, sorted. This includes verbs from
// explicit verb rules, verb pattern rules, and defbody verb entries
// (including the built-in ones like "clean" on file targets).
func (b *Build) Verbs() []string {
	seen := make(map[string]bool)
	for key := range b.verbConcretes {
		seen[key.verb] = true
	}
	for _, pe := range b.patterns {
		if pe.rule.Verb != "" {
			seen[pe.rule.Verb] = true
		}
	}
	for key := range b.defVerbBodies {
		seen[key.verb] = true
	}
	verbs := make([]string, 0, len(seen))
	for v := range seen {
		verbs = append(verbs, v)
	}
	sort.Strings(verbs)
	return verbs
}
