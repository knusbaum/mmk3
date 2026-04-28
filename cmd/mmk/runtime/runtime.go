// Package runtime wires the parsed mmk AST to the dag executor.
// It resolves concrete and pattern targets, generates the bash function
// script on demand, and implements dag.Node via TargetNode.
package runtime

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/knusbaum/mmk3/cmd/mmk/gen"
	"github.com/knusbaum/mmk3/cmd/mmk/parse"
	"github.com/knusbaum/mmk3/dag"
)

// genfileDir is the host directory where all Builds in this process write
// their generated bash scripts. The image runner mounts this path 1:1 into
// the container so bodies can source $MMK_GENFILE by its host path. A
// dedicated dir (rather than /tmp itself) keeps the rest of /tmp writable
// inside the container.
const genfileDir = "/tmp/mmk-genfiles"

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

// subprojectInfo is what the runtime tracks about each `subproject` directive
// after expansion: the target name (also the registered top-level target),
// the runner clause that becomes the sub-Build's defaultRunner, the directory
// containing the sub-mmkfile, and the eagerly-constructed sub-Build itself.
//
// In merged mode (current model), the sub-Build is part of the parent's
// resolution chain: parent.Resolve("foo") → sub.Resolve("all") for a
// subproject named foo. Sub-targets are real nodes in the unified DAG,
// not opaque shell delegations.
type subprojectInfo struct {
	target string
	runner string
	path   string
	build  *Build
}

// runnerRegistry holds the state for runner setup phases (e.g. container ids)
// shared across a parent Build and all of its sub-Builds. Without sharing,
// each Build would do its own `docker run` for the same image, even when
// nodes from parent and sub use the same runner.
type runnerRegistry struct {
	mu     sync.Mutex
	states map[string]string // runner target name → state from setup stdout
}

func newRunnerRegistry() *runnerRegistry {
	return &runnerRegistry{states: map[string]string{}}
}

// Build holds the indexed rules and the generated bash script.
// Create one with NewBuild; call Close when done to remove the temp script
// and run cleanup for any runners that were started during the build.
// Set Verbose = true before calling Execute to log each target as it runs or is skipped.
type Build struct {
	Verbose       bool
	concretes     map[string]*parse.TargetRule
	verbConcretes map[verbNodeKey]*parse.TargetRule
	patterns      []*patternEntry
	nodes         map[string]*TargetNode
	verbNodes     map[verbNodeKey]*TargetNode
	runnerNodes   map[string]*TargetNode // runner target name → synthetic runner init node
	runnerDefs    map[string]runnerDefInfo
	defBodies          map[string]bool // type name → has default body (built-in or user defbody)
	defVerbBodies      map[defVerbBodyKey]bool
	userDefBodyOptions map[defVerbBodyKey][]parse.Option // user defbody options keyed by (type, verb)
	subprojects        map[string]*subprojectInfo        // subproject target → metadata for sub-path delegation
	genPath       string
	genFile       *os.File

	// parent is non-nil for sub-Builds. Used for runner image fall-back
	// resolution and to share genfile context.
	parent *Build
	// path is the working directory for this Build's body executions. The
	// root Build leaves this empty (use os.Getwd at body time); sub-Builds
	// set it to the subproject path so cmd.Dir gets the right cwd.
	path string
	// defaultRunner, if non-empty, fills in the Runner clause on rules that
	// don't declare their own `on`. Set when the parent declared
	// `subproject foo on R` — every body in foo's mmkfile should default
	// to running on R.
	defaultRunner string

	runners *runnerRegistry
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

// BuildOption configures a Build during construction. Used by sub-Build
// construction to inject a path or parent reference before subproject
// expansion runs (which needs the path to resolve sub-mmkfile locations).
type BuildOption func(*Build)

// WithPath sets the absolute filesystem path for this Build. Used by
// expandSubprojects to root sub-Builds at the right directory so their own
// subproject expansions can read sub-sub-mmkfiles from disk.
func WithPath(p string) BuildOption {
	return func(b *Build) { b.path = p }
}

// WithDefaultRunner sets the runner that fills in rules with no explicit
// `on` clause. Set on sub-Builds whose parent declared `subproject foo on R`.
// Must be set before NewBuild's body runs so passthrough freezing logic can
// see it.
func WithDefaultRunner(r string) BuildOption {
	return func(b *Build) { b.defaultRunner = r }
}

// NewBuild parses src, validates names, generates the initial bash script, and
// returns a Build ready for Execute. By default the Build's working directory
// is os.Getwd() at construction time; pass WithPath to override (used for
// sub-Builds whose mmkfile lives in a subdirectory).
func NewBuild(src []byte, opts ...BuildOption) (*Build, error) {
	f, err := parse.Parse(src)
	if err != nil {
		return nil, err
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
		subprojects:        make(map[string]*subprojectInfo),
		runners:            newRunnerRegistry(),
	}

	if cwd, err := os.Getwd(); err == nil {
		b.path = cwd
	}
	for _, opt := range opts {
		opt(b)
	}

	// Populate runnerDefs from built-in definitions.
	for typeName, info := range gen.BuiltinRunnerDefs {
		b.runnerDefs[typeName] = runnerDefInfo{hasSetup: info.HasSetup, hasCleanup: info.HasCleanup}
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
			if d.Verb != "" {
				b.defVerbBodies[defVerbBodyKey{d.Type, d.Verb}] = true
			} else {
				b.defBodies[d.Type] = true
			}
		}
	}

	// Genfiles live in a dedicated host directory so the docker image runner
	// can mount only that path into the container (read-only) without
	// shadowing /tmp wholesale. Keeps the container's /tmp writable for tools
	// like `go run` while still letting bodies source the genfile by its
	// host path (which equals the container path because we mount it 1:1).
	if err := os.MkdirAll(genfileDir, 0o755); err != nil {
		return nil, err
	}
	genf, err := os.CreateTemp(genfileDir, "mmk-generated-*.sh")
	if err != nil {
		return nil, err
	}
	b.genPath = genf.Name()
	b.genFile = genf

	// evalPassthroughs freezes variable values at parse time, so $(pwd) and
	// similar resolve once. That's correct for bodies running in the same
	// context as parsing — but when this Build has a default runner (set on
	// sub-Builds whose parent declared `subproject foo on R`), bodies run
	// inside that runner where the filesystem layout differs (e.g. /work vs
	// the host path). Skip freezing in that case so the passthroughs
	// re-evaluate fresh inside the runner each body.
	var frozen []string
	if b.defaultRunner == "" {
		f, err := evalPassthroughs(f, b.path)
		if err == nil {
			frozen = f
		}
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
// temporary generated script. Sub-Builds share the runner registry with the
// root, so only the root's Close should run cleanup; sub Closes just remove
// their genfiles.
func (b *Build) Close() {
	if b.parent != nil {
		// Sub-Build: only clean up our own genfile. Runner cleanup is the
		// root's responsibility.
		if b.genFile != nil {
			b.genFile.Close()
			os.Remove(b.genPath)
			b.genFile = nil
		}
		for _, sp := range b.subprojects {
			if sp.build != nil {
				sp.build.Close()
			}
		}
		return
	}

	b.runners.mu.Lock()
	states := b.runners.states
	b.runners.states = nil
	b.runners.mu.Unlock()

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
// single runner node so setup executes only once.
func (b *Build) runnerNode(runnerTarget *TargetNode) *TargetNode {
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
	for _, sp := range b.subprojects {
		if sp.build != nil && sp.build.HasVerb(verb) {
			return true
		}
	}
	return false
}

// HasTarget reports whether name is a known concrete or pattern-matched
// target — including subproject names and "subproject/rest" paths that
// resolve via sub-Builds.
func (b *Build) HasTarget(name string) bool {
	if _, ok := b.concretes[name]; ok {
		return true
	}
	for _, pe := range b.patterns {
		if pe.re.MatchString(name) {
			return true
		}
	}
	if sp, ok := b.subprojects[name]; ok && sp.build != nil {
		return true
	}
	if i := strings.IndexByte(name, '/'); i > 0 {
		if sp, ok := b.subprojects[name[:i]]; ok && sp.build != nil {
			return sp.build.HasTarget(name[i+1:])
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

// checkVerbApplicable returns an error if root is a verb node and no node in
// its dependency tree has a non-empty body for that verb. This catches the
// case where a verb is defined elsewhere in the project but doesn't apply to
// any target reachable from the requested root — `mmk foop all` where `foop`
// only has a rule for some target outside `all`'s subtree.
func checkVerbApplicable(root *TargetNode) error {
	if root.verb == "" {
		return nil
	}
	if hasApplicableVerbBody(root, make(map[*TargetNode]bool)) {
		return nil
	}
	return fmt.Errorf("verb %q has no applicable rule in the dependency tree of [%s %s]", root.verb, root.verb, root.target)
}

func hasApplicableVerbBody(n *TargetNode, seen map[*TargetNode]bool) bool {
	if seen[n] {
		return false
	}
	seen[n] = true
	if n.verb != "" {
		if _, has := n.executeScript(); has {
			return true
		}
	}
	for _, dep := range n.Dependencies() {
		if hasApplicableVerbBody(dep, seen) {
			return true
		}
	}
	return false
}

// Execute builds the DAG rooted at target (optionally qualified by verb) and
// runs it with the given parallelism. parallelism <= 0 means unlimited.
// When b.Verbose is true, each target is logged as it runs or is skipped.
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
	if err := checkVerbApplicable(root); err != nil {
		return err
	}
	if !b.Verbose {
		return dag.Execute(root, parallelism)
	}
	hooks := dag.Hooks[*TargetNode]{
		OnRun: func(n *TargetNode) {
			if n.kind == kindRunner {
				fmt.Printf("starting runner: %s\n", n.runnerFor.target)
			} else if n.verb != "" {
				fmt.Printf("running: [%s %s]\n", n.verb, n.target)
			} else {
				fmt.Printf("running: %s\n", n.target)
			}
		},
		OnSkip: func(n *TargetNode) {
			if n.kind == kindRunner {
				return // runner setup dedup is an internal detail, not user-visible
			}
			fmt.Printf("skipping: %s (up to date)\n", n.target)
		},
	}
	return dag.Execute(root, parallelism, hooks)
}

// Resolve returns (creating if necessary) the TargetNode for the named target.
// It is safe to call multiple times with the same name; the same node is returned.
//
// Subproject delegation:
//   - parent.Resolve("foo") for subproject `foo` returns sub.Resolve("all").
//   - parent.Resolve("foo/bar") returns sub.Resolve("bar").
// The returned node's build pointer is the sub-Build, so its body executes
// with the sub-Build's genfile and cwd.
func (b *Build) Resolve(name string) (*TargetNode, error) {
	if n, ok := b.nodes[name]; ok {
		return n, nil
	}
	if sp, ok := b.subprojects[name]; ok && sp.build != nil {
		return sp.build.Resolve("all")
	}
	if i := strings.IndexByte(name, '/'); i > 0 {
		if sp, ok := b.subprojects[name[:i]]; ok && sp.build != nil {
			return sp.build.Resolve(name[i+1:])
		}
	}
	// Parent fallback for typed concretes (typically runner images declared
	// at the parent level). The returned node lives in the parent's Build,
	// so its deps resolve relative to the parent's working directory.
	if b.parent != nil {
		if rule, ok := b.parent.concretes[name]; ok && rule.Type != "" && rule.Type != "source" {
			return b.parent.Resolve(name)
		}
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
// node is created as long as the target has a default rule.
//
// Subproject delegation mirrors Resolve: [verb foo] → sub.ResolveVerb("all", verb),
// [verb foo/bar] → sub.ResolveVerb("bar", verb).
func (b *Build) ResolveVerb(target, verb string) (*TargetNode, error) {
	key := verbNodeKey{target, verb}
	if n, ok := b.verbNodes[key]; ok {
		return n, nil
	}
	if sp, ok := b.subprojects[target]; ok && sp.build != nil {
		return sp.build.ResolveVerb("all", verb)
	}
	if i := strings.IndexByte(target, '/'); i > 0 {
		if sp, ok := b.subprojects[target[:i]]; ok && sp.build != nil {
			return sp.build.ResolveVerb(target[i+1:], verb)
		}
	}
	rule := b.findRuleForVerb(target, verb)
	if rule == nil {
		// Ensure a default rule exists so dep inheritance can propagate.
		// Resolve populates concretes for inferred source targets.
		if _, ok := b.concretes[target]; !ok {
			b.Resolve(target) //nolint — side effect: populates concretes
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
type TargetNode struct {
	build     *Build
	target    string
	verb      string            // non-empty for verb nodes
	rule      *parse.TargetRule // nil when kind != kindRule, or for inherited verb nodes
	kind      targetKind
	runnerFor *TargetNode // set when kind == kindRunner
	deps      []*TargetNode
	depsBuilt bool
	resolveErr error

	// Date is computed once per node and cached. Recomputing is expensive
	// when the type's Date function shells into a runner (e.g. docker exec
	// per .o file's mtime check), and a node's Date is read by both its own
	// NeedsRun and every consumer's NeedsRun.
	dateOnce  sync.Once
	dateValue time.Time
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
		n.deps = []*TargetNode{n.runnerFor}
		return n.deps
	}

	if n.verb != "" {
		return n.verbDependencies()
	}

	for _, dep := range n.rule.Deps {
		targets, err := n.build.expandDep(dep.Target)
		if err != nil {
			n.resolveErr = err
			return n.deps
		}
		for _, target := range targets {
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
	if n.rule.Runner != "" {
		runnerNode, err := n.build.Resolve(n.rule.Runner)
		if err != nil {
			n.resolveErr = err
			return n.deps
		}
		n.deps = append(n.deps, runnerNode, n.build.runnerNode(runnerNode))
	}
	return n.deps
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
			targets, err := n.build.expandDep(dep.Target)
			if err != nil {
				n.resolveErr = err
				return n.deps
			}
			for _, target := range targets {
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
			runnerNode, err := n.build.Resolve(n.rule.Runner)
			if err != nil {
				n.resolveErr = err
				return n.deps
			}
			n.deps = append(n.deps, runnerNode, n.build.runnerNode(runnerNode))
		}
		return n.deps
	}

	n.deps = append(n.deps, n.inheritedVerbDeps()...)
	if n.rule != nil && n.rule.Runner != "" {
		runnerNode, err := n.build.Resolve(n.rule.Runner)
		if err != nil {
			n.resolveErr = err
			return n.deps
		}
		n.deps = append(n.deps, runnerNode, n.build.runnerNode(runnerNode))
	}
	return n.deps
}

// inheritedVerbDeps returns the default rule's deps with this node's verb
// applied to each. Used both for no-colon inheritance and for ':+' augment.
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
	for _, dep := range defaultRule.Deps {
		targets, err := n.build.expandDep(dep.Target)
		if err != nil {
			n.resolveErr = err
			return deps
		}
		for _, target := range targets {
			depNode, err := n.build.ResolveVerb(target, n.verb)
			if err != nil {
				n.resolveErr = err
				return deps
			}
			deps = append(deps, depNode)
		}
	}
	return deps
}

// OrderDependencies returns order-only deps for this node — edges that
// constrain scheduling but don't pull nodes into the DAG. The dag library
// honors them only when the referenced node is independently in the graph.
//
// Two cases produce order-only edges:
//
//   - This node is `[verb T]` where T's effective options for verb include
//     order=after-consumers. The runner-typed T runs after every
//     `[verb consumer]` for consumers using T as their runner.
//
//   - This node is `[verb T]` where T's runner R has effective options for
//     verb include order=before-consumers. T runs after `[verb R]`.
func (n *TargetNode) OrderDependencies() []*TargetNode {
	if n.verb == "" || n.kind == kindRunner {
		return nil
	}
	var deps []*TargetNode

	// Case 1: this is a verb on a runner-typed target with order=after-consumers.
	if order := n.build.effectiveVerbOption(n.target, n.verb, "order"); order == "after-consumers" {
		for _, r := range n.build.concretes {
			if r.Runner == n.target && r.Verb == "" {
				cn, err := n.build.ResolveVerb(r.Target, n.verb)
				if err == nil {
					deps = append(deps, cn)
				}
			}
		}
	}

	// Case 2: this node's default rule has a runner R, and R's effective
	// options for this verb include order=before-consumers.
	defaultRule := n.build.concretes[n.target]
	if defaultRule != nil && defaultRule.Runner != "" {
		if order := n.build.effectiveVerbOption(defaultRule.Runner, n.verb, "order"); order == "before-consumers" {
			rn, err := n.build.ResolveVerb(defaultRule.Runner, n.verb)
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
//     timestamp (epoch seconds or RFC3339); non-zero exit returns zero time.
func (n *TargetNode) Date() time.Time {
	n.dateOnce.Do(func() {
		n.dateValue = n.computeDate()
	})
	return n.dateValue
}

func (n *TargetNode) computeDate() time.Time {
	if n.kind == kindRunner {
		// Runner nodes are pure setup. Returning zero time means they never
		// look "newer" than artifacts that depend on them.
		return time.Time{}
	}
	switch n.rule.Type {
	case "":
		return time.Now()
	case "source", "file":
		// Fast path: skip the bash subprocess (which would be a docker exec
		// for sub-Builds with a runner) and stat the file directly. The
		// target path is resolved against this Build's cwd; we mount the
		// project root 1:1 into the container, so the file we stat on host
		// matches what the body would see in the runner.
		path := n.target
		if !filepath.IsAbs(path) && n.build.path != "" {
			path = filepath.Join(n.build.path, path)
		}
		info, err := os.Stat(path)
		if err != nil {
			return time.Time{}
		}
		return info.ModTime()
	default: // all typed targets run their deftype bash function
		t, err := n.userTypeDate()
		if err != nil {
			return time.Time{}
		}
		return t
	}
}

// NeedsRun returns true if the target needs to run.
// Phony targets (no type) always need to run.
// Typed targets compare their own Date() against each dependency's Date();
// if the artifact doesn't exist (zero Date) or any dep is newer, they run.
// Verb nodes always need to run (they are imperative actions, not artifacts).
func (n *TargetNode) NeedsRun() bool {
	if n.verb != "" {
		return true
	}
	if n.kind == kindRunner {
		// Runner nodes run setup at most once per build per runner target.
		n.build.runners.mu.Lock()
		_, started := n.build.runners.states[n.runnerFor.target]
		n.build.runners.mu.Unlock()
		return !started
	}
	if n.rule.Type == "" {
		return true
	}
	myDate := n.Date()
	if myDate.IsZero() {
		return true // artifact doesn't exist yet
	}
	for _, dep := range n.deps {
		if dep.Date().After(myDate) {
			return true
		}
	}
	return false
}

// userTypeDate runs the deftype bash function for this node's type and parses
// its stdout as a timestamp (epoch seconds or RFC3339/RFC3339Nano).
// Non-zero exit is treated as "artifact doesn't exist" and returns an error.
func (n *TargetNode) userTypeDate() (time.Time, error) {
	out, err := n.runBashOutput(gen.TypeFunc(n.rule.Type))
	if err != nil {
		return time.Time{}, err
	}
	return parseTimestamp(strings.TrimSpace(out))
}

// parseTimestamp parses s as either epoch seconds (all digits) or RFC3339/RFC3339Nano.
func parseTimestamp(s string) (time.Time, error) {
	if isAllDigits(s) {
		epoch, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse epoch %q: %w", s, err)
		}
		return time.Unix(epoch, 0), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("cannot parse timestamp %q (want epoch seconds or RFC3339)", s)
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
	return `__mmk_exec() { [ -n "$MMK_VERBOSE" ] && set -x;` + body + "}; __mmk_exec"
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
// whether there is anything to run. Non-verb nodes always have a snippet (at
// minimum a no-op). Verb nodes return (_, false) when there is no body.
func (n *TargetNode) executeScript() (string, bool) {
	if n.verb == "" {
		return wrapExecute(gen.NormalizeBody(n.nonVerbBody())), true
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
	if n.resolveErr != nil {
		return n.resolveErr
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
	runnerNode, err := n.build.Resolve(n.rule.Runner)
	if err != nil {
		return err
	}
	runnerType := runnerNode.rule.Type

	n.build.runners.mu.Lock()
	state := n.build.runners.states[runnerNode.target]
	n.build.runners.mu.Unlock()

	script := `. "$MMK_GENFILE"; ` + gen.RunnerRunFunc(runnerType)
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(),
		"MMK_GENFILE="+n.build.genPath,
		"target="+runnerNode.target,
		"deps="+strings.Join(runnerNode.explicitDepNames(), " "),
		"MMK_RUNNER_STATE="+state,
		"MMK_EXECUTE="+execute,
		"MMK_TARGET="+n.target,
		"MMK_DEPS="+strings.Join(n.explicitDepNames(), " "),
		// Path relative to the root Build's working directory. The image
		// runner uses this to set docker exec -w to the corresponding
		// container path so that bodies in sub-Builds see the right cwd
		// (`/work/<rel>`) instead of the container's WORKDIR (`/work`).
		"MMK_BUILD_PATH="+n.build.relPath(),
	)
	if dir := n.build.workdir(); dir != "" {
		cmd.Dir = dir
	}
	if n.build.Verbose {
		cmd.Env = append(cmd.Env, "MMK_VERBOSE=1")
	}
	// Image (runner) options first, then target options. On collision Go's
	// os/exec resolves duplicate keys by last-write-wins, so target shadows.
	cmd.Env = appendRuleOptions(cmd.Env, runnerNode.rule)
	cmd.Env = appendRuleOptions(cmd.Env, n.rule)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
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
		n.build.runners.mu.Lock()
		n.build.runners.states[runnerNode.target] = ""
		n.build.runners.mu.Unlock()
		return nil
	}

	out, err := runnerNode.runBashOutput(gen.RunnerSetupFunc(runnerType))
	if err != nil {
		return fmt.Errorf("runner setup for %q: %w", runnerNode.target, err)
	}

	n.build.runners.mu.Lock()
	n.build.runners.states[runnerNode.target] = strings.TrimSpace(out)
	n.build.runners.mu.Unlock()
	return nil
}

// explicitDepNames returns just the target names from rule.Deps — not the
// implicit deps (runner target, container node) that Dependencies() appends.
// This is what `$deps` should expose to user bodies.
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
	return names
}

// expandDep returns the target names a dep resolves to. Tokens that start
// with '$' are expanded via bash; the result is word-split, so a variable
// holding multiple space-separated names produces multiple deps.
func (b *Build) expandDep(dep string) ([]string, error) {
	return b.expandToken(dep, "dep")
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
	if b.path != "" {
		cmd.Dir = b.path
	}
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
// its concrete target name and runner clause using bash. Pattern targets are
// left untouched (their string is a regex). Both Target and Runner must
// expand to exactly one word.
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
	}
	return nil
}

// expandSubprojects processes each `subproject` directive in f. For each, it
// expands $VAR in the target/runner names, reads the subproject's mmkfile
// source, eagerly constructs a sub-Build, and registers it. Sub-Builds share
// the parent's runner registry and inherit the parent's image rules so they
// can resolve runner targets. Sub-Build rules with no explicit `on` inherit
// the parent's `subproject foo on R` runner.
//
// Resolution of subproject targets (e.g. parent.Resolve("foo") or
// "foo/bar") then traverses into the sub-Build at lookup time — no
// synthetic delegation rules are needed.
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

		absPath := path
		if !filepath.IsAbs(absPath) {
			absPath = filepath.Join(b.path, path)
		}

		subSrc, err := readSubMmkfileSource(absPath)
		if err != nil {
			return fmt.Errorf("subproject %q: %w", sp.Target, err)
		}

		sub, err := NewBuild(subSrc, WithPath(absPath), WithDefaultRunner(sp.Runner))
		if err != nil {
			return fmt.Errorf("subproject %q: %w", sp.Target, err)
		}
		// Wire sub into parent's chain. Recursive registry sharing handles
		// any nested subprojects that the sub already constructed.
		sub.parent = b
		sub.shareRunnerRegistry(b.runners)
		sub.applyDefaultRunner()
		// Image rules (runner targets) are resolved via parent fallback at
		// lookup time rather than copied — see Resolve. That keeps the
		// image's relative deps (e.g. ".gitlab/Dockerfile") rooted at the
		// parent's path.

		b.subprojects[sp.Target] = &subprojectInfo{
			target: sp.Target,
			runner: sp.Runner,
			path:   path,
			build:  sub,
		}
	}
	return nil
}

// readSubMmkfileSource is the byte-source variant of readSubMmkfile, used by
// expandSubprojects to construct sub-Builds via NewBuild.
func readSubMmkfileSource(path string) ([]byte, error) {
	for _, name := range []string{"mmkfile", "Mmkfile"} {
		p := filepath.Join(path, name)
		data, err := os.ReadFile(p)
		if err == nil {
			return data, nil
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
	}
	return nil, fmt.Errorf("no mmkfile or Mmkfile found in %q", path)
}

// workdir returns the absolute cwd in which this Build's body bashes should
// run. Set at construction (NewBuild captures os.Getwd or honors WithPath);
// expandSubprojects passes the joined absolute path when building sub-Builds.
func (b *Build) workdir() string { return b.path }

// relPath returns this Build's path relative to the root Build (the topmost
// ancestor with no parent). Empty for the root. Used to translate the host
// working directory into a container-side workdir for runners.
func (b *Build) relPath() string {
	if b.parent == nil {
		return ""
	}
	root := b
	for root.parent != nil {
		root = root.parent
	}
	rel, err := filepath.Rel(root.path, b.path)
	if err != nil {
		return ""
	}
	return rel
}

// shareRunnerRegistry sets b.runners and recursively propagates to all
// sub-Builds, so the entire parent/sub tree uses one runner state map.
func (b *Build) shareRunnerRegistry(r *runnerRegistry) {
	b.runners = r
	for _, sp := range b.subprojects {
		if sp.build != nil {
			sp.build.shareRunnerRegistry(r)
		}
	}
}

// applyDefaultRunner fills the Runner clause on rules that don't have one
// with b.defaultRunner. Used after sub-Build construction to make rules
// inherit "subproject foo on R" without needing each rule in foo to repeat.
func (b *Build) applyDefaultRunner() {
	if b.defaultRunner == "" {
		return
	}
	for _, r := range b.concretes {
		if r.Runner == "" && r.Type != "image" {
			r.Runner = b.defaultRunner
		}
	}
	for _, r := range b.verbConcretes {
		if r.Runner == "" {
			r.Runner = b.defaultRunner
		}
	}
	for _, pe := range b.patterns {
		if pe.rule.Runner == "" {
			pe.rule.Runner = b.defaultRunner
		}
	}
}

// ResolveSubpath reports whether `target` is of the form "<subproject>/<rest>"
// and resolvable via a registered sub-Build. In the merged subproject model
// this is purely a probe — Resolve handles the redirection at lookup time.
// Kept for back-compat with main's "is this a target or a verb?" check.
func (b *Build) ResolveSubpath(target, verb string) bool {
	i := strings.IndexByte(target, '/')
	if i <= 0 {
		return false
	}
	prefix, suffix := target[:i], target[i+1:]
	sp, ok := b.subprojects[prefix]
	if !ok {
		return false
	}
	if sp.build == nil {
		return false
	}
	if verb == "" {
		return sp.build.HasTarget(suffix)
	}
	return sp.build.HasTarget(suffix) && sp.build.HasVerb(verb)
}

// readSubMmkfile reads <path>/mmkfile or <path>/Mmkfile and returns the parsed AST.
func readSubMmkfile(path string) (*parse.File, error) {
	for _, name := range []string{"mmkfile", "Mmkfile"} {
		p := filepath.Join(path, name)
		data, err := os.ReadFile(p)
		if err == nil {
			return parse.Parse(data)
		}
		if !os.IsNotExist(err) {
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
	script := `. "$MMK_GENFILE"; target="$MMK_TARGET"; deps="$MMK_DEPS"; eval "$MMK_EXECUTE"`
	c := exec.Command("bash", "-c", script)
	c.Env = append(os.Environ(),
		"MMK_GENFILE="+n.build.genPath,
		"MMK_TARGET="+n.target,
		"MMK_DEPS="+strings.Join(n.explicitDepNames(), " "),
		"MMK_EXECUTE="+execute,
	)
	if dir := n.build.workdir(); dir != "" {
		c.Dir = dir
	}
	if n.build.Verbose {
		c.Env = append(c.Env, "MMK_VERBOSE=1")
	}
	c.Env = appendRuleOptions(c.Env, n.rule)
	if output {
		c.Stdin = os.Stdin
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
	}
	return c.Run()
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
	if dir := n.build.workdir(); dir != "" {
		c.Dir = dir
	}
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

// subprojectSummary captures what's in a (sub-)mmkfile for -list display.
type subprojectSummary struct {
	path               string                 // path relative to the root invocation
	prefix             string                 // accumulated subproject path, e.g. "src/lib"
	targets            []string               // concrete (non-pattern, non-verb) target names
	targetDescriptions map[string]string      // target name → description
	verbs              []string               // harvested verbs
	children           []*subprojectSummary   // nested subprojects
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
func (b *Build) PrintList(w io.Writer) {
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
	type entry struct{ name, info string }
	pickInfo := func(desc, annot string) string {
		if desc != "" {
			return desc
		}
		return annot
	}
	all := make([]entry, 0, len(b.concretes))
	for _, name := range b.Targets() {
		r := b.concretes[name]
		annot := annotateRule(r)
		desc := ""
		if r != nil {
			desc = firstLine(r.Description)
		}
		if subAnnot, ok := subRoots[name]; ok {
			// Subproject root: the "subproject (path/mmkfile)" annotation
			// adds structural info beyond what the runner clause alone says,
			// so prefer it when no description is supplied.
			if annot != "" {
				annot = subAnnot + ", " + annot
			} else {
				annot = subAnnot
			}
			delete(subRoots, name)
		}
		all = append(all, entry{name, pickInfo(desc, annot)})
	}
	// Sub-of-sub subproject roots (not in top-level concretes), and sub-targets.
	for _, s := range subSummaries {
		if annot, ok := subRoots[s.prefix]; ok {
			all = append(all, entry{name: s.prefix, info: annot})
		}
		for _, t := range s.targets {
			all = append(all, entry{
				name: s.prefix + "/" + t,
				info: firstLine(s.targetDescriptions[t]),
			})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].name < all[j].name })
	for _, e := range all {
		fmt.Fprintf(tw, "  %s\t%s\n", e.name, e.info)
	}
	tw.Flush()

	patterns := b.Patterns()
	if len(patterns) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Patterns:")
		for _, p := range patterns {
			fmt.Fprintf(w, "  '%s'\n", p)
		}
	}

	verbToTargets := b.verbToTargetsMap()
	verbPatterns := b.verbToPatternsMap()
	if len(verbToTargets) > 0 || len(verbPatterns) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Verbs (run as `mmk <verb>` or `mmk <verb> <target>`):")
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, verb := range b.Verbs() {
			parts := append([]string{}, verbToTargets[verb]...)
			for _, p := range verbPatterns[verb] {
				parts = append(parts, "'"+p+"'")
			}
			fmt.Fprintf(tw, "  %s\t%s\n", verb, strings.Join(parts, ", "))
		}
		tw.Flush()
	}

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

