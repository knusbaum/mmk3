// Package runtime wires the parsed mmk AST to the dag executor.
// It resolves concrete and pattern targets, generates the bash function
// script on demand, and implements dag.Node via TargetNode.
package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/knusbaum/mmk3/cmd/mmk/gen"
	"github.com/knusbaum/mmk3/cmd/mmk/parse"
	"github.com/knusbaum/mmk3/dag"
)

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
	defVerbBodies map[defVerbBodyKey]bool
	genPath       string
	genFile       *os.File

	runnerStatesMu sync.Mutex
	runnerStates   map[string]string // runner target name → state from setup stdout
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
	}
	return nil
}

// NewBuild parses src, validates names, generates the initial bash script, and
// returns a Build ready for Execute.
func NewBuild(src []byte) (*Build, error) {
	f, err := parse.Parse(src)
	if err != nil {
		return nil, err
	}
	if err := gen.ValidateDuplicates(f); err != nil {
		return nil, err
	}
	if err := validateDirectives(f); err != nil {
		return nil, err
	}

	b := &Build{
		concretes:     make(map[string]*parse.TargetRule),
		verbConcretes: make(map[verbNodeKey]*parse.TargetRule),
		nodes:         make(map[string]*TargetNode),
		verbNodes:     make(map[verbNodeKey]*TargetNode),
		runnerNodes:   make(map[string]*TargetNode),
		runnerDefs:    make(map[string]runnerDefInfo),
		defVerbBodies: make(map[defVerbBodyKey]bool),
		runnerStates:  make(map[string]string),
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

	// Pre-populate with built-in verb bodies; user defbody entries below may override.
	for typeName, verbs := range gen.BuiltinVerbBodies {
		for verb := range verbs {
			b.defVerbBodies[defVerbBodyKey{typeName, verb}] = true
		}
	}

	for _, d := range f.Directives {
		switch d := d.(type) {
		case *parse.DefBody:
			if d.Verb != "" {
				b.defVerbBodies[defVerbBodyKey{d.Type, d.Verb}] = true
			}
		case *parse.TargetRule:
			if d.Pattern != "" {
				re, err := regexp.Compile(`^(?:` + d.Pattern + `)$`)
				if err != nil {
					return nil, fmt.Errorf("pattern %q: %w", d.Pattern, err)
				}
				b.patterns = append(b.patterns, &patternEntry{rule: d, re: re})
			} else if d.Verb != "" {
				b.verbConcretes[verbNodeKey{d.Target, d.Verb}] = d
			} else {
				b.concretes[d.Target] = d
			}
		}
	}

	genf, err := os.CreateTemp("", "mmk-generated-*.sh")
	if err != nil {
		return nil, err
	}
	b.genPath = genf.Name()
	b.genFile = genf

	if err := gen.Generate(genf, f); err != nil {
		genf.Close()
		os.Remove(b.genPath)
		return nil, err
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
func (b *Build) Resolve(name string) (*TargetNode, error) {
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
// node is created as long as the target has a default rule.
func (b *Build) ResolveVerb(target, verb string) (*TargetNode, error) {
	key := verbNodeKey{target, verb}
	if n, ok := b.verbNodes[key]; ok {
		return n, nil
	}
	var rule *parse.TargetRule
	if r, ok := b.verbConcretes[key]; ok {
		rule = r
	}
	if rule == nil {
		// Check for a matching verb pattern rule.
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
			if err := gen.GenerateVerbRule(b.genFile, instantiated); err != nil {
				return nil, fmt.Errorf("instantiate verb pattern [%s %s]: %w", verb, target, err)
			}
			b.verbConcretes[key] = instantiated
			rule = instantiated
			break
		}
	}
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

// findRule returns the rule for name, instantiating a pattern rule if needed.
func (b *Build) findRule(name string) (*parse.TargetRule, error) {
	if r, ok := b.concretes[name]; ok {
		return r, nil
	}
	for _, pe := range b.patterns {
		m := pe.re.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		captures := m[1:]
		rule := &parse.TargetRule{
			Type:   pe.rule.Type,
			Target: name,
			Runner: pe.rule.Runner,
			Body:   substituteCaptures(pe.rule.Body, captures),
		}
		for _, dep := range pe.rule.Deps {
			rule.Deps = append(rule.Deps, parse.Dep{
				Target: substituteCaptures(dep.Target, captures),
				Verb:   dep.Verb,
			})
		}
		if err := gen.GenerateRule(b.genFile, rule); err != nil {
			return nil, fmt.Errorf("instantiate pattern for %q: %w", name, err)
		}
		b.concretes[name] = rule
		return rule, nil
	}
	// No explicit or pattern rule: infer a source target so that real input files
	// don't need to be declared. source targets have no clean verb body, so they
	// are never deleted by `mmk clean`. Generate a bash function that delegates to
	// __mmk_default_source, which fails with a clear message if the file is absent.
	inferred := &parse.TargetRule{
		Type:   "source",
		Target: name,
		Body:   "\n\t" + gen.DefaultFunc("source") + "\n",
	}
	if err := gen.GenerateRule(b.genFile, inferred); err != nil {
		return nil, fmt.Errorf("infer file rule for %q: %w", name, err)
	}
	b.concretes[name] = inferred
	return inferred, nil
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

// verbDependencies resolves deps for a verb node. If the verb rule has explicit
// deps, those are used. Otherwise deps are inherited from the default rule with
// the same verb applied to each.
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

	// Inherit deps from the default rule, applying the same verb to each dep.
	defaultRule := n.build.concretes[n.target]
	if defaultRule == nil {
		// Try to instantiate a pattern rule first.
		n.build.Resolve(n.target) //nolint — side effect: populates concretes
		defaultRule = n.build.concretes[n.target]
	}
	if defaultRule == nil {
		return n.deps
	}
	for _, dep := range defaultRule.Deps {
		targets, err := n.build.expandDep(dep.Target)
		if err != nil {
			n.resolveErr = err
			return n.deps
		}
		for _, target := range targets {
			depNode, err := n.build.ResolveVerb(target, n.verb)
			if err != nil {
				n.resolveErr = err
				return n.deps
			}
			n.deps = append(n.deps, depNode)
		}
	}
	// If the verb rule itself has a runner, add runner + container for execution.
	// Otherwise propagate the verb to the default rule's runner (e.g. clean).
	if n.rule != nil && n.rule.Runner != "" {
		runnerNode, err := n.build.Resolve(n.rule.Runner)
		if err != nil {
			n.resolveErr = err
			return n.deps
		}
		n.deps = append(n.deps, runnerNode, n.build.runnerNode(runnerNode))
	} else if defaultRule.Runner != "" {
		runnerNode, err := n.build.ResolveVerb(defaultRule.Runner, n.verb)
		if err != nil {
			n.resolveErr = err
			return n.deps
		}
		n.deps = append(n.deps, runnerNode)
	}
	return n.deps
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
	if n.kind == kindRunner {
		// Runner nodes are pure setup. Returning zero time means they never
		// look "newer" than artifacts that depend on them.
		return time.Time{}
	}
	switch n.rule.Type {
	case "":
		return time.Now()
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
		n.build.runnerStatesMu.Lock()
		_, started := n.build.runnerStates[n.runnerFor.target]
		n.build.runnerStatesMu.Unlock()
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

// bodyFuncName returns the bash function name to call for this node's body
// and whether there is a body to run. It handles both verb and non-verb nodes:
//   - Non-verb: always gen.TargetFunc (the generated script embeds the defbody fallback).
//   - Verb with explicit body: gen.VerbTargetFunc.
//   - Verb with matching defbody: gen.DefaultVerbFunc.
//   - Verb with no body: ("", false) — no-op.
func (n *TargetNode) bodyFuncName() (string, bool) {
	if n.verb == "" {
		return gen.TargetFunc(n.target), true
	}
	if n.rule != nil && n.rule.Body != "" {
		return gen.VerbTargetFunc(n.verb, n.target), true
	}
	defaultRule := n.build.concretes[n.target]
	if defaultRule != nil && defaultRule.Type != "" {
		key := defVerbBodyKey{defaultRule.Type, n.verb}
		if n.build.defVerbBodies[key] {
			return gen.DefaultVerbFunc(defaultRule.Type, n.verb), true
		}
	}
	return "", false
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
	funcName, ok := n.bodyFuncName()
	if !ok {
		return nil
	}
	runner := ""
	if n.rule != nil {
		runner = n.rule.Runner
	}
	if runner != "" {
		return n.runWithRunner(funcName)
	}
	return n.runBash(funcName, true)
}

// runWithRunner executes funcName through the runner type's run bash function.
// The runner target's state (from setup) and the task context are passed as
// environment variables.
func (n *TargetNode) runWithRunner(funcName string) error {
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
	cmd.Env = append(os.Environ(),
		"MMK_GENFILE="+n.build.genPath,
		"target="+runnerNode.target,
		"deps="+strings.Join(runnerNode.explicitDepNames(), " "),
		"MMK_RUNNER_STATE="+state,
		"MMK_FUNC="+funcName,
		"MMK_TARGET="+n.target,
		"MMK_DEPS="+strings.Join(n.explicitDepNames(), " "),
	)
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

// expandDep returns the target names a dep resolves to. If dep starts with '$',
// it is expanded by sourcing the genfile and letting bash word-split the value.
// Otherwise it is returned as-is in a single-element slice.
func (b *Build) expandDep(dep string) ([]string, error) {
	if !strings.HasPrefix(dep, "$") {
		return []string{dep}, nil
	}
	cmd := exec.Command("bash", "-c", `. "$MMK_GENFILE"; echo `+dep)
	cmd.Env = append(os.Environ(), "MMK_GENFILE="+b.genPath)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("expand dep %q: %w", dep, err)
	}
	names := strings.Fields(string(out))
	if len(names) == 0 {
		return nil, fmt.Errorf("expand dep %q: empty expansion", dep)
	}
	return names, nil
}

// runBash sources generated.sh and runs cmd in a bash subprocess.
// If output is true, stdout/stderr are forwarded to the process's own streams.
// Target and dep names are passed via environment to avoid quoting issues.
func (n *TargetNode) runBash(cmd string, output bool) error {
	script := `. "$MMK_GENFILE"; target="$MMK_TARGET"; deps="$MMK_DEPS"; ` + cmd
	c := exec.Command("bash", "-c", script)
	c.Env = append(os.Environ(),
		"MMK_GENFILE="+n.build.genPath,
		"MMK_TARGET="+n.target,
		"MMK_DEPS="+strings.Join(n.explicitDepNames(), " "),
	)
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
	out, err := c.Output()
	return string(out), err
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

