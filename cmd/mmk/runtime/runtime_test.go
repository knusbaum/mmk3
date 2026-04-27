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

func TestVerbInheritancePropagatesRunner(t *testing.T) {
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
	targets := depTargets(deps)
	found := false
	for _, name := range targets {
		if name == "myimage" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected [clean myimage] in deps, got %v", targets)
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
