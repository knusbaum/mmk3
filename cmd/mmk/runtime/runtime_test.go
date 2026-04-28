package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newBuild(t *testing.T, src string) *Build {
	t.Helper()
	b, err := NewBuild([]byte(src))
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}
	t.Cleanup(b.Close)
	return b
}

func readGenerated(t *testing.T, b *Build) string {
	t.Helper()
	data, err := os.ReadFile(b.genPath)
	if err != nil {
		t.Fatalf("read generated script: %v", err)
	}
	return string(data)
}

// touchAt creates a file at path with the given mtime.
func touchAt(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatalf("touchAt write %s: %v", path, err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("touchAt chtimes %s: %v", path, err)
	}
}

func depTargets(nodes []*TargetNode) []string {
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = n.target
	}
	return names
}

// --- resolution ---

func TestResolveConcreteTarget(t *testing.T) {
	b := newBuild(t, `all : foo`)
	n, err := b.Resolve("all")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if n.target != "all" {
		t.Errorf("target: got %q, want %q", n.target, "all")
	}
}

func TestResolveSameNodeReturned(t *testing.T) {
	b := newBuild(t, `all : foo`)
	n1, _ := b.Resolve("all")
	n2, _ := b.Resolve("all")
	if n1 != n2 {
		t.Error("expected same *TargetNode on repeated Resolve")
	}
}

func TestResolveUnknownInfersSourceType(t *testing.T) {
	b := newBuild(t, `all :`)
	n, err := b.Resolve("somefile.c")
	if err != nil {
		t.Fatalf("Resolve: expected inferred source node, got error: %v", err)
	}
	if n.rule.Type != "source" {
		t.Errorf("inferred type: got %q, want \"source\"", n.rule.Type)
	}
}

// --- verb propagation to inferred source targets ---

func TestVerbPropagationToInferredSource(t *testing.T) {
	// [clean main.o] should propagate to [clean main.c], but main.c is an
	// inferred source target not yet in concretes. ResolveVerb must not error.
	b := newBuild(t, `
file '(.*)\.o' : $1.c {
    cc -c $1.c -o $target
}
all : main.o
`)
	n, err := b.ResolveVerb("main.o", "clean")
	if err != nil {
		t.Fatalf("ResolveVerb main.o clean: %v", err)
	}
	deps := n.Dependencies()
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep ([clean main.c]), got %d: %v", len(deps), depTargets(deps))
	}
	if deps[0].target != "main.c" {
		t.Errorf("dep target: got %q, want %q", deps[0].target, "main.c")
	}
}

// --- variable expansion in deps ---

func TestVarDepExpansion(t *testing.T) {
	b := newBuild(t, `
ITEMS="foo bar"
foo :
bar :
all : $ITEMS
`)
	all, _ := b.Resolve("all")
	deps := all.Dependencies()
	if len(deps) != 2 {
		t.Fatalf("expected 2 deps from $ITEMS expansion, got %d: %v", len(deps), depTargets(deps))
	}
	if deps[0].target != "foo" || deps[1].target != "bar" {
		t.Errorf("deps: got %v, want [foo bar]", depTargets(deps))
	}
}

func TestVarTargetNameExpansion(t *testing.T) {
	b := newBuild(t, `
NAME=resolved-target
$NAME : {
    :
}
all : $NAME
`)
	if _, ok := b.concretes["resolved-target"]; !ok {
		t.Fatalf("expected concrete %q after $NAME expansion; got %v", "resolved-target", b.Targets())
	}
	all, _ := b.Resolve("all")
	deps := all.Dependencies()
	if len(deps) != 1 || deps[0].target != "resolved-target" {
		t.Errorf("deps: got %v, want [resolved-target]", depTargets(deps))
	}
}

func TestVarRunnerExpansion(t *testing.T) {
	b := newBuild(t, `
IMG=ubuntu
image $IMG :
build on $IMG : {
    :
}
`)
	r := b.concretes["build"]
	if r == nil {
		t.Fatalf("expected concrete %q", "build")
	}
	if r.Runner != "ubuntu" {
		t.Errorf("Runner: got %q, want %q", r.Runner, "ubuntu")
	}
}

func TestTargetOptionVisibleInBody(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	src := fmt.Sprintf(`mytarget mode=debug count=3 {
    printf '%%s %%s' "$mode" "$count" > %q
}
`, out)
	b := newBuild(t, src)
	n, _ := b.Resolve("mytarget")
	if err := n.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "debug 3" {
		t.Errorf("body output: got %q, want %q", got, "debug 3")
	}
}

func TestImageUserOptionLiteral(t *testing.T) {
	// A literal user= value is exposed to the runner-run body as $user.
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	src := fmt.Sprintf(`image fakeimg user=root skip_if=true :
build on fakeimg {
    printf '%%s' "$user" > %q
}
`, out)
	b := newBuild(t, src)
	defer b.Close()
	if err := b.Prepare("build", ""); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	n, _ := b.Resolve("build")
	for _, dep := range n.Dependencies() {
		if dep.kind == kindRunner {
			if err := dep.Run(); err != nil {
				t.Fatalf("setup: %v", err)
			}
		}
	}
	if err := n.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, _ := os.ReadFile(out)
	if string(got) != "root" {
		t.Errorf("got %q, want %q", got, "root")
	}
}

func TestImageSkipIfRunsBodyLocally(t *testing.T) {
	// skip_if=true unconditionally bypasses docker. The body should run via
	// the runner-run phase's local-eval branch, NOT via docker exec — so this
	// test passes even with no docker daemon available.
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	src := fmt.Sprintf(`image fakeimg skip_if=true :
build on fakeimg {
    printf 'ran' > %q
}
`, out)
	b := newBuild(t, src)
	defer b.Close()
	if err := b.Prepare("build", ""); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	n, _ := b.Resolve("build")
	for _, dep := range n.Dependencies() {
		if dep.kind == kindRunner {
			if err := dep.Run(); err != nil {
				t.Fatalf("setup: %v", err)
			}
		}
	}
	if err := n.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "ran" {
		t.Errorf("body output: got %q, want %q", got, "ran")
	}
}

func TestImageOptionShadowedByTargetOption(t *testing.T) {
	// When an image and a target both set the same option name, the runner-run
	// phase should see the target's value (last-write-wins on cmd.Env).
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")

	// Custom image type whose runner just writes $platform to a file. No
	// docker, no real container — the runner is just bash.
	src := fmt.Sprintf(`deftype fake-image {
    echo 1
}
defrunner fake-image setup {
    printf 'state'
}
defrunner fake-image {
    printf '%%s' "$platform" > %q
}
defrunner fake-image cleanup {
    :
}
fake-image myimg platform=image-value :
mytarget on myimg platform=target-value {
    :
}
`, out)
	b := newBuild(t, src)
	defer b.Close()
	n, _ := b.Resolve("mytarget")
	n.Dependencies()
	if err := b.Prepare("mytarget", ""); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	// Run setup so MMK_RUNNER_STATE is populated.
	for _, dep := range n.Dependencies() {
		if dep.kind == kindRunner {
			if err := dep.Run(); err != nil {
				t.Fatalf("setup: %v", err)
			}
		}
	}
	if err := n.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "target-value" {
		t.Errorf("expected target option to shadow image option; got %q", got)
	}
}

func TestVarTargetNameMultiWordError(t *testing.T) {
	_, err := NewBuild([]byte(`
NAMES="a b"
$NAMES : {
    :
}
`))
	if err == nil {
		t.Fatal("expected error for multi-word target-name expansion")
	}
}

func TestVarDepExpansionInVerbDeps(t *testing.T) {
	b := newBuild(t, `
ITEMS="foo bar"
foo :
bar :
all : $ITEMS
`)
	// Verb deps inherited from default rule should also expand $ITEMS.
	n, err := b.ResolveVerb("all", "clean")
	if err != nil {
		t.Fatalf("ResolveVerb: %v", err)
	}
	deps := n.Dependencies()
	if len(deps) != 2 {
		t.Fatalf("expected 2 verb deps from $ITEMS expansion, got %d: %v", len(deps), depTargets(deps))
	}
	if deps[0].target != "foo" || deps[1].target != "bar" {
		t.Errorf("verb deps: got %v, want [foo bar]", depTargets(deps))
	}
}

func TestVarDepExpansionInExplicitVerbDeps(t *testing.T) {
	b := newBuild(t, `
ITEMS="foo bar"
foo :
bar :
[clean all] : $ITEMS
`)
	n, err := b.ResolveVerb("all", "clean")
	if err != nil {
		t.Fatalf("ResolveVerb: %v", err)
	}
	deps := n.Dependencies()
	if len(deps) != 2 {
		t.Fatalf("expected 2 explicit verb deps from $ITEMS expansion, got %d: %v", len(deps), depTargets(deps))
	}
	if deps[0].target != "foo" || deps[1].target != "bar" {
		t.Errorf("verb deps: got %v, want [foo bar]", depTargets(deps))
	}
}

// --- dep resolution ---

func TestDependenciesResolved(t *testing.T) {
	b := newBuild(t, `
all : foo bar
foo :
bar :
`)
	all, _ := b.Resolve("all")
	deps := all.Dependencies()
	if len(deps) != 2 {
		t.Fatalf("expected 2 deps, got %d: %v", len(deps), depTargets(deps))
	}
	if deps[0].target != "foo" || deps[1].target != "bar" {
		t.Errorf("deps: got %v, want [foo bar]", depTargets(deps))
	}
}

func TestMissingFileDepFailsOnRun(t *testing.T) {
	b := newBuild(t, `all : nosuchfile.c`)
	all, _ := b.Resolve("all")
	deps := all.Dependencies()
	if len(deps) != 1 {
		t.Fatalf("expected 1 inferred dep, got %d", len(deps))
	}
	err := deps[0].Run()
	if err == nil {
		t.Fatal("expected Run to fail for missing inferred file")
	}
}

// --- pattern instantiation ---

func TestPatternTargetInstantiated(t *testing.T) {
	b := newBuild(t, `
'(.*)\.o' : $1.c
main.c :
`)
	n, err := b.Resolve("main.o")
	if err != nil {
		t.Fatalf("Resolve pattern target: %v", err)
	}
	if n.target != "main.o" {
		t.Errorf("target: got %q, want %q", n.target, "main.o")
	}
	deps := n.Dependencies()
	if len(deps) != 1 || deps[0].target != "main.c" {
		t.Errorf("deps: got %v, want [main.c]", depTargets(deps))
	}
}

func TestPatternInstantiationCached(t *testing.T) {
	b := newBuild(t, `'(.*)\.o' : $1.c`)
	n1, _ := b.Resolve("main.o")
	n2, _ := b.Resolve("main.o")
	if n1 != n2 {
		t.Error("expected same *TargetNode on repeated Resolve of pattern-matched target")
	}
}

func TestPatternMultipleCaptures(t *testing.T) {
	b := newBuild(t, `
'(.*)-(.*)\.o' : $1.c $2.c
foo.c :
bar.c :
`)
	n, err := b.Resolve("foo-bar.o")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	deps := n.Dependencies()
	if len(deps) != 2 {
		t.Fatalf("expected 2 deps, got %d: %v", len(deps), depTargets(deps))
	}
	if deps[0].target != "foo.c" || deps[1].target != "bar.c" {
		t.Errorf("deps: got %v, want [foo.c bar.c]", depTargets(deps))
	}
}

func TestPatternConcreteRuleTakesPrecedence(t *testing.T) {
	b := newBuild(t, `
'(.*)\.o' : $1.c
special.o :
`)
	n, err := b.Resolve("special.o")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(n.rule.Deps) != 0 {
		t.Errorf("expected concrete rule with no deps, got %v", n.rule.Deps)
	}
}

func TestPatternNoMatchInfersSource(t *testing.T) {
	b := newBuild(t, `'(.*)\.o' : $1.c`)
	n, err := b.Resolve("main.c")
	if err != nil {
		t.Fatalf("Resolve: expected inferred source node, got error: %v", err)
	}
	if n.rule.Type != "source" {
		t.Errorf("expected inferred source type, got %q", n.rule.Type)
	}
}

// A verb pattern rule must not be picked up by bare-target Resolve. Without
// the guard in findRule, a target like main.c reached as a normal build dep
// would match `[fmt '(.*\.[ch])']` and inherit its body, causing the verb to
// fire during a regular (non-verb) build.
func TestVerbPatternDoesNotApplyToBareResolve(t *testing.T) {
	b := newBuild(t, `[fmt '(.*\.[ch])'] { clang-format -i $target; }`)
	n, err := b.Resolve("main.c")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if n.rule.Type != "source" {
		t.Errorf("expected inferred source type, got %q with body %q", n.rule.Type, n.rule.Body)
	}
}

// --- generated script ---

func TestGeneratedScriptContainsBuiltinFunctions(t *testing.T) {
	b := newBuild(t, `all : foo`)
	content := readGenerated(t, b)
	// target bodies are passed via MMK_EXECUTE; generated script has shared infrastructure only
	if strings.Contains(content, "__mmk_target_all()") {
		t.Error("generated script should not contain per-target functions with MMK_EXECUTE design")
	}
	if !strings.Contains(content, "__mmk_type_file()") {
		t.Errorf("generated script missing built-in __mmk_type_file\n%s", content)
	}
}

// --- integration: NeedsRun and Run via bash ---

func TestNeedsRunTrueWithNoType(t *testing.T) {
	b := newBuild(t, `all :`)
	n, _ := b.Resolve("all")
	if !n.NeedsRun() {
		t.Error("phony target (no type) should always NeedsRun")
	}
}

func TestRunNoBody(t *testing.T) {
	b := newBuild(t, `all :`)
	n, _ := b.Resolve("all")
	if err := n.Run(); err != nil {
		t.Errorf("no-body target Run: %v", err)
	}
}

func TestRunWithBody(t *testing.T) {
	b := newBuild(t, `all {
	true
}`)
	n, _ := b.Resolve("all")
	if err := n.Run(); err != nil {
		t.Errorf("Run: %v", err)
	}
}

func TestRunWithBodyFailure(t *testing.T) {
	b := newBuild(t, `all {
	false
}`)
	n, _ := b.Resolve("all")
	if err := n.Run(); err == nil {
		t.Error("expected non-zero exit to return error")
	}
}

func TestNeedsRunFileUpToDate(t *testing.T) {
	dir := t.TempDir()
	dep := filepath.Join(dir, "src.c")
	tgt := filepath.Join(dir, "out.o")
	past := time.Now().Add(-time.Minute)
	touchAt(t, dep, past) // dep is older
	touchAt(t, tgt, time.Now())

	src := fmt.Sprintf("file %q : %q\nfile %q :", tgt, dep, dep)
	b := newBuild(t, src)
	n, _ := b.Resolve(tgt)
	n.Dependencies()
	if n.NeedsRun() {
		t.Error("expected NeedsRun() == false when target is newer than dep")
	}
}

func TestNeedsRunFileStale(t *testing.T) {
	dir := t.TempDir()
	dep := filepath.Join(dir, "src.c")
	tgt := filepath.Join(dir, "out.o")
	past := time.Now().Add(-time.Minute)
	touchAt(t, tgt, past) // target is older
	touchAt(t, dep, time.Now())

	src := fmt.Sprintf("file %q : %q\nfile %q :", tgt, dep, dep)
	b := newBuild(t, src)
	n, _ := b.Resolve(tgt)
	n.Dependencies()
	if !n.NeedsRun() {
		t.Error("expected NeedsRun() == true when dep is newer than target")
	}
}

func TestNeedsRunFileMissing(t *testing.T) {
	dir := t.TempDir()
	dep := filepath.Join(dir, "src.c")
	tgt := filepath.Join(dir, "out.o") // intentionally not created
	touchAt(t, dep, time.Now())

	src := fmt.Sprintf("file %q : %q\nfile %q :", tgt, dep, dep)
	b := newBuild(t, src)
	n, _ := b.Resolve(tgt)
	n.Dependencies()
	if !n.NeedsRun() {
		t.Error("expected NeedsRun() == true when target file doesn't exist")
	}
}

// --- validation errors ---

func TestDeftypeEpochDate(t *testing.T) {
	epoch := time.Now().Unix()
	src := fmt.Sprintf(`
deftype mytype {
	echo %d
}
mytype mytarget :
`, epoch)
	b := newBuild(t, src)
	n, _ := b.Resolve("mytarget")
	n.Dependencies()
	got := n.Date()
	want := time.Unix(epoch, 0)
	if !got.Equal(want) {
		t.Errorf("Date(): got %v, want %v", got, want)
	}
}

func TestDeftypeRFC3339Date(t *testing.T) {
	ts := time.Now().UTC().Truncate(time.Second)
	src := fmt.Sprintf(`
deftype mytype {
	echo %s
}
mytype mytarget :
`, ts.Format(time.RFC3339))
	b := newBuild(t, src)
	n, _ := b.Resolve("mytarget")
	n.Dependencies()
	got := n.Date()
	if !got.Equal(ts) {
		t.Errorf("Date(): got %v, want %v", got, ts)
	}
}

func TestDeftypeNonzeroExitMeansAbsent(t *testing.T) {
	src := `
deftype mytype {
	return 1
}
mytype mytarget :
`
	b := newBuild(t, src)
	n, _ := b.Resolve("mytarget")
	n.Dependencies()
	if !n.NeedsRun() {
		t.Error("deftype with non-zero exit should cause NeedsRun() == true")
	}
}

func TestErrorOnUnknownType(t *testing.T) {
	_, err := NewBuild([]byte(`custom all :`))
	if err == nil {
		t.Fatal("expected error for unknown type with no deftype")
	}
	if !strings.Contains(err.Error(), "custom") {
		t.Errorf("error should mention the type name: %v", err)
	}
}

func TestExecuteRunsDeps(t *testing.T) {
	src := `
all : dep

dep {
	true
}
`
	b := newBuild(t, src)
	if err := b.Execute("all", "", 1); err != nil {
		t.Errorf("Execute: %v", err)
	}
}

func TestDefBodyUsedForBodylessTarget(t *testing.T) {
	src := `
deftype mytype {
	echo 0
}
defbody mytype {
	true
}
mytype mytarget :
`
	b := newBuild(t, src)
	n, _ := b.Resolve("mytarget")
	if err := n.Run(); err != nil {
		t.Errorf("Run with defbody: %v", err)
	}
}

func TestDefBodyDuplicateError(t *testing.T) {
	src := `
deftype mytype {
	echo 0
}
defbody mytype { true }
defbody mytype { false }
`
	_, err := NewBuild([]byte(src))
	if err == nil {
		t.Fatal("expected error for duplicate defbody")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error should mention duplicate: %v", err)
	}
}

func TestDefBodyUnknownTypeError(t *testing.T) {
	_, err := NewBuild([]byte(`defbody nosuchtype { true }`))
	if err == nil {
		t.Fatal("expected error for defbody with unknown type")
	}
	if !strings.Contains(err.Error(), "nosuchtype") {
		t.Errorf("error should mention type name: %v", err)
	}
}

func TestDefrunnerIsAccepted(t *testing.T) {
	// A defrunner with a run body should be valid on its own.
	_, err := NewBuild([]byte(`defrunner myrunner { echo run }`))
	if err != nil {
		t.Fatalf("unexpected error for valid defrunner: %v", err)
	}
}

func TestDefrunnerSetupWithoutRunErrors(t *testing.T) {
	_, err := NewBuild([]byte(`defrunner myrunner setup { echo setup }`))
	if err == nil {
		t.Fatal("expected error for defrunner with setup but no run body")
	}
	if !strings.Contains(err.Error(), "myrunner") {
		t.Errorf("error should mention runner name: %v", err)
	}
}

func TestErrorOnUnknownRunnerTarget(t *testing.T) {
	_, err := NewBuild([]byte(`all on norunner :`))
	if err == nil {
		t.Fatal("expected error for unknown runner target")
	}
	if !strings.Contains(err.Error(), "norunner") {
		t.Errorf("error should mention runner name: %v", err)
	}
}

func TestErrorOnNonRunnableType(t *testing.T) {
	src := `
file somefile :
all on somefile :
`
	_, err := NewBuild([]byte(src))
	if err == nil {
		t.Fatal("expected error for non-runnable runner type")
	}
	if !strings.Contains(err.Error(), "file") {
		t.Errorf("error should mention the offending type: %v", err)
	}
}

func TestRunnerImplicitDeps(t *testing.T) {
	src := `
image buildimage : {
	true
}
all on buildimage : explicit_dep
explicit_dep :
`
	b := newBuild(t, src)
	n, _ := b.Resolve("all")
	deps := n.Dependencies()
	if len(deps) != 3 {
		t.Fatalf("expected 3 deps (explicit_dep, buildimage, container), got %d: %v", len(deps), depTargets(deps))
	}
	if deps[0].target != "explicit_dep" {
		t.Errorf("dep[0] target: got %q, want %q", deps[0].target, "explicit_dep")
	}
	if deps[1].target != "buildimage" {
		t.Errorf("dep[1] target: got %q, want %q", deps[1].target, "buildimage")
	}
	if deps[2].kind != kindRunner || deps[2].runnerFor != deps[1] {
		t.Errorf("dep[2] should be runner node for buildimage, got %+v", deps[2])
	}
}

func TestRunnerNodeDedup(t *testing.T) {
	src := `
image buildimage : {
	true
}
file a on buildimage :
file b on buildimage :
all : a b
`
	b := newBuild(t, src)
	a, _ := b.Resolve("a")
	bb, _ := b.Resolve("b")
	aDeps := a.Dependencies()
	bDeps := bb.Dependencies()
	// last dep is the runner node in both cases
	if aDeps[len(aDeps)-1] != bDeps[len(bDeps)-1] {
		t.Error("runner node should be shared between a and b")
	}
}

func TestRunnerNodeOnlyDepIsRunner(t *testing.T) {
	src := `
image buildimage : {
	true
}
file a on buildimage :
`
	b := newBuild(t, src)
	a, _ := b.Resolve("a")
	a.Dependencies()
	rn := b.runnerNodes["buildimage"]
	if rn == nil {
		t.Fatal("runner node was not created")
	}
	rnDeps := rn.Dependencies()
	if len(rnDeps) != 1 || rnDeps[0].target != "buildimage" {
		t.Errorf("runner node deps: got %v, want [buildimage]", depTargets(rnDeps))
	}
	if !rn.Date().IsZero() {
		t.Errorf("runner node Date() should be zero, got %v", rn.Date())
	}
}

func TestExecutePatternTarget(t *testing.T) {
	src := `
all : main.o

'(.*)\.o' {
	true
}
`
	b := newBuild(t, src)
	if err := b.Execute("all", "", 1); err != nil {
		t.Errorf("Execute with pattern target: %v", err)
	}
}

// --- verb rules ---

func TestResolveVerbExplicit(t *testing.T) {
	src := `
all :
[clean all] :
`
	b := newBuild(t, src)
	n, err := b.ResolveVerb("all", "clean")
	if err != nil {
		t.Fatalf("ResolveVerb: %v", err)
	}
	if n.target != "all" || n.verb != "clean" {
		t.Errorf("got target=%q verb=%q", n.target, n.verb)
	}
}

func TestResolveVerbInherited(t *testing.T) {
	b := newBuild(t, `all : dep
dep :`)
	n, err := b.ResolveVerb("all", "clean")
	if err != nil {
		t.Fatalf("ResolveVerb inherited: %v", err)
	}
	if n.rule != nil {
		t.Error("inherited verb node should have nil rule")
	}
}

func TestVerbDepInheritancePropagates(t *testing.T) {
	src := `
all : foo bar
foo :
bar :
`
	b := newBuild(t, src)
	n, _ := b.ResolveVerb("all", "clean")
	deps := n.Dependencies()
	if len(deps) != 2 {
		t.Fatalf("expected 2 inherited deps, got %d: %v", len(deps), depTargets(deps))
	}
	if deps[0].target != "foo" || deps[0].verb != "clean" {
		t.Errorf("deps[0]: got target=%q verb=%q", deps[0].target, deps[0].verb)
	}
	if deps[1].target != "bar" || deps[1].verb != "clean" {
		t.Errorf("deps[1]: got target=%q verb=%q", deps[1].target, deps[1].verb)
	}
}

func TestVerbExplicitDepsOverrideInheritance(t *testing.T) {
	src := `
all : foo bar
foo :
bar :
baz :
[clean all] : baz
`
	b := newBuild(t, src)
	n, _ := b.ResolveVerb("all", "clean")
	deps := n.Dependencies()
	if len(deps) != 1 || deps[0].target != "baz" {
		t.Errorf("expected explicit dep [baz], got %v", depTargets(deps))
	}
}

func TestVerbRunWithBody(t *testing.T) {
	src := `
all :
[clean all] : {
	true
}
`
	b := newBuild(t, src)
	n, _ := b.ResolveVerb("all", "clean")
	if err := n.Run(); err != nil {
		t.Errorf("Run verb with body: %v", err)
	}
}

func TestVerbRunNoOp(t *testing.T) {
	b := newBuild(t, `all :`)
	n, _ := b.ResolveVerb("all", "clean")
	if err := n.Run(); err != nil {
		t.Errorf("Run verb no-op: %v", err)
	}
}

func TestVerbRunWithDefBody(t *testing.T) {
	src := `
deftype mytype {
	echo 0
}
defbody mytype clean {
	true
}
mytype mytarget :
`
	b := newBuild(t, src)
	n, _ := b.ResolveVerb("mytarget", "clean")
	if err := n.Run(); err != nil {
		t.Errorf("Run verb via defbody: %v", err)
	}
}

func TestVerbNeedsRunAlwaysTrue(t *testing.T) {
	b := newBuild(t, `all :`)
	n, _ := b.ResolveVerb("all", "clean")
	n.Dependencies()
	if !n.NeedsRun() {
		t.Error("verb node NeedsRun should always be true")
	}
}

func TestExecuteVerb(t *testing.T) {
	src := `
all : dep
dep :
[clean all] : [clean dep] {
	true
}
[clean dep] : {
	true
}
`
	b := newBuild(t, src)
	if err := b.Execute("all", "clean", 1); err != nil {
		t.Errorf("Execute verb: %v", err)
	}
}

func TestHasTarget(t *testing.T) {
	b := newBuild(t, `
all : foo
foo :
`)
	if !b.HasTarget("all") {
		t.Error("HasTarget(all) should be true")
	}
	if !b.HasTarget("foo") {
		t.Error("HasTarget(foo) should be true")
	}
	if b.HasTarget("missing") {
		t.Error("HasTarget(missing) should be false")
	}
}

func TestVerbInheritsRunnerVerbButNotRunnerOn(t *testing.T) {
	// An inherited verb-rule on a target with `on R`:
	//   - The verb is propagated to R (regular dep on [verb R]) so cleaning
	//     a target also reaches the underlying build infrastructure.
	//   - The `on R` itself is NOT inherited: the verb body runs locally,
	//     not inside R. Otherwise destructive verbs would race against R.
	src := `
image myimage : {
	true
}
file target on myimage : dep
dep :
`
	b := newBuild(t, src)
	n, err := b.ResolveVerb("target", "clean")
	if err != nil {
		t.Fatalf("ResolveVerb: %v", err)
	}
	deps := n.Dependencies()
	var sawCleanMyimage bool
	for _, d := range deps {
		if d.target == "myimage" && d.verb == "" {
			t.Errorf("[clean target] should not inherit `on myimage`; got bare myimage in deps: %v", depTargets(deps))
		}
		if d.target == "myimage" && d.verb == "clean" {
			sawCleanMyimage = true
		}
	}
	if !sawCleanMyimage {
		t.Errorf("expected [clean myimage] in [clean target] deps; got %v", depTargets(deps))
	}
}

func TestOrderOnlyBuiltinImageCleanAfterConsumers(t *testing.T) {
	// Built-in `defbody image clean` ships order=after-consumers. The
	// consumers are the nodes whose body actually runs inside the image:
	//   - default-build rules with `on myimg` (the bare `src`/`preload` nodes)
	//   - explicit verb-rules with their own `on myimg` (any verb)
	// Inherited verbs whose body runs locally are NOT consumers under the
	// new "verbs don't inherit `on`" model.
	src := `
image myimg : Dockerfile
src on myimg : { :; }
preload on myimg : { :; }
[lint src] on myimg { :; }
`
	b := newBuild(t, src)
	n, err := b.ResolveVerb("myimg", "clean")
	if err != nil {
		t.Fatalf("ResolveVerb: %v", err)
	}
	orderDeps := n.OrderDependencies()
	got := make(map[string]bool)
	for _, d := range orderDeps {
		got[d.target+":"+d.verb] = true
	}
	// Default-build consumers are the bare nodes themselves.
	for _, want := range []string{"src:", "preload:"} {
		if !got[want] {
			t.Errorf("expected %q in OrderDependencies of [clean myimg]; got %v", want, got)
		}
	}
	// Explicit verb rules with their own `on myimg` are also consumers,
	// regardless of which verb they are.
	if !got["src:lint"] {
		t.Errorf("expected %q in OrderDependencies of [clean myimg]; got %v", "src:lint", got)
	}
	// Regular Dependencies should NOT include consumers (otherwise standalone
	// `mmk clean myimg` would pull them in).
	for _, d := range n.Dependencies() {
		if d.target == "src" || d.target == "preload" {
			t.Errorf("regular Dependencies of [clean myimg] should not include consumer %q", d.target)
		}
	}
}

// hasDep reports whether deps contains a node with the given verb/target.
func hasDep(deps []*TargetNode, verb, target string) bool {
	for _, d := range deps {
		if d.target == target && d.verb == verb {
			return true
		}
	}
	return false
}

// TestVerbPropagation_InheritedNoCycle covers Case 1: `target on R` with no
// explicit verb-rule. `mmk clean target` propagates [clean R] as a regular
// dep but does NOT inherit `on R` (body runs locally), and the resulting
// graph has no cycle even though `[clean R]` ships order=after-consumers.
func TestVerbPropagation_InheritedNoCycle(t *testing.T) {
	src := `
image R : Dockerfile
file target on R : main.o
file main.o : main.c
`
	b := newBuild(t, src)
	if err := b.Prepare("target", "clean"); err != nil {
		t.Fatalf("Prepare(clean target) returned error (cycle?): %v", err)
	}
	n, err := b.ResolveVerb("target", "clean")
	if err != nil {
		t.Fatalf("ResolveVerb: %v", err)
	}
	deps := n.Dependencies()
	if !hasDep(deps, "clean", "R") {
		t.Errorf("expected [clean R] in [clean target] deps; got %v", depTargets(deps))
	}
	if hasDep(deps, "", "R") {
		t.Errorf("[clean target] should not inherit `on R`; got bare R in deps: %v", depTargets(deps))
	}
}

// TestVerbPropagation_ExplicitOnRunnerNoPropagate covers Case 2:
// `[clean target] on R` (own runner == default runner). The verb-rule body
// runs in R, so [clean R] must NOT be a regular dep of [clean target] —
// otherwise the order=after-consumers edge would cycle against it.
func TestVerbPropagation_ExplicitOnRunnerNoPropagate(t *testing.T) {
	src := `
image R : Dockerfile
file target on R : main.o
file main.o : main.c
[clean target] on R {
	rm -f /opt/$target
}
`
	b := newBuild(t, src)
	n, err := b.ResolveVerb("target", "clean")
	if err != nil {
		t.Fatalf("ResolveVerb: %v", err)
	}
	deps := n.Dependencies()
	if hasDep(deps, "clean", "R") {
		t.Errorf("[clean target] on R should NOT regular-dep on [clean R]; got %v", depTargets(deps))
	}
	if !hasDep(deps, "", "R") {
		t.Errorf("[clean target] on R should regular-dep on R (build); got %v", depTargets(deps))
	}
	if err := b.Prepare("target", "clean"); err != nil {
		t.Fatalf("Prepare(clean target) returned error: %v", err)
	}
}

// TestVerbPropagation_AfterConsumersFiresOnExplicitOn covers Case 3: when
// both [clean target] (explicit on R) and [clean R] are in the graph, the
// after-consumers order edge sequences [clean target] before [clean R].
func TestVerbPropagation_AfterConsumersFiresOnExplicitOn(t *testing.T) {
	src := `
image R : Dockerfile
file target on R : main.o
file main.o : main.c
[clean target] on R {
	rm -f /opt/$target
}
all : target
[clean all] :+ [clean R]
`
	b := newBuild(t, src)
	if err := b.Prepare("all", "clean"); err != nil {
		t.Fatalf("Prepare(clean all) returned error: %v", err)
	}
	cleanR, err := b.ResolveVerb("R", "clean")
	if err != nil {
		t.Fatalf("ResolveVerb([clean R]): %v", err)
	}
	if !hasDep(cleanR.OrderDependencies(), "clean", "target") {
		t.Errorf("expected [clean target] in [clean R].OrderDependencies; got %v",
			depTargets(cleanR.OrderDependencies()))
	}
}

// TestVerbPropagation_CrossRunner covers Case 4: `target on R` with
// `[clean target] on S` (different runner). The verb body runs in S, so
// [clean target] auto-propagates [clean R] (the default's runner) as a
// regular dep, AND when [clean S] is in the graph, the order edge fires
// because `target` has Runner==R but the `[clean target]` verb-rule has
// Runner==S — both endpoints sequence relative to their own runners.
func TestVerbPropagation_CrossRunner(t *testing.T) {
	src := `
image R : Dockerfile
image S : Dockerfile
file target on R : main.o
file main.o : main.c
[clean target] on S {
	echo cross-runner clean
}
all : target
[clean all] :+ [clean S]
`
	b := newBuild(t, src)
	if err := b.Prepare("all", "clean"); err != nil {
		t.Fatalf("Prepare(clean all) returned error: %v", err)
	}
	cleanTarget, err := b.ResolveVerb("target", "clean")
	if err != nil {
		t.Fatalf("ResolveVerb([clean target]): %v", err)
	}
	deps := cleanTarget.Dependencies()
	if !hasDep(deps, "clean", "R") {
		t.Errorf("[clean target] on S should auto-propagate [clean R]; got %v", depTargets(deps))
	}
	if !hasDep(deps, "", "S") {
		t.Errorf("[clean target] on S should regular-dep on S (build); got %v", depTargets(deps))
	}
	cleanS, err := b.ResolveVerb("S", "clean")
	if err != nil {
		t.Fatalf("ResolveVerb([clean S]): %v", err)
	}
	if !hasDep(cleanS.OrderDependencies(), "clean", "target") {
		t.Errorf("expected [clean target] in [clean S].OrderDependencies; got %v",
			depTargets(cleanS.OrderDependencies()))
	}
}

// TestVerbPropagation_NonVerbConsumerOrdered covers the user's specific
// example: when a build of `target on R` is in the graph alongside [clean R]
// (e.g. as siblings under a parent), [clean R] must order after the build
// of `target` so the image isn't removed mid-build.
func TestVerbPropagation_NonVerbConsumerOrdered(t *testing.T) {
	src := `
image R : Dockerfile
file target on R : main.o
file main.o : main.c
all : target [clean R]
`
	b := newBuild(t, src)
	cleanR, err := b.ResolveVerb("R", "clean")
	if err != nil {
		t.Fatalf("ResolveVerb([clean R]): %v", err)
	}
	if !hasDep(cleanR.OrderDependencies(), "", "target") {
		t.Errorf("expected bare `target` in [clean R].OrderDependencies; got %v",
			depTargets(cleanR.OrderDependencies()))
	}
}

func TestOrderOptionRejectedWithoutDefrunner(t *testing.T) {
	_, err := NewBuild([]byte(`
deftype mytype { echo 0 }
defbody mytype clean order=after-consumers { true }
`))
	if err == nil || !strings.Contains(err.Error(), "defrunner") {
		t.Fatalf("expected error mentioning 'defrunner'; got %v", err)
	}
}

func TestSubprojectExpandsToTopLevelRules(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "sub")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Sub-mmkfile declares a couple verbs and a typed target so we get
	// the built-in 'clean' verb too.
	subSrc := `
all : foo
file foo :
[test foo] {
    :
}
[fmt foo] {
    :
}
`
	if err := os.WriteFile(filepath.Join(subDir, "mmkfile"), []byte(subSrc), 0644); err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)

	src := `
all : sub
subproject sub
`
	b := newBuild(t, src)

	// Default-build target was created.
	if _, ok := b.concretes["sub"]; !ok {
		t.Fatalf("expected 'sub' in concretes; got %v", b.Targets())
	}
	// Verb-rules for the harvested verbs were created.
	for _, verb := range []string{"clean", "fmt", "test"} {
		if _, ok := b.verbConcretes[verbNodeKey{"sub", verb}]; !ok {
			t.Errorf("expected [%s sub] to be registered", verb)
		}
	}
}

func TestSubprojectResolveSubpath(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "sub")
	os.MkdirAll(subDir, 0755)
	subSrc := `
all : foo
file foo :
[fmt foo] {
    :
}
`
	os.WriteFile(filepath.Join(subDir, "mmkfile"), []byte(subSrc), 0644)
	cwd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(cwd)

	b := newBuild(t, `subproject sub`)

	// Default verb: `mmk sub/foo` should register `sub/foo` as a concrete.
	if !b.ResolveSubpath("sub/foo", "") {
		t.Fatal("ResolveSubpath returned false for known subproject prefix")
	}
	r, ok := b.concretes["sub/foo"]
	if !ok {
		t.Fatal("expected synthesized rule for sub/foo")
	}
	if !strings.Contains(r.Body, `cd "sub"`) || !strings.Contains(r.Body, "mmk foo") {
		t.Errorf("body looks wrong: %q", r.Body)
	}

	// Verb form: `mmk fmt sub/foo`.
	if !b.ResolveSubpath("sub/foo", "fmt") {
		t.Fatal("ResolveSubpath returned false for fmt sub/foo")
	}
	r2, ok := b.verbConcretes[verbNodeKey{"sub/foo", "fmt"}]
	if !ok {
		t.Fatal("expected synthesized [fmt sub/foo] rule")
	}
	if !strings.Contains(r2.Body, "mmk fmt foo") {
		t.Errorf("verb body looks wrong: %q", r2.Body)
	}

	// Unknown prefix: returns false.
	if b.ResolveSubpath("not-a-sub/anything", "") {
		t.Error("ResolveSubpath should return false for unknown prefix")
	}

	// subprojectDelegate helper: feeds the -graph -full sub-process spawn.
	cases := []struct {
		target, verb string
		wantPath     string
		wantArgs     []string
	}{
		{"sub", "", "sub", nil},
		{"sub", "fmt", "sub", []string{"fmt"}},
		{"sub/foo", "", "sub", []string{"foo"}},
		{"sub/foo", "fmt", "sub", []string{"fmt", "foo"}},
	}
	for _, tc := range cases {
		path, args, ok := b.subprojectDelegate(tc.target, tc.verb)
		if !ok || path != tc.wantPath || !equalStrSlice(args, tc.wantArgs) {
			t.Errorf("subprojectDelegate(%q,%q) = (%q, %v, %v); want (%q, %v, true)",
				tc.target, tc.verb, path, args, ok, tc.wantPath, tc.wantArgs)
		}
	}
	if _, _, ok := b.subprojectDelegate("not-a-sub/foo", ""); ok {
		t.Error("subprojectDelegate should return ok=false for unknown subproject prefix")
	}
}

func equalStrSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSubprojectMissingMmkfileErrors(t *testing.T) {
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)
	_, err := NewBuild([]byte(`subproject does-not-exist`))
	if err == nil {
		t.Fatal("expected error for missing sub-mmkfile")
	}
}

func TestHasVerbRecognizesDefinedVerbs(t *testing.T) {
	b := newBuild(t, `
foo :
[fmt foo] {
    :
}
`)
	if !b.HasVerb("fmt") {
		t.Error("expected HasVerb(\"fmt\") = true")
	}
	if b.HasVerb("nonexistent") {
		t.Error("expected HasVerb(\"nonexistent\") = false")
	}
}

func TestExecuteRejectsVerbWithNoApplicableRule(t *testing.T) {
	// `foop` is defined for `weird`, but not for anything in `all`'s subtree.
	src := `
all : foo
foo :
weird {
    :
}
[foop weird] {
    :
}
`
	b := newBuild(t, src)
	err := b.Execute("all", "foop", 0)
	if err == nil || !strings.Contains(err.Error(), "no applicable rule") {
		t.Fatalf("expected 'no applicable rule' error, got %v", err)
	}
}

func TestGraphPrunesEmptyVerbSubtrees(t *testing.T) {
	// `all` has deps `foo` (with [test foo] rule) and `bar` (no [test bar]).
	// `mmk -graph test all` should prune [test bar] but keep [test foo].
	src := `
all : foo bar
foo :
bar :
[test foo] {
    :
}
`
	b := newBuild(t, src)
	defer b.Close()
	var buf strings.Builder
	if err := b.GraphTo(&buf, "all", "test", false); err != nil {
		t.Fatalf("Graph: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "[test foo]") {
		t.Errorf("expected [test foo] in graph; got:\n%s", out)
	}
	if strings.Contains(out, "[test bar]") {
		t.Errorf("[test bar] should be pruned; got:\n%s", out)
	}
}

func TestExecuteAllowsVerbWhenAtLeastOneRuleApplies(t *testing.T) {
	// `clean` only applies to `foo` directly, not via [clean all] explicitly.
	src := `
all : foo
foo :
[clean foo] {
    :
}
`
	b := newBuild(t, src)
	if err := b.Execute("all", "clean", 0); err != nil {
		t.Errorf("expected verb-inheritance to succeed, got %v", err)
	}
}

func TestVerbAugmentDepsCombinesWithInherited(t *testing.T) {
	// `[verb t] :+ extra` combines explicit deps with the default rule's
	// inherited deps.
	src := `
all : a b
[clean all] :+ extra
a :
b :
extra :
`
	b := newBuild(t, src)
	n, err := b.ResolveVerb("all", "clean")
	if err != nil {
		t.Fatalf("ResolveVerb: %v", err)
	}
	got := depTargets(n.Dependencies())
	want := []string{"extra", "a", "b"}
	if len(got) != len(want) {
		t.Fatalf("deps: got %v, want %v", got, want)
	}
	for i, name := range want {
		if got[i] != name {
			t.Errorf("Deps[%d]: got %q, want %q (full: %v)", i, got[i], name, got)
		}
	}
}


func TestVerbPatternBodyRunsWhenDepHasRunner(t *testing.T) {
	tmp := t.TempDir()
	marker := filepath.Join(tmp, "ran")
	src := fmt.Sprintf(`
image myimage : {
	true
}
file executable on myimage : main.o {
	true
}
[check '(.*)\.o'] : {
	touch %s
}
'(.*)\.o' :
`, marker)
	b := newBuild(t, src)
	// Execute check all — verb nodes only, no docker needed.
	// [check main.o] should touch the marker file.
	b.Execute("executable", "check", 1) //nolint — may fail due to container; we only care about the marker
	if _, err := os.Stat(marker); os.IsNotExist(err) {
		t.Error("verb pattern body did not execute — marker file not created")
	}
}

func TestVerbPatternRuleRunsBody(t *testing.T) {
	src := `
all : main.o
'(.*)\.o' : $1.c
[check '(.*)\.o'] : {
	true
}
`
	b := newBuild(t, src)
	n, err := b.ResolveVerb("main.o", "check")
	if err != nil {
		t.Fatalf("ResolveVerb verb pattern: %v", err)
	}
	n.Dependencies()
	if err := n.Run(); err != nil {
		t.Errorf("Run verb pattern: %v", err)
	}
}

func TestVerbPatternRuleWithOnHasRunnerAndDeps(t *testing.T) {
	src := `
image myimage : {
	true
}
'(.*)\.o' : $1.c
[check '(.*)\.o'] on myimage {
	true
}
`
	b := newBuild(t, src)
	n, err := b.ResolveVerb("main.o", "check")
	if err != nil {
		t.Fatalf("ResolveVerb: %v", err)
	}
	if n.rule == nil {
		t.Fatal("expected non-nil rule for verb pattern with on clause")
	}
	if n.rule.Runner != "myimage" {
		t.Errorf("instantiated rule Runner: got %q, want %q", n.rule.Runner, "myimage")
	}
	deps := n.Dependencies()
	var foundRunner, foundRunnerNode bool
	for _, dep := range deps {
		if dep.target == "myimage" && dep.kind == kindRule {
			foundRunner = true
		}
		if dep.kind == kindRunner && dep.runnerFor != nil && dep.runnerFor.target == "myimage" {
			foundRunnerNode = true
		}
	}
	if !foundRunner {
		t.Errorf("expected myimage runner in deps, got %v", depTargets(deps))
	}
	if !foundRunnerNode {
		t.Errorf("expected runner node for myimage in deps, got %v", depTargets(deps))
	}
}

// --- matrix expansion ---

func TestMatrixExpansionCreatesCombos(t *testing.T) {
	b := newBuild(t, `build for os in [linux macos] { echo $os }`)
	// Aggregator replaces original.
	if _, ok := b.concretes["build"]; !ok {
		t.Fatal("expected aggregator concrete 'build'")
	}
	// Both combo targets registered.
	linuxName := comboTargetName("build", matrixCombo{"os": "linux"})
	macosName := comboTargetName("build", matrixCombo{"os": "macos"})
	if _, ok := b.concretes[linuxName]; !ok {
		t.Errorf("expected combo concrete %q", linuxName)
	}
	if _, ok := b.concretes[macosName]; !ok {
		t.Errorf("expected combo concrete %q", macosName)
	}
}

func TestMatrixCrossProduct(t *testing.T) {
	b := newBuild(t, `build for os in [linux macos] for libc in [musl glibc] { :; }`)
	want := []matrixCombo{
		{"os": "linux", "libc": "musl"},
		{"os": "linux", "libc": "glibc"},
		{"os": "macos", "libc": "musl"},
		{"os": "macos", "libc": "glibc"},
	}
	for _, combo := range want {
		name := comboTargetName("build", combo)
		if _, ok := b.concretes[name]; !ok {
			t.Errorf("expected combo concrete %q", name)
		}
	}
	info := b.matrixInfo["build"]
	if info == nil {
		t.Fatal("expected matrixInfo for 'build'")
	}
	if len(info.combos) != 4 {
		t.Errorf("combos: got %d, want 4", len(info.combos))
	}
}

func TestMatrixAggregatorDependsOnAllCombos(t *testing.T) {
	b := newBuild(t, `build for os in [linux macos] for libc in [musl glibc] { :; }`)
	agg, err := b.Resolve("build")
	if err != nil {
		t.Fatalf("Resolve aggregator: %v", err)
	}
	deps := agg.Dependencies()
	if len(deps) != 4 {
		t.Fatalf("aggregator deps: got %d, want 4: %v", len(deps), depTargets(deps))
	}
}

func TestMatrixExclude(t *testing.T) {
	b := newBuild(t, `build for os in [linux macos] for libc in [musl glibc] exclude [os=macos libc=musl] { :; }`)
	// 4 combos minus 1 excluded = 3.
	info := b.matrixInfo["build"]
	if info == nil {
		t.Fatal("expected matrixInfo")
	}
	if len(info.combos) != 3 {
		t.Errorf("combos after exclude: got %d, want 3", len(info.combos))
	}
	excluded := comboTargetName("build", matrixCombo{"os": "macos", "libc": "musl"})
	if _, ok := b.concretes[excluded]; ok {
		t.Errorf("excluded combo %q should not be in concretes", excluded)
	}
}

func TestMatrixExcludePartial(t *testing.T) {
	// Partial exclude [os=macos] should drop both macos combos.
	b := newBuild(t, `build for os in [linux macos] for libc in [musl glibc] exclude [os=macos] { :; }`)
	info := b.matrixInfo["build"]
	if len(info.combos) != 2 {
		t.Errorf("combos after partial exclude: got %d, want 2", len(info.combos))
	}
}

func TestMatrixVarsExportedToBody(t *testing.T) {
	dir := t.TempDir()
	out := dir + "/out.txt"
	src := fmt.Sprintf(`build for os in [linux] for libc in [musl] {
    printf '%%s %%s' "$os" "$libc" > %q
}`, out)
	b := newBuild(t, src)
	name := comboTargetName("build", matrixCombo{"os": "linux", "libc": "musl"})
	n, err := b.Resolve(name)
	if err != nil {
		t.Fatalf("Resolve combo: %v", err)
	}
	if err := n.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(got) != "linux musl" {
		t.Errorf("body output: got %q, want %q", string(got), "linux musl")
	}
}

func TestMatrixPlainDepResolvesToAggregator(t *testing.T) {
	// Plain dep on a matrix target resolves to the aggregator, regardless of
	// whether the caller is also a matrix rule or what dimensions they share.
	b := newBuild(t, `
build for os in [linux macos] for libc in [musl glibc] { :; }
test for os in [linux macos] : build { :; }
`)
	testLinux := comboTargetName("test", matrixCombo{"os": "linux"})
	n, err := b.Resolve(testLinux)
	if err != nil {
		t.Fatalf("Resolve %q: %v", testLinux, err)
	}
	deps := n.Dependencies()
	if len(deps) != 1 {
		t.Fatalf("test[os=linux] deps: got %d, want 1 (aggregator): %v", len(deps), depTargets(deps))
	}
	if deps[0].target != "build" {
		t.Errorf("dep: got %q, want %q", deps[0].target, "build")
	}
}

func TestMatrixSingletonDep(t *testing.T) {
	// A non-matrix dep from a matrix node should resolve to the singleton directly.
	b := newBuild(t, `
artifacts { :; }
test for os in [linux macos] : artifacts { :; }
`)
	testLinux := comboTargetName("test", matrixCombo{"os": "linux"})
	n, err := b.Resolve(testLinux)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	deps := n.Dependencies()
	if len(deps) != 1 {
		t.Fatalf("deps: got %d, want 1: %v", len(deps), depTargets(deps))
	}
	if deps[0].target != "artifacts" {
		t.Errorf("dep target: got %q, want %q", deps[0].target, "artifacts")
	}
}

func TestMatrixExplicitComboDep(t *testing.T) {
	// [build @ os=linux libc=musl] resolves to exactly that one combo.
	b := newBuild(t, `
build for os in [linux macos] for libc in [musl glibc] { :; }
foo : [build @ os=linux libc=musl] { :; }
`)
	n, err := b.Resolve("foo")
	if err != nil {
		t.Fatalf("Resolve foo: %v", err)
	}
	deps := n.Dependencies()
	if len(deps) != 1 {
		t.Fatalf("deps: got %d, want 1: %v", len(deps), depTargets(deps))
	}
	want := comboTargetName("build", matrixCombo{"os": "linux", "libc": "musl"})
	if deps[0].target != want {
		t.Errorf("dep: got %q, want %q", deps[0].target, want)
	}
}

func TestMatrixExplicitComboDepZeroMatchErrors(t *testing.T) {
	// Explicit combo dep with no matching combos should error, not silently add zero deps.
	b := newBuild(t, `
build for os in [linux macos] { :; }
foo : [build @ os=windows] { :; }
`)
	n, err := b.Resolve("foo")
	if err != nil {
		t.Fatalf("Resolve foo: %v", err)
	}
	deps := n.Dependencies()
	_ = deps
	if n.resolveErr == nil {
		t.Fatal("expected error when explicit combo dep matches no combos")
	}
}

func TestMatrixExplicitComboDepDisjointRangeErrors(t *testing.T) {
	// [extra @ x=$x] where the dep's x values and caller's x values are disjoint
	// should error, not silently add zero deps.
	b := newBuild(t, `
extra for x in [6 7 8 9] { :; }
base for x in [1 2 3] : [extra @ x=$x] { :; }
`)
	baseName := comboTargetName("base", matrixCombo{"x": "1"})
	n, err := b.Resolve(baseName)
	if err != nil {
		t.Fatalf("Resolve %q: %v", baseName, err)
	}
	deps := n.Dependencies()
	_ = deps
	if n.resolveErr == nil {
		t.Fatal("expected error when $x substitution matches no extra combos")
	}
}

func TestMatrixRunnerVarSubstitution(t *testing.T) {
	// on runner-$os should be substituted per combo.
	b := newBuild(t, `
image runner-linux skip_if=true :
image runner-macos skip_if=true :
build for os in [linux macos] on runner-$os { :; }
`)
	linuxName := comboTargetName("build", matrixCombo{"os": "linux"})
	macosName := comboTargetName("build", matrixCombo{"os": "macos"})

	linuxRule := b.concretes[linuxName]
	if linuxRule == nil {
		t.Fatalf("combo %q not found", linuxName)
	}
	if linuxRule.Runner != "runner-linux" {
		t.Errorf("linux combo runner: got %q, want %q", linuxRule.Runner, "runner-linux")
	}

	macosRule := b.concretes[macosName]
	if macosRule == nil {
		t.Fatalf("combo %q not found", macosName)
	}
	if macosRule.Runner != "runner-macos" {
		t.Errorf("macos combo runner: got %q, want %q", macosRule.Runner, "runner-macos")
	}
}

func TestMatrixVerbInheritsMatrix(t *testing.T) {
	// [clean build] on a matrix rule should apply to all combos.
	b := newBuild(t, `
build for os in [linux macos] { :; }
[clean build] { rm -f $os }
`)
	// Resolving [clean build] should reach the aggregator, and verb deps
	// should propagate to both combos.
	n, err := b.ResolveVerb("build", "clean")
	if err != nil {
		t.Fatalf("ResolveVerb: %v", err)
	}
	deps := n.Dependencies()
	if len(deps) != 2 {
		t.Fatalf("[clean build] deps: got %d, want 2: %v", len(deps), depTargets(deps))
	}
}

func TestMatrixBashExpansionInForExpr(t *testing.T) {
	// Bash variable in for expr is evaluated at build time.
	b := newBuild(t, `
OSES="linux macos"
build for os in [$OSES] { :; }
`)
	info := b.matrixInfo["build"]
	if info == nil {
		t.Fatal("expected matrixInfo")
	}
	if len(info.combos) != 2 {
		t.Errorf("combos from bash expansion: got %d, want 2", len(info.combos))
	}
}

func TestMatrixErrorOnPatternRule(t *testing.T) {
	_, err := NewBuild([]byte(`'(.*)' for os in [linux] { :; }`))
	if err == nil {
		t.Fatal("expected error for matrix on pattern rule")
	}
}

func TestMatrixErrorAllCombosExcluded(t *testing.T) {
	_, err := NewBuild([]byte(`build for os in [linux] exclude [os=linux] { :; }`))
	if err == nil {
		t.Fatal("expected error when all combos are excluded")
	}
}

func TestMatrixExplicitComboDepWithVarSubstitution(t *testing.T) {
	// [build @ os=$os] from test[os=linux go=1.20] substitutes $os=linux and
	// fans out over the unspecified libc dimension.
	b := newBuild(t, `
build for os in [linux macos] for libc in [musl glibc] { :; }
test for os in [linux macos] for go in [1.20 1.21] : [build @ os=$os] { :; }
`)
	testLinuxGo120 := comboTargetName("test", matrixCombo{"os": "linux", "go": "1.20"})
	n, err := b.Resolve(testLinuxGo120)
	if err != nil {
		t.Fatalf("Resolve %q: %v", testLinuxGo120, err)
	}
	deps := n.Dependencies()
	if len(deps) != 2 {
		t.Fatalf("test[os=linux go=1.20] deps: got %d, want 2: %v", len(deps), depTargets(deps))
	}
	linuxMusl := comboTargetName("build", matrixCombo{"os": "linux", "libc": "musl"})
	linuxGlibc := comboTargetName("build", matrixCombo{"os": "linux", "libc": "glibc"})
	got := depTargets(deps)
	if !contains(got, linuxMusl) {
		t.Errorf("expected dep %q in %v", linuxMusl, got)
	}
	if !contains(got, linuxGlibc) {
		t.Errorf("expected dep %q in %v", linuxGlibc, got)
	}
}

func TestMatrixExplicitComboDepPartialFanOut(t *testing.T) {
	// [build @ os=linux] (literal, no $var) fans out over the unspecified libc
	// dimension regardless of which combo the caller is in.
	b := newBuild(t, `
build for os in [linux macos] for libc in [musl glibc] { :; }
foo : [build @ os=linux] { :; }
`)
	n, err := b.Resolve("foo")
	if err != nil {
		t.Fatalf("Resolve foo: %v", err)
	}
	deps := n.Dependencies()
	if len(deps) != 2 {
		t.Fatalf("foo deps: got %d, want 2: %v", len(deps), depTargets(deps))
	}
	linuxMusl := comboTargetName("build", matrixCombo{"os": "linux", "libc": "musl"})
	linuxGlibc := comboTargetName("build", matrixCombo{"os": "linux", "libc": "glibc"})
	got := depTargets(deps)
	if !contains(got, linuxMusl) {
		t.Errorf("expected dep %q in %v", linuxMusl, got)
	}
	if !contains(got, linuxGlibc) {
		t.Errorf("expected dep %q in %v", linuxGlibc, got)
	}
}

// contains reports whether slice contains s.
func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

func TestDefBodyVerbDuplicateError(t *testing.T) {
	src := `
deftype mytype {
	echo 0
}
defbody mytype clean { true }
defbody mytype clean { false }
`
	_, err := NewBuild([]byte(src))
	if err == nil {
		t.Fatal("expected error for duplicate verb defbody")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error should mention duplicate: %v", err)
	}
}
