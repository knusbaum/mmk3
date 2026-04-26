// Package runtime wires the parsed mmk AST to the dag executor.
// It resolves concrete and pattern targets, generates the bash function
// script on demand, and implements dag.Node via TargetNode.
package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/knusbaum/mmk3/cmd/mmk/gen"
	"github.com/knusbaum/mmk3/cmd/mmk/parse"
	"github.com/knusbaum/mmk3/dag"
)

// builtinTypes are type names implemented natively in Go.
// All other non-empty type names require a deftype definition.
var builtinTypes = map[string]bool{
	"file":  true,
	"image": true,
}

// runnerTypes are the type names that can appear after `on`. The dispatch in
// (*TargetNode).runOn picks the strategy based on the runner target's type.
// Add new entries here (and a case in runOn) to support more runner kinds.
var runnerTypes = map[string]bool{
	"image": true,
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
// and tear down any containers that were started for runner-based targets.
// Set Verbose = true before calling Execute to log each target as it runs or is skipped.
type Build struct {
	Verbose        bool
	concretes      map[string]*parse.TargetRule
	verbConcretes  map[verbNodeKey]*parse.TargetRule
	patterns       []*patternEntry
	nodes          map[string]*TargetNode
	verbNodes      map[verbNodeKey]*TargetNode
	containerNodes map[string]*TargetNode // image target name → synthetic container node
	defVerbBodies  map[defVerbBodyKey]bool
	genPath        string
	genFile        *os.File

	containersMu sync.Mutex
	containers   map[string]string // image target name → running container ID
}

type patternEntry struct {
	rule *parse.TargetRule
	re   *regexp.Regexp
}

// validateDirectives checks the AST against runtime constraints:
//   - defrunner is no longer supported (runner behavior is type-driven, not user-defined).
//   - Each TargetRule's type is either built-in or has a deftype.
//   - Each TargetRule's `on` clause names an existing concrete target whose
//     type appears in runnerTypes.
func validateDirectives(f *parse.File) error {
	type defBodyKey struct{ typ, verb string }
	deftypes := make(map[string]bool)
	defbodies := make(map[defBodyKey]bool)
	concretes := make(map[string]*parse.TargetRule)
	for _, d := range f.Directives {
		switch d := d.(type) {
		case *parse.DefRunner:
			return fmt.Errorf("defrunner is no longer supported; the `on` clause names a target and the runner behavior is determined by that target's type")
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
	for key := range defbodies {
		if !builtinTypes[key.typ] && !deftypes[key.typ] {
			return fmt.Errorf("defbody %q: unknown type (built-in types: file, image; define others with deftype)", key.typ)
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
		if r.Type != "" && !builtinTypes[r.Type] && !deftypes[r.Type] {
			return fmt.Errorf("target %q uses unknown type %q (built-in types: file, image; define others with deftype)", name, r.Type)
		}
		if r.Runner != "" {
			runner, ok := concretes[r.Runner]
			if !ok {
				return fmt.Errorf("target %q uses unknown runner target %q", name, r.Runner)
			}
			if !runnerTypes[runner.Type] {
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
		concretes:      make(map[string]*parse.TargetRule),
		verbConcretes:  make(map[verbNodeKey]*parse.TargetRule),
		nodes:          make(map[string]*TargetNode),
		verbNodes:      make(map[verbNodeKey]*TargetNode),
		containerNodes: make(map[string]*TargetNode),
		defVerbBodies:  make(map[defVerbBodyKey]bool),
		containers:     make(map[string]string),
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

// Close removes the temporary generated script and tears down any containers
// that were started during the build.
func (b *Build) Close() {
	b.containersMu.Lock()
	for _, id := range b.containers {
		exec.Command("docker", "rm", "-f", id).Run()
	}
	b.containers = nil
	b.containersMu.Unlock()
	b.genFile.Close()
	os.Remove(b.genPath)
}

// containerNode returns (creating once) the synthetic node that starts the
// container for the given image target. Multiple targets running `on
// imageTarget` share a single container node so the container starts once.
func (b *Build) containerNode(imageTarget *TargetNode) *TargetNode {
	if n, ok := b.containerNodes[imageTarget.target]; ok {
		return n
	}
	n := &TargetNode{
		build:        b,
		target:       "__container__" + imageTarget.target,
		kind:         kindContainer,
		containerFor: imageTarget,
	}
	b.containerNodes[imageTarget.target] = n
	return n
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
			if n.kind == kindContainer {
				fmt.Printf("starting container: %s\n", n.containerFor.target)
			} else if n.verb != "" {
				fmt.Printf("running: [%s %s]\n", n.verb, n.target)
			} else {
				fmt.Printf("running: %s\n", n.target)
			}
		},
		OnSkip: func(n *TargetNode) {
			if n.kind == kindContainer {
				return // container dedup is an internal detail, not user-visible
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
		// Require a default rule to exist so that dep inheritance can propagate.
		if _, ok := b.concretes[target]; !ok {
			found := false
			for _, pe := range b.patterns {
				if pe.re.MatchString(target) {
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
	// No explicit or pattern rule: infer a file target so that source files
	// don't need to be declared. Run() will fail if the file doesn't exist.
	inferred := &parse.TargetRule{Type: "file", Target: name}
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
// synthetic nodes the runtime adds to the DAG (currently just container
// startup nodes for runner-based execution).
type targetKind int

const (
	kindRule      targetKind = iota // backed by a parse.TargetRule
	kindContainer                   // synthetic: starts a container for an image target
)

// TargetNode is a dag.Node[*TargetNode]. Most TargetNodes are kindRule and
// model a user-declared target. Synthetic nodes (e.g. container startup) use
// other kinds and dispatch on `kind` inside the Date/NeedsRun/Run methods.
// When verb is non-empty the node represents a verb rule (e.g. [clean executable]).
type TargetNode struct {
	build        *Build
	target       string
	verb         string            // non-empty for verb nodes
	rule         *parse.TargetRule // nil when kind != kindRule, or for inherited verb nodes
	kind         targetKind
	containerFor *TargetNode // set when kind == kindContainer
	deps         []*TargetNode
	depsBuilt    bool
	resolveErr   error
}

// Dependencies resolves each named dep to a TargetNode, instantiating pattern
// rules as needed. Targets with `on R` get two implicit deps appended: the
// runner target R itself (for freshness) and the synthetic container node for
// R (for setup ordering). Any resolution error is stored and returned by Run.
func (n *TargetNode) Dependencies() []*TargetNode {
	if n.depsBuilt {
		return n.deps
	}
	n.depsBuilt = true

	if n.kind == kindContainer {
		n.deps = []*TargetNode{n.containerFor}
		return n.deps
	}

	if n.verb != "" {
		return n.verbDependencies()
	}

	for _, dep := range n.rule.Deps {
		var depNode *TargetNode
		var err error
		if dep.Verb != "" {
			depNode, err = n.build.ResolveVerb(dep.Target, dep.Verb)
		} else {
			depNode, err = n.build.Resolve(dep.Target)
		}
		if err != nil {
			n.resolveErr = err
			return n.deps
		}
		n.deps = append(n.deps, depNode)
	}
	if n.rule.Runner != "" {
		runnerNode, err := n.build.Resolve(n.rule.Runner)
		if err != nil {
			n.resolveErr = err
			return n.deps
		}
		n.deps = append(n.deps, runnerNode, n.build.containerNode(runnerNode))
	}
	return n.deps
}

// verbDependencies resolves deps for a verb node. If the verb rule has explicit
// deps, those are used. Otherwise deps are inherited from the default rule with
// the same verb applied to each.
func (n *TargetNode) verbDependencies() []*TargetNode {
	if n.rule != nil && len(n.rule.Deps) > 0 {
		for _, dep := range n.rule.Deps {
			var depNode *TargetNode
			var err error
			if dep.Verb != "" {
				depNode, err = n.build.ResolveVerb(dep.Target, dep.Verb)
			} else {
				depNode, err = n.build.Resolve(dep.Target)
			}
			if err != nil {
				n.resolveErr = err
				return n.deps
			}
			n.deps = append(n.deps, depNode)
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
		depNode, err := n.build.ResolveVerb(dep.Target, n.verb)
		if err != nil {
			n.resolveErr = err
			return n.deps
		}
		n.deps = append(n.deps, depNode)
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
	if n.kind == kindContainer {
		// Container nodes are pure setup. Returning the zero time means they
		// never look "newer" than artifacts that depend on them, so a fresh
		// container start does not force downstream rebuilds.
		return time.Time{}
	}
	switch n.rule.Type {
	case "":
		return time.Now()
	case "file":
		info, err := os.Stat(n.target)
		if err != nil {
			return time.Time{}
		}
		return info.ModTime()
	case "image":
		t, err := dockerImageDate(n.target)
		if err != nil {
			return time.Time{}
		}
		return t
	default: // user-defined type via deftype
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
	if n.kind == kindContainer {
		// Containers are short-lived (per-build). If we have not started one
		// yet for this image during this build, we need to.
		n.build.containersMu.Lock()
		_, started := n.build.containers[n.containerFor.target]
		n.build.containersMu.Unlock()
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

// dockerImageDate returns the creation time of a local docker image.
func dockerImageDate(name string) (time.Time, error) {
	out, err := exec.Command("docker", "inspect", "--format", "{{.Created}}", name).Output()
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, strings.TrimSpace(string(out)))
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

// Run executes the target's body. For kindContainer it starts the container
// for its image. For kindRule with `on R` it dispatches to runOn. Otherwise
// it runs the body in a local bash subprocess.
func (n *TargetNode) Run() error {
	if n.resolveErr != nil {
		return n.resolveErr
	}
	if n.kind == kindContainer {
		return n.startContainer()
	}
	if n.verb != "" {
		return n.runVerb()
	}
	if n.rule.Runner != "" {
		return n.runOn()
	}
	// A file target with no body can't be built — fail if the file is absent.
	if n.rule.Type == "file" && n.rule.Body == "" {
		if _, err := os.Stat(n.target); err != nil {
			return fmt.Errorf("file %q does not exist and has no rule to create it", n.target)
		}
		return nil
	}
	return n.runBash(gen.TargetFunc(n.target), true)
}

// runVerb executes a verb node's action. Priority:
//  1. Explicit verb rule with a body → call the generated VerbTargetFunc.
//  2. No explicit body, but defbody for (type, verb) → call DefaultVerbFunc.
//  3. Otherwise → no-op.
func (n *TargetNode) runVerb() error {
	if n.rule != nil && n.rule.Body != "" {
		return n.runBash(gen.VerbTargetFunc(n.verb, n.target), true)
	}
	defaultRule := n.build.concretes[n.target]
	if defaultRule != nil && defaultRule.Type != "" {
		key := defVerbBodyKey{defaultRule.Type, n.verb}
		if n.build.defVerbBodies[key] {
			return n.runBash(gen.DefaultVerbFunc(defaultRule.Type, n.verb), true)
		}
	}
	return nil
}

// runOn executes the body inside the environment provided by the runner
// target. The strategy is selected by the runner target's type — currently
// only "image" is supported. Add a case here to wire up new runner kinds.
func (n *TargetNode) runOn() error {
	runnerNode, err := n.build.Resolve(n.rule.Runner)
	if err != nil {
		return err
	}
	switch runnerNode.rule.Type {
	case "image":
		return n.runInDockerContainer(runnerNode.target)
	default:
		return fmt.Errorf("type %q is not a valid runner", runnerNode.rule.Type)
	}
}

// runInDockerContainer executes the target body via `docker exec` against the
// long-running container that the corresponding container node started for
// this build. The genfile is sourced from inside the container at the path
// the container node bind-mounted it to.
func (n *TargetNode) runInDockerContainer(image string) error {
	n.build.containersMu.Lock()
	containerID, ok := n.build.containers[image]
	n.build.containersMu.Unlock()
	if !ok {
		return fmt.Errorf("container for image %q is not running", image)
	}
	uid, gid, err := currentUIDGID()
	if err != nil {
		return err
	}
	inner := `. ` + containerGenfilePath + `; target="$MMK_TARGET"; deps="$MMK_DEPS"; ` + gen.TargetFunc(n.target)
	c := exec.Command("docker", "exec",
		"--user", uid+":"+gid,
		"-e", "MMK_TARGET="+n.target,
		"-e", "MMK_DEPS="+strings.Join(n.explicitDepNames(), " "),
		containerID,
		"bash", "-c", inner,
	)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

// containerGenfilePath is where startContainer bind-mounts the host genfile
// inside the container. Kept simple and fixed; users do not see this path.
const containerGenfilePath = "/mmk-generated.sh"

// startContainer launches the long-running container for this node's image
// target and registers the container ID with the Build for cleanup. The
// container does nothing on its own (`sleep infinity`); subsequent tasks
// docker exec into it.
func (n *TargetNode) startContainer() error {
	image := n.containerFor.target
	pwd, err := os.Getwd()
	if err != nil {
		return err
	}
	name := fmt.Sprintf("mmk-%s-%d", sanitizeContainerName(image), os.Getpid())
	// Best-effort: clean up any leftover container with the same name.
	exec.Command("docker", "rm", "-f", name).Run()

	out, err := exec.Command("docker", "run", "-d", "--rm",
		"--name", name,
		"-v", pwd+":/work",
		"-v", n.build.genPath+":"+containerGenfilePath+":ro",
		"-w", "/work",
		image,
		"sleep", "infinity",
	).Output()
	if err != nil {
		return fmt.Errorf("start container for image %q: %w", image, err)
	}
	containerID := strings.TrimSpace(string(out))
	n.build.containersMu.Lock()
	n.build.containers[image] = containerID
	n.build.containersMu.Unlock()
	return nil
}

// sanitizeContainerName makes an arbitrary string safe to use in a docker
// container name (which must match [a-zA-Z0-9][a-zA-Z0-9_.-]+).
func sanitizeContainerName(s string) string {
	var sb strings.Builder
	for i, r := range s {
		safe := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '.' || r == '-'
		if i == 0 && !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			sb.WriteByte('x')
		}
		if safe {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('-')
		}
	}
	return sb.String()
}

func currentUIDGID() (string, string, error) {
	u, err := user.Current()
	if err != nil {
		return "", "", err
	}
	return u.Uid, u.Gid, nil
}

// explicitDepNames returns just the target names from rule.Deps — not the
// implicit deps (runner target, container node) that Dependencies() appends.
// This is what `$deps` should expose to user bodies.
func (n *TargetNode) explicitDepNames() []string {
	if n.rule == nil {
		return nil
	}
	names := make([]string, len(n.rule.Deps))
	for i, dep := range n.rule.Deps {
		names[i] = dep.Target
	}
	return names
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
