package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/knusbaum/mmk3/cmd/mmk/parse"
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

// runForTest builds a node and all its transitive deps via post-order DFS.
// Stops on the first failure. Useful for tests that need to inspect a body's
// observable side effects.
func runForTest(n *TargetNode) error {
	visited := make(map[*TargetNode]bool)
	var visit func(*TargetNode) error
	visit = func(node *TargetNode) error {
		if visited[node] {
			return nil
		}
		visited[node] = true
		for _, dep := range node.Dependencies() {
			if err := visit(dep); err != nil {
				return err
			}
		}
		return node.Run()
	}
	return visit(n)
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
	src := fmt.Sprintf(`deftype fake-image platform= {
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
	needsRun, err := n.NeedsRun()
	if err != nil {
		t.Fatal(err)
	}
	if !needsRun {
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
	needsRun, err := n.NeedsRun()
	if err != nil {
		t.Fatal(err)
	}
	if needsRun {
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
	needsRun, err := n.NeedsRun()
	if err != nil {
		t.Fatal(err)
	}
	if !needsRun {
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
	needsRun, err := n.NeedsRun()
	if err != nil {
		t.Fatal(err)
	}
	if !needsRun {
		t.Error("expected NeedsRun() == true when target file doesn't exist")
	}
}

// TestInRunRebuildCascadesToConsumer is a regression test for a bug where a
// target's cached Date held its pre-rebuild value, so a consumer scheduled
// later in the same build saw the old mtime and skipped. With the fix, a
// successful Run invalidates the cached Date and the consumer sees the new
// mtime when its own NeedsRun fires.
func TestInRunRebuildCascadesToConsumer(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	c := filepath.Join(dir, "c")

	// Start with a < b = c (all older than now). Then touch `a` to make b
	// stale; c must cascade-rebuild.
	past := time.Now().Add(-time.Minute)
	touchAt(t, a, past)
	touchAt(t, b, past)
	touchAt(t, c, past)
	// Bump a so it's strictly newer than b — this is the only source change.
	touchAt(t, a, time.Now())

	src := fmt.Sprintf(`
file %q : %q { cp %q %q }
file %q : %q { cp %q %q }
file %q :
`, c, b, b, c, b, a, a, b, a)

	build := newBuild(t, src)
	if err := build.Execute(c, "", 1); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// c must be at least as new as b after the cascade. The bug manifested
	// as c keeping its original "past" mtime because the rebuild was skipped.
	bInfo, err := os.Stat(b)
	if err != nil {
		t.Fatalf("stat b: %v", err)
	}
	cInfo, err := os.Stat(c)
	if err != nil {
		t.Fatalf("stat c: %v", err)
	}
	if cInfo.ModTime().Before(bInfo.ModTime()) {
		t.Errorf("c (%v) is older than b (%v): cascade did not propagate", cInfo.ModTime(), bInfo.ModTime())
	}
	if !cInfo.ModTime().After(past) {
		t.Errorf("c mtime %v ≤ past %v: c was not rebuilt at all", cInfo.ModTime(), past)
	}
}

// TestUserDeftypeNanoStatNoSpuriousRebuild combines the cascade fix and the
// epoch.nanos parser: a user deftype emitting `stat -c %.Y` against a source
// dep in the same integer second must NOT trigger a spurious rebuild on the
// second mmk run. The pre-fix behavior was a precision mismatch between the
// source's Go-native ModTime (full precision) and the user deftype's bash
// `stat -c %Y` (integer seconds) — the source always looked sub-second-newer
// even when it wasn't actually written after the target.
func TestUserDeftypeNanoStatNoSpuriousRebuild(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	out := filepath.Join(dir, "out")

	// Source at t.000, output at t.500 — same integer second, output strictly
	// later in real time. Without %.Y, target.Date() returns the integer t.000
	// and source.Date() returns t.000000000 — equal, so no rebuild. With %Y
	// integer + source's Go ModTime, the comparison sees source > target.
	t0 := time.Unix(time.Now().Unix(), 0)
	touchAt(t, src, t0)
	touchAt(t, out, t0.Add(500*time.Millisecond))

	mmkSrc := fmt.Sprintf(`
deftype mypkg { stat -c %%.Y "$target" 2>/dev/null || stat -f %%m "$target" 2>/dev/null || return 1 }
mypkg %q : %q { cp %q %q }
file %q :
`, out, src, src, out, src)

	build := newBuild(t, mmkSrc)
	n, _ := build.Resolve(out)
	n.Dependencies()
	needsRun, err := n.NeedsRun()
	if err != nil {
		t.Fatal(err)
	}
	if needsRun {
		t.Errorf("expected NeedsRun() == false: source (%v) is older than target (%v) at full precision; spurious rebuild indicates precision mismatch in deftype parsing", t0, t0.Add(500*time.Millisecond))
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
	got, err := n.Date()
	if err != nil {
		t.Fatal(err)
	}
	want := time.Unix(epoch, 0)
	if !got.Equal(want) {
		t.Errorf("Date(): got %v, want %v", got, want)
	}
}

// TestDeftypeEpochNanosDate exercises the "epoch.nanos" timestamp form that
// matches GNU `stat -c %.Y` output, so deftypes can preserve sub-second
// precision without going through RFC3339.
func TestDeftypeEpochNanosDate(t *testing.T) {
	want := time.Unix(1780111599, 362179295)
	src := fmt.Sprintf(`
deftype mytype {
	echo %d.%09d
}
mytype mytarget :
`, want.Unix(), want.Nanosecond())
	b := newBuild(t, src)
	n, _ := b.Resolve("mytarget")
	n.Dependencies()
	got, err := n.Date()
	if err != nil {
		t.Fatal(err)
	}
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
	got, err := n.Date()
	if err != nil {
		t.Fatal(err)
	}
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
	needsRun, err := n.NeedsRun()
	if err != nil {
		t.Fatal(err)
	}
	if !needsRun {
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

// TestCancelledBuildNoBodyNode verifies that Run() on a no-body node returns
// ErrCancelled when the build has been cancelled. Before the fix, Run() returned
// nil immediately after executeScript() reported no body, skipping the
// IsCancelled check entirely — so cancellation silently fell through chains of
// bodyless aggregator nodes (e.g. `all : dep1 dep2`).
func TestCancelledBuildNoBodyNode(t *testing.T) {
	// `all` has no body — executeScript returns ("", false).
	b := newBuild(t, `all : dep`)
	n, err := b.Resolve("all")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	b.Cancel()
	err = n.Run()
	if err != ErrCancelled {
		t.Errorf("Run on cancelled build: got %v, want ErrCancelled", err)
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

func TestFailureSummaryRecordsOmitReplayOutputByDefault(t *testing.T) {
	fs := []FailureRecord{{Target: "bad", Output: "already printed\n"}}

	got := failureSummaryRecords(fs, false)
	if got[0].Output != "" {
		t.Fatalf("Output = %q, want empty", got[0].Output)
	}
	if fs[0].Output == "" {
		t.Fatal("failureSummaryRecords mutated input")
	}

	got = failureSummaryRecords(fs, true)
	if got[0].Output != fs[0].Output {
		t.Fatalf("Output = %q, want %q", got[0].Output, fs[0].Output)
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

// --- defrunner dep clause ---

func TestRunnerDepsDefaultAddsRunnerTarget(t *testing.T) {
	// Backwards-compat: a runner type with no dep clause causes consumers'
	// `on R` to add R as a dep — the historical behavior.
	b := newBuild(t, `
deftype custom_runner { echo 1 }
defrunner custom_runner { :; }
custom_runner myrunner :
file build/foo on myrunner : src.c { :; }
`)
	n, err := b.Resolve("build/foo")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got := depTargets(n.Dependencies())
	if !contains(got, "myrunner") {
		t.Errorf("expected 'myrunner' in deps (default runner-as-dep behavior), got %v", got)
	}
}

func TestRunnerDepsEmptyClauseElidesRunnerTarget(t *testing.T) {
	// `defrunner T : { ... }` — explicit empty clause means the runner type
	// contributes no consumer-side dep. The synthetic runner setup node still
	// fires, but the runner target itself is not in the consumer's dep list.
	b := newBuild(t, `
deftype custom_runner { echo 1 }
defrunner custom_runner : { :; }
custom_runner myrunner :
file build/foo on myrunner : src.c { :; }
`)
	n, err := b.Resolve("build/foo")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got := depTargets(n.Dependencies())
	if contains(got, "myrunner") {
		t.Errorf("expected 'myrunner' NOT in deps (empty dep clause), got %v", got)
	}
	// The synthetic setup node should still be present so the runner's setup
	// phase fires (even when it has nothing meaningful to do).
	if !containsAnyWithPrefix(got, "__runner__") {
		t.Errorf("expected a synthetic runner setup dep, got %v", got)
	}
}

func TestRunnerDepsClauseInjectsCustomDeps(t *testing.T) {
	// A defrunner dep clause can name arbitrary deps. The output is
	// word-split and appended to every `on T` consumer's dep list.
	b := newBuild(t, `
deftype custom_runner { echo 1 }
defrunner custom_runner : prereq.bin $target { :; }
custom_runner myrunner :
file build/foo on myrunner : src.c { :; }
`)
	n, err := b.Resolve("build/foo")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got := depTargets(n.Dependencies())
	if !contains(got, "prereq.bin") {
		t.Errorf("expected 'prereq.bin' in deps (named in clause), got %v", got)
	}
	if !contains(got, "myrunner") {
		t.Errorf("expected 'myrunner' in deps ($target in clause), got %v", got)
	}
}

func TestRunnerDepsClauseSeesRunnerOptions(t *testing.T) {
	// The dep clause is evaluated with the runner target's options bound as
	// bash variables. Different runner instances with the same type get
	// different deps based on their options.
	b := newBuild(t, `
deftype custom_runner flavor= { echo 1 }
defrunner custom_runner : prereq_$flavor.bin { :; }
custom_runner runner_a flavor=alpha :
custom_runner runner_b flavor=beta :
file build/foo on runner_a : src.c { :; }
file build/bar on runner_b : src.c { :; }
`)
	nFoo, err := b.Resolve("build/foo")
	if err != nil {
		t.Fatalf("Resolve foo: %v", err)
	}
	nBar, err := b.Resolve("build/bar")
	if err != nil {
		t.Fatalf("Resolve bar: %v", err)
	}
	if got := depTargets(nFoo.Dependencies()); !contains(got, "prereq_alpha.bin") {
		t.Errorf("foo: expected 'prereq_alpha.bin', got %v", got)
	}
	if got := depTargets(nBar.Dependencies()); !contains(got, "prereq_beta.bin") {
		t.Errorf("bar: expected 'prereq_beta.bin', got %v", got)
	}
}

func TestBuiltinImageRunnerDepsAddsImageByDefault(t *testing.T) {
	// With no skip_if, the built-in image runner's dep clause expands to the
	// image's own name. Consumers depend on the image target. (Tested via
	// graph inspection — no docker needed.)
	b := newBuild(t, `
image fakeimg : Dockerfile
file build/foo on fakeimg : src.c { :; }
`)
	n, err := b.Resolve("build/foo")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := depTargets(n.Dependencies()); !contains(got, "fakeimg") {
		t.Errorf("expected 'fakeimg' in deps (no skip_if), got %v", got)
	}
}

func TestBuiltinImageRunnerDepsSkipElidesImage(t *testing.T) {
	// With skip_if=true the image runner's dep clause emits no deps; the
	// consumer's edge to the image target is gone. The synthetic runner
	// setup node stays so the run phase still has its skip-sentinel state.
	b := newBuild(t, `
image fakeimg skip_if=true : Dockerfile
file build/foo on fakeimg : src.c { :; }
`)
	n, err := b.Resolve("build/foo")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got := depTargets(n.Dependencies())
	if contains(got, "fakeimg") {
		t.Errorf("expected 'fakeimg' NOT in deps (skip_if=true), got %v", got)
	}
	if !containsAnyWithPrefix(got, "__runner__") {
		t.Errorf("expected a synthetic runner setup dep, got %v", got)
	}
}

func TestRunnerDepsCachedAcrossConsumers(t *testing.T) {
	// Two consumers sharing one runner instance should share one resolved
	// dep set — the bash subprocess runs once per runner instance.
	b := newBuild(t, `
deftype custom_runner { echo 1 }
defrunner custom_runner : $target { :; }
custom_runner myrunner :
file build/foo on myrunner : a.c { :; }
file build/bar on myrunner : b.c { :; }
`)
	nFoo, err := b.Resolve("build/foo")
	if err != nil {
		t.Fatalf("Resolve foo: %v", err)
	}
	_ = nFoo.Dependencies()
	nBar, err := b.Resolve("build/bar")
	if err != nil {
		t.Fatalf("Resolve bar: %v", err)
	}
	_ = nBar.Dependencies()
	cached, ok := b.defRunnerDepsCache["myrunner"]
	if !ok {
		t.Fatal("expected myrunner to be cached after Dependencies()")
	}
	if len(cached) != 1 || cached[0] != "myrunner" {
		t.Errorf("cache: got %v, want [myrunner]", cached)
	}
}

// containsAnyWithPrefix reports whether any element of s starts with prefix.
func containsAnyWithPrefix(s []string, prefix string) bool {
	for _, x := range s {
		if strings.HasPrefix(x, prefix) {
			return true
		}
	}
	return false
}

// --- built-in directory type ---

func TestDirectoryTypeCreatesDir(t *testing.T) {
	// `directory <path>` with no body uses the built-in defbody, which runs
	// `mkdir -p`. After Run, the directory exists.
	dir := t.TempDir()
	target := filepath.Join(dir, "out", "nested")
	src := fmt.Sprintf(`directory %q :`, target)
	b := newBuild(t, src)
	n, err := b.Resolve(target)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := runForTest(n); err != nil {
		t.Fatalf("Run: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat after Run: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("expected %q to be a directory", target)
	}
}

func TestDirectoryTypeAbsentTriggersBuild(t *testing.T) {
	// When the directory doesn't exist, deftype returns nonzero ⇒ Date is
	// the zero time, NeedsRun returns true, defbody runs.
	dir := t.TempDir()
	target := filepath.Join(dir, "missing")
	src := fmt.Sprintf(`directory %q :`, target)
	b := newBuild(t, src)
	n, _ := b.Resolve(target)
	needs, err := n.NeedsRun()
	if err != nil {
		t.Fatalf("NeedsRun: %v", err)
	}
	if !needs {
		t.Errorf("NeedsRun before mkdir: want true, got false")
	}
}

func TestDirectoryTypeFixedDate(t *testing.T) {
	// Once the directory exists, deftype reports a fixed small mtime (epoch 1)
	// so consumers don't churn when files inside the dir are added/removed.
	dir := t.TempDir()
	target := filepath.Join(dir, "out")
	src := fmt.Sprintf(`directory %q :`, target)
	b := newBuild(t, src)
	n, _ := b.Resolve(target)
	if err := runForTest(n); err != nil {
		t.Fatalf("Run: %v", err)
	}
	date, err := n.Date()
	if err != nil {
		t.Fatalf("Date: %v", err)
	}
	if date.Unix() != 1 {
		t.Errorf("Date after creation: got %d, want 1 (fixed-low so consumers don't churn)", date.Unix())
	}
	// Touch a file inside the directory — its mtime shouldn't bleed through
	// to consumers as a "newer than them" signal.
	stamp := filepath.Join(target, "x")
	if err := os.WriteFile(stamp, nil, 0o644); err != nil {
		t.Fatalf("write inside dir: %v", err)
	}
	// Clear cached Date so we re-stat.
	n.invalidateDate()
	date2, err := n.Date()
	if err != nil {
		t.Fatalf("Date second call: %v", err)
	}
	if date2.Unix() != 1 {
		t.Errorf("Date after content change: got %d, want 1 (dir mtime must not propagate)", date2.Unix())
	}
}

func TestDirectoryTypeCleanRemovesTree(t *testing.T) {
	// Clean verb is rm -rf — removes the directory and any contents.
	dir := t.TempDir()
	target := filepath.Join(dir, "out")
	src := fmt.Sprintf(`directory %q :`, target)
	b := newBuild(t, src)
	n, _ := b.Resolve(target)
	if err := runForTest(n); err != nil {
		t.Fatalf("build: %v", err)
	}
	stamp := filepath.Join(target, "leftover")
	if err := os.WriteFile(stamp, nil, 0o644); err != nil {
		t.Fatalf("write inside dir: %v", err)
	}
	cleanNode, err := b.ResolveVerb(target, "clean")
	if err != nil {
		t.Fatalf("ResolveVerb clean: %v", err)
	}
	if err := runForTest(cleanNode); err != nil {
		t.Fatalf("clean Run: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("after clean: dir should be gone, stat err = %v", err)
	}
}

func TestDirectoryTypeCreatesParents(t *testing.T) {
	// `mkdir -p` semantics: declaring `directory a/b/c :` should create a,
	// a/b, AND a/b/c without the user having to declare each level.
	dir := t.TempDir()
	target := filepath.Join(dir, "a", "b", "c")
	src := fmt.Sprintf(`directory %q :`, target)
	b := newBuild(t, src)
	n, _ := b.Resolve(target)
	if err := runForTest(n); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, path := range []string{
		filepath.Join(dir, "a"),
		filepath.Join(dir, "a", "b"),
		filepath.Join(dir, "a", "b", "c"),
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("expected %q to exist: %v", path, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("expected %q to be a directory", path)
		}
	}
}

func TestDirectoryAsDependency(t *testing.T) {
	// The typical use: a build artifact lists the directory as a dep so the
	// dir exists before the artifact's body runs.
	dir := t.TempDir()
	dirTarget := filepath.Join(dir, "out", "sub")
	fileTarget := filepath.Join(dirTarget, "stamp")
	src := fmt.Sprintf(`directory %q :
file %q : %q {
    touch "$target"
}`, dirTarget, fileTarget, dirTarget)
	b := newBuild(t, src)
	n, err := b.Resolve(fileTarget)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := runForTest(n); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(fileTarget); err != nil {
		t.Errorf("file dep on dir should have created file: %v", err)
	}
}

func TestNodeForRaceVsResolveVerb(t *testing.T) {
	// Regression for: TUI View goroutine reads b.verbNodes via NodeFor while
	// the executor goroutine writes to it via ResolveVerb during dag.Build.
	// With -race, the unprotected version trips concurrent map access; with
	// nodesMu in place, no race.
	src := `
file foo : src.c { :; }
[clean foo]
`
	b := newBuild(t, src)
	const iters = 200
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < iters; i++ {
			_, _ = b.ResolveVerb("foo", "clean")
			_, _ = b.Resolve("foo")
		}
	}()
	for i := 0; i < iters; i++ {
		_ = b.NodeFor("foo", "clean")
		_ = b.NodeFor("foo", "")
	}
	<-done
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
	rnDate, err := rn.Date()
	if err != nil {
		t.Fatal(err)
	}
	if !rnDate.IsZero() {
		t.Errorf("runner node Date() should be zero, got %v", rnDate)
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
	needsRun, err := n.NeedsRun()
	if err != nil {
		t.Fatal(err)
	}
	if !needsRun {
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

func TestExecuteRejectsVerbWithNoTargetsInGraph(t *testing.T) {
	// `foop` is defined for `weird`, but not for anything in `all`'s graph.
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
	if err == nil || !strings.Contains(err.Error(), "no targets with bodies") {
		t.Fatalf("expected 'no targets with bodies' error, got %v", err)
	}
}

func TestGraphPrunesEmptyVerbSubgraphs(t *testing.T) {
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

// A verb rule whose explicit deps are plain (non-verb) targets with real
// bodies is fully defined: the verb's "work" is the deps. checkVerbHasTargets
// should not reject it.
func TestVerbWithExplicitNonVerbDepsExecutes(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	mustWrite(t, dir, "mmkfile", `
leaf {
    touch leaf.stamp
}
[test all] : leaf
`)
	b := newFileBuild(t, dir)
	if err := b.Execute("all", "test", 0); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "leaf.stamp")); err != nil {
		t.Errorf("expected leaf body to run; %v", err)
	}
}

// `[test all] : a b c` with several non-verb deps, each with a body, runs
// all three (and would parallelize under -j > 1).
func TestVerbWithMultipleNonVerbDepsRunsAll(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	mustWrite(t, dir, "mmkfile", `
a { touch a.stamp }
b { touch b.stamp }
c { touch c.stamp }
[test all] : a b c
`)
	b := newFileBuild(t, dir)
	if err := b.Execute("all", "test", 0); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, name := range []string{"a.stamp", "b.stamp", "c.stamp"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("expected %s; %v", name, err)
		}
	}
}

// `[verb all] :+ leaf` mixes an explicit non-verb dep with the inherited
// verb-applied deps. Both halves should run.
func TestVerbAugmentMixingNonVerbAndInheritedExecutes(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	mustWrite(t, dir, "mmkfile", `
all : inherited
inherited :
[test inherited] { touch inherited.stamp }
leaf { touch leaf.stamp }
[test all] :+ leaf
`)
	b := newFileBuild(t, dir)
	if err := b.Execute("all", "test", 0); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, name := range []string{"inherited.stamp", "leaf.stamp"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("expected %s; %v", name, err)
		}
	}
}

// When an explicit non-verb dep references an undeclared target, mmk infers
// a source rule for it. The static checkVerbHasTargets accepts this (the
// source body counts as work), and the runtime then surfaces the missing
// source via the source-rule body (which prints to stderr) — better than
// the previous misleading "no applicable rule" message.
func TestVerbWithUndeclaredNonVerbDepFailsAtRuntime(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	mustWrite(t, dir, "mmkfile", `[test all] : nosuchfile.c
`)
	b := newFileBuild(t, dir)
	err := b.Execute("all", "test", 0)
	if err == nil {
		t.Fatal("expected error from missing source dep")
	}
	// The check passed (no "no targets with bodies"), so the failure is the
	// runtime source-existence check, not the static check.
	if strings.Contains(err.Error(), "no targets with bodies") {
		t.Errorf("expected runtime failure, not static check failure: %v", err)
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

// --- defbody dep clause ---

func TestDefBodyDepClauseAddsDepsToTarget(t *testing.T) {
	src := `
deftype c_library { stat -f %m "$target" 2>/dev/null }
defbody c_library : $(echo foo.o bar.o) {
    ar -rcs "$target" "${dep[@]}"
}
file '(.*)\.o' : $1.c { cc -c $1.c -o $target }
c_library mylib.a :
`
	b := newBuild(t, src)
	n, err := b.Resolve("mylib.a")
	if err != nil {
		t.Fatalf("Resolve mylib.a: %v", err)
	}
	deps := n.Dependencies()
	got := depTargets(deps)
	want := []string{"foo.o", "bar.o"}
	if len(got) != len(want) {
		t.Fatalf("deps: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("dep[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestDefBodyDepClauseUsesOptions(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.c"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.c"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	src := fmt.Sprintf(`
deftype c_library { stat -f %%m "$target" 2>/dev/null }
defbody c_library source= : $(find "$source" -name '*.c' | sed 's/\.c$/.o/') {
    ar -rcs "$target" "${dep[@]}"
}
file '(.*)\.o' : $1.c { cc -c $1.c -o $target }
c_library mylib.a source=%q :
`, dir)
	b := newBuild(t, src)
	n, _ := b.Resolve("mylib.a")
	deps := n.Dependencies()
	got := depTargets(deps)
	if len(got) != 2 {
		t.Fatalf("expected 2 deps from $source scan, got %v", got)
	}
	// Order from find is filesystem-dependent; check set membership.
	wantSet := map[string]bool{
		filepath.Join(dir, "a.o"): true,
		filepath.Join(dir, "b.o"): true,
	}
	for _, g := range got {
		if !wantSet[g] {
			t.Errorf("unexpected dep %q (want one of a.o/b.o under %s)", g, dir)
		}
	}
}

func TestDefBodyDepClauseAugmentsExplicitDeps(t *testing.T) {
	src := `
deftype c_library { stat -f %m "$target" 2>/dev/null }
defbody c_library : $(echo computed.o) {
    ar -rcs "$target" "${dep[@]}"
}
file '(.*)\.o' : $1.c { cc -c $1.c -o $target }
c_library mylib.a : explicit.o
`
	b := newBuild(t, src)
	n, _ := b.Resolve("mylib.a")
	got := depTargets(n.Dependencies())
	want := []string{"explicit.o", "computed.o"}
	if len(got) != len(want) {
		t.Fatalf("deps: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("dep[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestDefBodyDepClauseFiresWithCustomBody(t *testing.T) {
	// Computed deps must still be added to the DAG when the rule has its own body.
	src := `
deftype c_library { stat -f %m "$target" 2>/dev/null }
defbody c_library : $(echo a.o b.o) {
    ar -rcs "$target" "${dep[@]}"
}
file '(.*)\.o' : $1.c { cc -c $1.c -o $target }
c_library mylib.a : { custom_link_command "${dep[@]}"; }
`
	b := newBuild(t, src)
	n, _ := b.Resolve("mylib.a")
	got := depTargets(n.Dependencies())
	if len(got) != 2 || got[0] != "a.o" || got[1] != "b.o" {
		t.Errorf("computed deps should still fire with custom body; got %v", got)
	}
}

func TestDefBodyDepClauseVerbInheritance(t *testing.T) {
	// `mmk clean mylib.a` should recurse through defbody-computed deps.
	src := `
deftype c_library { stat -f %m "$target" 2>/dev/null }
defbody c_library : $(echo foo.o) {
    ar -rcs "$target" "${dep[@]}"
}
defbody c_library clean {
    rm -f "$target" "${dep[@]}"
}
file '(.*)\.o' : $1.c { cc -c $1.c -o $target }
c_library mylib.a :
`
	b := newBuild(t, src)
	cleanNode, err := b.ResolveVerb("mylib.a", "clean")
	if err != nil {
		t.Fatalf("ResolveVerb mylib.a clean: %v", err)
	}
	deps := cleanNode.Dependencies()
	// We expect [clean foo.o] as a dep of [clean mylib.a] via verb inheritance
	// over the type-computed dep.
	found := false
	for _, d := range deps {
		if d.target == "foo.o" && d.verb == "clean" {
			found = true
			break
		}
	}
	if !found {
		var got []string
		for _, d := range deps {
			got = append(got, fmt.Sprintf("[%s %s]", d.verb, d.target))
		}
		t.Errorf("expected [clean foo.o] in deps via verb inheritance, got %v", got)
	}
}

func TestDefBodyDepClauseBindsTargetVar(t *testing.T) {
	// $target should be bound during dep clause evaluation.
	src := `
deftype c_library { stat -f %m "$target" 2>/dev/null }
defbody c_library : $(echo "$target.dep") {
    :
}
mylib.a.dep :
c_library mylib.a :
`
	b := newBuild(t, src)
	n, _ := b.Resolve("mylib.a")
	got := depTargets(n.Dependencies())
	if len(got) != 1 || got[0] != "mylib.a.dep" {
		t.Errorf("$target should expand to mylib.a; got deps %v", got)
	}
}

func TestDefBodyDepClauseBindsExplicitDeps(t *testing.T) {
	// ${dep[@]} should be bound to the rule's explicit deps during dep
	// clause evaluation, so a dep expression can reference them.
	src := `
deftype c_library { stat -f %m "$target" 2>/dev/null }
defbody c_library : $(echo "${dep[0]}.computed") {
    :
}
foo.o :
foo.o.computed :
c_library mylib.a : foo.o
`
	b := newBuild(t, src)
	n, _ := b.Resolve("mylib.a")
	got := depTargets(n.Dependencies())
	want := []string{"foo.o", "foo.o.computed"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("dep clause should see ${dep[@]} from explicit deps; got %v, want %v", got, want)
	}
}

func TestDefBodyDepClauseSeesPassthroughVars(t *testing.T) {
	// Passthrough variables should be visible in dep clause evaluation.
	src := `
EXTRA="x.o y.o"
deftype c_library { stat -f %m "$target" 2>/dev/null }
defbody c_library : $(echo $EXTRA) {
    :
}
x.o :
y.o :
c_library mylib.a :
`
	b := newBuild(t, src)
	n, _ := b.Resolve("mylib.a")
	got := depTargets(n.Dependencies())
	want := []string{"x.o", "y.o"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("dep clause should see passthrough vars; got %v, want %v", got, want)
	}
}

func TestDefBodyDepClauseMultipleTokens(t *testing.T) {
	// A dep clause can have multiple tokens; each is evaluated and word-split.
	src := `
deftype c_library { stat -f %m "$target" 2>/dev/null }
defbody c_library : $(echo a.o b.o) extra.o $(echo c.o) {
    :
}
a.o :
b.o :
c.o :
extra.o :
c_library mylib.a :
`
	b := newBuild(t, src)
	n, _ := b.Resolve("mylib.a")
	got := depTargets(n.Dependencies())
	want := []string{"a.o", "b.o", "extra.o", "c.o"}
	if len(got) != len(want) {
		t.Fatalf("got %d deps, want %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("dep[%d]: got %q, want %q", i, got[i], w)
		}
	}
}

func TestDefBodyDepClauseBodySeesCombinedDeps(t *testing.T) {
	// The body must see explicit deps + computed deps combined in ${dep[@]}.
	// Run the body (no custom override) and verify $deps content.
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("explicit.o", nil, 0644); err != nil {
		t.Fatal(err)
	}
	src := `
deftype c_library { stat -f %m "$target" 2>/dev/null }
defbody c_library : $(echo computed1.o computed2.o) {
    printf '%s\n' "${dep[@]}" > deps.txt
    touch "$target"
}
file '(.*)\.o' : { touch "$target"; }
c_library mylib.a : explicit.o
`
	b := newBuild(t, src)
	n, err := b.Resolve("mylib.a")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := runForTest(n); err != nil {
		t.Fatalf("Run: %v", err)
	}
	data, err := os.ReadFile("deps.txt")
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	gotLines := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{"explicit.o", "computed1.o", "computed2.o"}
	if len(gotLines) != len(want) {
		t.Fatalf("body's ${dep[@]}: got %v, want %v", gotLines, want)
	}
	for i, w := range want {
		if gotLines[i] != w {
			t.Errorf("body's dep[%d]: got %q, want %q", i, gotLines[i], w)
		}
	}
}

func TestDefBodyDepClauseCustomBodyOverrides(t *testing.T) {
	// When the rule has its own body, the custom body runs (not the defbody),
	// while computed deps still apply.
	dir := t.TempDir()
	t.Chdir(dir)
	src := `
deftype c_library { stat -f %m "$target" 2>/dev/null }
defbody c_library : $(echo a.o b.o) {
    echo "DEFBODY ran" > out.txt
    touch "$target"
}
file '(.*)\.o' : { touch "$target"; }
c_library mylib.a : {
    echo "CUSTOM ran with deps=${dep[*]}" > out.txt
    touch "$target"
}
`
	b := newBuild(t, src)
	n, _ := b.Resolve("mylib.a")
	if err := runForTest(n); err != nil {
		t.Fatalf("Run: %v", err)
	}
	data, _ := os.ReadFile("out.txt")
	got := strings.TrimSpace(string(data))
	if !strings.HasPrefix(got, "CUSTOM ran") {
		t.Fatalf("expected custom body to run, got %q", got)
	}
	if !strings.Contains(got, "a.o") || !strings.Contains(got, "b.o") {
		t.Errorf("custom body should see computed deps in ${dep[@]}; got %q", got)
	}
}

// --- verb body sees default rule's options ---

// --- ruleOptionKeys ---

func TestRuleOptionKeys_EmptyRule(t *testing.T) {
	if got := ruleOptionKeys(nil); got != "" {
		t.Errorf("nil rule: got %q, want empty", got)
	}
	if got := ruleOptionKeys(&parse.TargetRule{}); got != "" {
		t.Errorf("rule with no options: got %q, want empty", got)
	}
}

func TestRuleOptionKeys_PreservesOrder(t *testing.T) {
	r := &parse.TargetRule{Options: []parse.Option{
		{Key: "alpha", Value: "1"},
		{Key: "beta", Value: "2"},
		{Key: "gamma", Value: "3"},
	}}
	got := ruleOptionKeys(r)
	want := "alpha beta gamma"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMergedRuleOptionKeys(t *testing.T) {
	def := &parse.TargetRule{Options: []parse.Option{
		{Key: "build_dir", Value: "build"},
		{Key: "prefix", Value: "dist"},
	}}
	overlay := &parse.TargetRule{Options: []parse.Option{
		{Key: "prefix", Value: "override"},
		{Key: "extra", Value: "x"},
	}}
	got := mergedRuleOptionKeys(def, overlay)
	want := "build_dir prefix extra"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// Nil cases should not panic.
	if got := mergedRuleOptionKeys(nil, nil); got != "" {
		t.Errorf("nil/nil: got %q, want empty", got)
	}
	if got := mergedRuleOptionKeys(def, nil); got != "build_dir prefix" {
		t.Errorf("def/nil: got %q, want %q", got, "build_dir prefix")
	}
	if got := mergedRuleOptionKeys(nil, overlay); got != "prefix extra" {
		t.Errorf("nil/overlay: got %q, want %q", got, "prefix extra")
	}
}

// --- option-value $VAR expansion ---

func TestOptionValueExpandsVar(t *testing.T) {
	// `source=./dir/$SUFFIX` should resolve $SUFFIX from passthrough state at
	// build registration time, not be bound to bash as the literal string.
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	src := fmt.Sprintf(`SUFFIX=foo
mytype mytarget source=./dir/$SUFFIX {
    printf '%%s' "$source" > %q
}
deftype mytype source= { return 1; }
defbody mytype {
    :
}
`, out)
	b := newBuild(t, src)
	n, _ := b.Resolve("mytarget")
	if err := runForTest(n); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, _ := os.ReadFile(out)
	if string(got) != "./dir/foo" {
		t.Errorf("$SUFFIX should expand in option value; got %q want %q", got, "./dir/foo")
	}
}

func TestOptionValueLiteralPassesThroughUnchanged(t *testing.T) {
	// Values with no $ skip the expansion path and survive unchanged
	// (e.g. they don't get accidentally word-split or trimmed).
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	src := fmt.Sprintf(`mytype mytarget flags="--whole-archive  -lfoo" {
    printf '%%s' "$flags" > %q
}
deftype mytype flags= { return 1; }
defbody mytype { :; }
`, out)
	b := newBuild(t, src)
	n, _ := b.Resolve("mytarget")
	if err := runForTest(n); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, _ := os.ReadFile(out)
	if string(got) != "--whole-archive  -lfoo" {
		t.Errorf("literal option value should pass through unchanged; got %q", got)
	}
}

func TestVerbBodyInheritsDefaultRuleOptions_InheritedVerb(t *testing.T) {
	// A verb body that inherits from a defbody (no explicit verb rule)
	// should see the target rule's options.
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	src := fmt.Sprintf(`
deftype mytype { return 1; }
defbody mytype { :; }
defbody mytype clean myopt= {
    printf '%%s' "$myopt" > %q
}

mytype mytarget myopt=hello :
`, out)
	b := newBuild(t, src)
	n, err := b.ResolveVerb("mytarget", "clean")
	if err != nil {
		t.Fatalf("ResolveVerb: %v", err)
	}
	if err := runForTest(n); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, _ := os.ReadFile(out)
	if string(got) != "hello" {
		t.Errorf("verb body should see myopt=hello; got %q", got)
	}
}

func TestVerbBodyInheritsDefaultRuleOptions_ExplicitVerbRule(t *testing.T) {
	// A verb body with an explicit `[verb target]` rule should also see the
	// target rule's options (not just the verb rule's own).
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	src := fmt.Sprintf(`
deftype mytype myopt= { return 1; }
defbody mytype { :; }

mytype mytarget myopt=fromtarget :

[clean mytarget] {
    printf '%%s' "$myopt" > %q
}
`, out)
	b := newBuild(t, src)
	n, _ := b.ResolveVerb("mytarget", "clean")
	if err := runForTest(n); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, _ := os.ReadFile(out)
	if string(got) != "fromtarget" {
		t.Errorf("explicit verb body should see target's myopt; got %q", got)
	}
}

func TestVerbBodyInheritsDefaultRuleOptions_VerbRuleOverrides(t *testing.T) {
	// When both the target rule and the verb rule define the same key, the
	// verb rule wins (more specific).
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	src := fmt.Sprintf(`
deftype mytype myopt= { return 1; }
defbody mytype { :; }

mytype mytarget myopt=fromtarget :

[clean mytarget] myopt=fromverb {
    printf '%%s' "$myopt" > %q
}
`, out)
	b := newBuild(t, src)
	n, _ := b.ResolveVerb("mytarget", "clean")
	if err := runForTest(n); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, _ := os.ReadFile(out)
	if string(got) != "fromverb" {
		t.Errorf("verb-rule option should win; got %q", got)
	}
}

func TestDefBodyDepClauseRejectsVerbForm(t *testing.T) {
	// Verb-specific defbody dep clauses are parsed but not yet honored at
	// runtime. Reject up front so users don't write something silently broken.
	src := `
deftype c_library { stat -f %m "$target" 2>/dev/null }
defbody c_library clean : $(echo extra.o) {
    rm -f "${dep[@]}"
}
`
	_, err := NewBuild([]byte(src))
	if err == nil {
		t.Fatal("expected error for verb-specific defbody dep clause")
	}
	if !strings.Contains(err.Error(), "verb") {
		t.Errorf("error should mention verb: %v", err)
	}
}

func TestUnknownOptionOnTypeWithDeclaredOptionsIsRejected(t *testing.T) {
	// mytype declares only `myopt=`; setting an undeclared key on a target
	// of that type must be a hard error.
	src := `
deftype mytype myopt= { return 1; }
defbody mytype { :; }

mytype mytarget bogus=1 :
`
	_, err := NewBuild([]byte(src))
	if err == nil {
		t.Fatal("expected error for unknown option key")
	}
	if !strings.Contains(err.Error(), "bogus") || !strings.Contains(err.Error(), "myopt") {
		t.Errorf("error should name the unknown key and list known keys: %v", err)
	}
}

func TestAnyOptionOnTypeWithNoDeclaredOptionsIsRejected(t *testing.T) {
	// mytype declares no options at all; setting any option key on a
	// target of that type must be a hard error, not silently accepted.
	src := `
deftype mytype { return 1; }
defbody mytype { :; }

mytype mytarget bogus=1 :
`
	_, err := NewBuild([]byte(src))
	if err == nil {
		t.Fatal("expected error for option on a type with no declared options")
	}
	if !strings.Contains(err.Error(), "bogus") || !strings.Contains(err.Error(), "no options") {
		t.Errorf("error should name the key and say the type declares no options: %v", err)
	}
}

func TestOrderAndTtyOptionsAreExemptFromTypeVocabulary(t *testing.T) {
	// `order=` and `tty=` are engine-level, cross-cutting options — they
	// must be accepted on any rule regardless of the type's declared
	// option vocabulary, even a type that declares nothing.
	src := `
deftype mytype { return 1; }
defbody mytype { :; }

mytype mytarget tty=true :
`
	b := newBuild(t, src)
	n, err := b.Resolve("mytarget")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := runForTest(n); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestUnknownOptionOnVerbRuleResolvesBaseRuleType(t *testing.T) {
	// A `[verb target]` rule has no Type of its own; validation must
	// resolve the base concrete rule's type to find its known options.
	src := `
deftype mytype { return 1; }
defbody mytype { :; }

mytype mytarget :

[clean mytarget] bogus=1 {
    :
}
`
	_, err := NewBuild([]byte(src))
	if err == nil {
		t.Fatal("expected error for unknown option on verb rule")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error should name the unknown key: %v", err)
	}
}

// --- group features ---

func TestGroupFlatDep_NonMatrixMembers(t *testing.T) {
	// A group with non-matrix members can be depended on as a flat dep.
	src := `
group mytests

a into mytests :
b into mytests :
all : mytests
`
	b := newBuild(t, src)
	// Group aggregator "mytests" should be registered.
	if _, ok := b.concretes["mytests"]; !ok {
		t.Fatalf("expected group aggregator 'mytests' in concretes; got %v", b.Targets())
	}
	n, err := b.Resolve("all")
	if err != nil {
		t.Fatalf("Resolve all: %v", err)
	}
	deps := n.Dependencies()
	if len(deps) != 1 || deps[0].target != "mytests" {
		t.Errorf("all deps: got %v, want [mytests]", depTargets(deps))
	}
	aggDeps := deps[0].Dependencies()
	got := depTargets(aggDeps)
	if !contains(got, "a") || !contains(got, "b") {
		t.Errorf("mytests deps: got %v, want [a b]", got)
	}
}

func TestGroupMatrixMembersRegistered(t *testing.T) {
	// A matrix rule with `into` registers all its combos into the group.
	src := `
group all_builds

build for os in [linux macos] into all_builds { :; }
`
	b := newBuild(t, src)
	linuxName := comboTargetName("build", matrixCombo{"os": "linux"})
	macosName := comboTargetName("build", matrixCombo{"os": "macos"})

	gd := b.groups["all_builds"]
	if gd == nil {
		t.Fatal("expected group 'all_builds'")
	}
	memberNames := make([]string, len(gd.members))
	for i, m := range gd.members {
		memberNames[i] = m.internalName
	}
	if !contains(memberNames, linuxName) {
		t.Errorf("expected linux combo %q in group members; got %v", linuxName, memberNames)
	}
	if !contains(memberNames, macosName) {
		t.Errorf("expected macos combo %q in group members; got %v", macosName, memberNames)
	}
}

func TestGroupConsumerNoForClauses(t *testing.T) {
	// A consumer with only a group dep and no for clauses: one combo per group dim-tuple.
	src := `
group all_builds

build for os in [linux macos] into all_builds { :; }
test : [all_builds @ os] { :; }
`
	b := newBuild(t, src)
	// "test" should have been expanded into test[os=linux] and test[os=macos].
	linuxName := comboTargetName("test", matrixCombo{"os": "linux"})
	macosName := comboTargetName("test", matrixCombo{"os": "macos"})
	if _, ok := b.concretes[linuxName]; !ok {
		t.Errorf("expected combo %q in concretes; got %v", linuxName, b.Targets())
	}
	if _, ok := b.concretes[macosName]; !ok {
		t.Errorf("expected combo %q in concretes; got %v", macosName, b.Targets())
	}
	// Each consumer combo should depend on the build combo for the same os.
	linuxBuild := comboTargetName("build", matrixCombo{"os": "linux"})
	macosBuild := comboTargetName("build", matrixCombo{"os": "macos"})

	testLinux, err := b.Resolve(linuxName)
	if err != nil {
		t.Fatalf("Resolve %q: %v", linuxName, err)
	}
	depsLinux := depTargets(testLinux.Dependencies())
	if !contains(depsLinux, linuxBuild) {
		t.Errorf("test[os=linux] should dep on build[os=linux]; got %v", depsLinux)
	}

	testMacos, err := b.Resolve(macosName)
	if err != nil {
		t.Fatalf("Resolve %q: %v", macosName, err)
	}
	depsMacos := depTargets(testMacos.Dependencies())
	if !contains(depsMacos, macosBuild) {
		t.Errorf("test[os=macos] should dep on build[os=macos]; got %v", depsMacos)
	}
}

func TestGroupConsumerWithExplicitForAndGroupDims(t *testing.T) {
	// Consumer has both for clauses and a group projection dep.
	// Result should be cross-product of explicit combos × group dim-tuples.
	src := `
group all_builds

build for os in [linux macos] into all_builds { :; }
test for go in [1.20 1.21] : [all_builds @ os] { :; }
`
	b := newBuild(t, src)
	// Expect 4 combos: (go=1.20,os=linux), (go=1.20,os=macos), (go=1.21,os=linux), (go=1.21,os=macos).
	want := []matrixCombo{
		{"go": "1.20", "os": "linux"},
		{"go": "1.20", "os": "macos"},
		{"go": "1.21", "os": "linux"},
		{"go": "1.21", "os": "macos"},
	}
	for _, combo := range want {
		name := comboTargetName("test", combo)
		if _, ok := b.concretes[name]; !ok {
			t.Errorf("expected combo %q in concretes; got %v", name, b.Targets())
		}
	}
}

func TestGroupProjectionOnOneDim_FanInForUnselectedDims(t *testing.T) {
	// Group members have (os, libc) dims. Consumer projects on `os` only.
	// Each test[os=X] should fan-in on ALL build members with os=X (both libc values).
	src := `
group all_builds

build for os in [linux macos] for libc in [musl glibc] into all_builds { :; }
test : [all_builds @ os] { :; }
`
	b := newBuild(t, src)

	linuxTestName := comboTargetName("test", matrixCombo{"os": "linux"})
	n, err := b.Resolve(linuxTestName)
	if err != nil {
		t.Fatalf("Resolve %q: %v", linuxTestName, err)
	}
	deps := depTargets(n.Dependencies())

	linuxMusl := comboTargetName("build", matrixCombo{"os": "linux", "libc": "musl"})
	linuxGlibc := comboTargetName("build", matrixCombo{"os": "linux", "libc": "glibc"})

	if !contains(deps, linuxMusl) {
		t.Errorf("test[os=linux] should dep on build[os=linux libc=musl]; got %v", deps)
	}
	if !contains(deps, linuxGlibc) {
		t.Errorf("test[os=linux] should dep on build[os=linux libc=glibc]; got %v", deps)
	}
	// Should NOT contain macos combos.
	macosMusl := comboTargetName("build", matrixCombo{"os": "macos", "libc": "musl"})
	if contains(deps, macosMusl) {
		t.Errorf("test[os=linux] should not dep on macos build; got %v", deps)
	}
}

func TestGroupProjectionOnMultipleDims(t *testing.T) {
	// Consumer projects on both os and libc: each consumer combo maps to exactly one member.
	src := `
group all_builds

build for os in [linux macos] for libc in [musl glibc] into all_builds { :; }
test : [all_builds @ os libc] { :; }
`
	b := newBuild(t, src)
	// Expect 4 consumer combos, each depending on exactly one build combo.
	want := []matrixCombo{
		{"os": "linux", "libc": "musl"},
		{"os": "linux", "libc": "glibc"},
		{"os": "macos", "libc": "musl"},
		{"os": "macos", "libc": "glibc"},
	}
	for _, combo := range want {
		name := comboTargetName("test", combo)
		n, err := b.Resolve(name)
		if err != nil {
			t.Fatalf("Resolve %q: %v", name, err)
		}
		deps := depTargets(n.Dependencies())
		buildName := comboTargetName("build", combo)
		if !contains(deps, buildName) {
			t.Errorf("test%v should dep on build%v; got %v", combo, combo, deps)
		}
		if len(deps) != 1 {
			t.Errorf("test%v should have exactly 1 dep; got %v", combo, deps)
		}
	}
}

func TestGroupUndeclaredGroupError(t *testing.T) {
	src := `
build into nosuchgroup :
`
	_, err := NewBuild([]byte(src))
	if err == nil {
		t.Fatal("expected error for into undeclared group")
	}
	if !strings.Contains(err.Error(), "nosuchgroup") {
		t.Errorf("error should mention group name: %v", err)
	}
}

func TestGroupProjectionUndeclaredGroupError(t *testing.T) {
	src := `
test : [nosuchgroup @ os]
`
	_, err := NewBuild([]byte(src))
	if err == nil {
		t.Fatal("expected error for projection dep on undeclared group")
	}
	if !strings.Contains(err.Error(), "nosuchgroup") {
		t.Errorf("error should mention group name: %v", err)
	}
}

func TestGroupAggregatorFlatDep(t *testing.T) {
	// A plain dep on the group name resolves to the group aggregator naturally.
	src := `
group g

a into g :
b into g :
consumer : g
`
	b := newBuild(t, src)
	n, err := b.Resolve("consumer")
	if err != nil {
		t.Fatalf("Resolve consumer: %v", err)
	}
	deps := n.Dependencies()
	if len(deps) != 1 || deps[0].target != "g" {
		t.Errorf("consumer deps: got %v, want [g]", depTargets(deps))
	}
	// The group aggregator itself depends on both members.
	aggDeps := depTargets(deps[0].Dependencies())
	if !contains(aggDeps, "a") || !contains(aggDeps, "b") {
		t.Errorf("group aggregator deps: got %v, want [a b]", aggDeps)
	}
}

func TestGroupEmptyProjectionError(t *testing.T) {
	src := `
group empty_group
test : [empty_group @ os] { :; }
`
	_, err := NewBuild([]byte(src))
	if err == nil {
		t.Fatal("expected error for group projection with empty group")
	}
	if !strings.Contains(err.Error(), "no members") {
		t.Errorf("error should say 'no members': %v", err)
	}
}

func TestGroupDisjointDimsError(t *testing.T) {
	// Members contribute x and y separately; [g @ x y] requires both on one member.
	// Error should explain the situation and suggest projecting each dim separately.
	src := `
group g
a for x in [1 2] into g { :; }
b for y in [p q] into g { :; }
consumer : [g @ x y] { :; }
`
	_, err := NewBuild([]byte(src))
	if err == nil {
		t.Fatal("expected error when no member has all projected dims")
	}
	if !strings.Contains(err.Error(), "[g @ x] [g @ y]") {
		t.Errorf("error should suggest separate projections: %v", err)
	}
}

func TestGroupMembersWithoutRequestedDimExcluded(t *testing.T) {
	// Members that don't have the projected dimension are silently excluded
	// from projection consumers but still appear in the flat aggregator.
	src := `
group g

build for os in [linux macos] into g { :; }
plain_task into g :

consumer : [g @ os] { :; }
flat : g
`
	b := newBuild(t, src)

	// Projection creates one instance per distinct os value.
	linuxName := comboTargetName("consumer", matrixCombo{"os": "linux"})
	macosName := comboTargetName("consumer", matrixCombo{"os": "macos"})
	if _, ok := b.concretes[linuxName]; !ok {
		t.Errorf("expected %q in concretes", linuxName)
	}
	if _, ok := b.concretes[macosName]; !ok {
		t.Errorf("expected %q in concretes", macosName)
	}
	// plain_task (no `os` dim) must not have added a spurious empty-dim combo to consumer.
	info := b.matrixInfo["consumer"]
	if info == nil {
		t.Fatal("expected matrixInfo for consumer aggregator")
	}
	if len(info.combos) != 2 {
		t.Errorf("consumer should have exactly 2 combos (linux, macos), got %d: %v", len(info.combos), info.combos)
	}

	// Flat dep includes plain_task via the aggregator.
	flatNode, err := b.Resolve("flat")
	if err != nil {
		t.Fatalf("Resolve flat: %v", err)
	}
	aggDeps := depTargets(flatNode.Dependencies()[0].Dependencies())
	if !contains(aggDeps, "plain_task") {
		t.Errorf("flat group aggregator should include plain_task; got %v", aggDeps)
	}
}

func TestGroupMultipleGroupProjectionDeps(t *testing.T) {
	// Consumer depends on two different groups projected on different dims.
	// Result is the cross-product of the two groups' dim values.
	src := `
group inputs
group variants

task_a for input in [in1 in2] into inputs { :; }
task_b for variant in [v1 v2] into variants { :; }

consumer for x in [x1] : [inputs @ input] [variants @ variant] { :; }
`
	b := newBuild(t, src)

	// Should have 1 (x) × 2 (input) × 2 (variant) = 4 combos.
	want := []matrixCombo{
		{"x": "x1", "input": "in1", "variant": "v1"},
		{"x": "x1", "input": "in1", "variant": "v2"},
		{"x": "x1", "input": "in2", "variant": "v1"},
		{"x": "x1", "input": "in2", "variant": "v2"},
	}
	for _, combo := range want {
		name := comboTargetName("consumer", combo)
		n, err := b.Resolve(name)
		if err != nil {
			t.Fatalf("Resolve %q: %v", name, err)
		}
		deps := depTargets(n.Dependencies())
		inputName := comboTargetName("task_a", matrixCombo{"input": combo["input"]})
		variantName := comboTargetName("task_b", matrixCombo{"variant": combo["variant"]})
		if !contains(deps, inputName) {
			t.Errorf("%v should dep on %q; got %v", combo, inputName, deps)
		}
		if !contains(deps, variantName) {
			t.Errorf("%v should dep on %q; got %v", combo, variantName, deps)
		}
	}
}

func TestGroupCascading(t *testing.T) {
	// stage1 members → group stage1
	// stage2 consumer of stage1 → also member of group stage2
	// stage3 consumer of stage2
	src := `
group stage1
group stage2

task for x in [a b] into stage1 { :; }
agg into stage2 : [stage1 @ x] { :; }
final : [stage2 @ x] { :; }
`
	b := newBuild(t, src)

	// agg should be expanded: agg[x=a] and agg[x=b].
	aggA := comboTargetName("agg", matrixCombo{"x": "a"})
	aggB := comboTargetName("agg", matrixCombo{"x": "b"})
	if _, ok := b.concretes[aggA]; !ok {
		t.Errorf("expected %q in concretes", aggA)
	}
	if _, ok := b.concretes[aggB]; !ok {
		t.Errorf("expected %q in concretes", aggB)
	}

	// final should also be expanded: final[x=a] and final[x=b].
	finalA := comboTargetName("final", matrixCombo{"x": "a"})
	finalB := comboTargetName("final", matrixCombo{"x": "b"})
	if _, ok := b.concretes[finalA]; !ok {
		t.Errorf("expected %q in concretes", finalA)
	}
	if _, ok := b.concretes[finalB]; !ok {
		t.Errorf("expected %q in concretes", finalB)
	}

	// final[x=a] should dep on agg[x=a].
	finalANode, err := b.Resolve(finalA)
	if err != nil {
		t.Fatalf("Resolve %q: %v", finalA, err)
	}
	deps := depTargets(finalANode.Dependencies())
	if !contains(deps, aggA) {
		t.Errorf("final[x=a] should dep on %q; got %v", aggA, deps)
	}
}

func TestGroupBracketFlatDepResolvesToAggregator(t *testing.T) {
	// consumer : [g] (bracket form, no @) is a plain dep on the group aggregator.
	src := `
group g

a into g :
b into g :
consumer : [g]
`
	b := newBuild(t, src)
	n, err := b.Resolve("consumer")
	if err != nil {
		t.Fatalf("Resolve consumer: %v", err)
	}
	deps := n.Dependencies()
	if len(deps) != 1 || deps[0].target != "g" {
		t.Errorf("consumer deps: got %v, want [g]", depTargets(deps))
	}
	aggDeps := depTargets(deps[0].Dependencies())
	if !contains(aggDeps, "a") || !contains(aggDeps, "b") {
		t.Errorf("group aggregator deps: got %v, want [a b]", aggDeps)
	}
}

func TestGroupVerbAppliedToAggregator(t *testing.T) {
	// [clean g] propagates to all group members via the aggregator's inherited verb deps.
	src := `
group g

a into g :
b into g :
`
	b := newBuild(t, src)
	n, err := b.ResolveVerb("g", "clean")
	if err != nil {
		t.Fatalf("ResolveVerb g clean: %v", err)
	}
	deps := depTargets(n.Dependencies())
	if !contains(deps, "a") || !contains(deps, "b") {
		t.Errorf("[clean g] should propagate to members a and b; got %v", deps)
	}
}

// --- include ---

func TestNewBuildFromFile_IncludesArrive(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lib.mmk"), []byte(`
file from_lib : { :; }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "mmkfile")
	if err := os.WriteFile(root, []byte(`
include lib.mmk

all : from_lib
`), 0o644); err != nil {
		t.Fatal(err)
	}

	b, err := NewBuildFromFile(root)
	if err != nil {
		t.Fatalf("NewBuildFromFile: %v", err)
	}
	t.Cleanup(b.Close)

	if !b.HasTarget("from_lib") {
		t.Error("expected target 'from_lib' from included file")
	}
	if !b.HasTarget("all") {
		t.Error("expected target 'all' from root file")
	}
}

func TestNewBuild_RejectsUnresolvedInclude(t *testing.T) {
	// NewBuild (the byte-based path) doesn't know what dir to resolve includes
	// against. An Include directive that survives parsing must produce an
	// error that points the caller at NewBuildFromFile.
	_, err := NewBuild([]byte("include lib.mmk\n"))
	if err == nil {
		t.Fatal("expected error from NewBuild on unresolved include")
	}
	if !strings.Contains(err.Error(), "unresolved include") {
		t.Errorf("error should mention 'unresolved include'; got: %v", err)
	}
}

// --- PrintList: docstring-driven default vs -all ---

func printListString(t *testing.T, b *Build, all bool) string {
	t.Helper()
	var buf strings.Builder
	b.PrintList(&buf, all)
	return buf.String()
}

func TestPrintList_DefaultHidesUndocumented(t *testing.T) {
	b := newBuild(t, `
all : public_thing

## A target a user is meant to invoke.
public_thing : { :; }

internal_thing : { :; }
`)
	out := printListString(t, b, false)
	if !strings.Contains(out, "public_thing") {
		t.Errorf("default -list should include docstringed target; got:\n%s", out)
	}
	if !strings.Contains(out, "  all") {
		t.Errorf("default -list should always include 'all'; got:\n%s", out)
	}
	if strings.Contains(out, "internal_thing") {
		t.Errorf("default -list should NOT include undocumented target; got:\n%s", out)
	}
	if !strings.Contains(out, "1 target hidden") {
		t.Errorf("expected '1 target hidden' footer; got:\n%s", out)
	}
}

func TestPrintList_FileDescriptionPrintedAsHeader(t *testing.T) {
	b := newBuild(t, `
##! A tiny build.
##! Second line of the blurb.
all : public_thing

## A target a user is meant to invoke.
public_thing : { :; }
`)
	out := printListString(t, b, false)
	if !strings.HasPrefix(out, "A tiny build.\nSecond line of the blurb.\n\nTargets:") {
		t.Errorf("expected file description header before Targets:; got:\n%s", out)
	}
}

func TestPrintList_FileDescriptionShownRegardlessOfAll(t *testing.T) {
	b := newBuild(t, `
##! A tiny build.
all : { :; }
`)
	out := printListString(t, b, true)
	if !strings.HasPrefix(out, "A tiny build.\n") {
		t.Errorf("expected file description header with -all too; got:\n%s", out)
	}
}

func TestPrintList_NoFileDescriptionNoHeader(t *testing.T) {
	b := newBuild(t, `
all : { :; }
`)
	out := printListString(t, b, false)
	if !strings.HasPrefix(out, "Targets:") {
		t.Errorf("expected no header when file has no description; got:\n%s", out)
	}
}

func TestPrintList_AllShowsEverything(t *testing.T) {
	b := newBuild(t, `
all : public_thing

## A docstringed target.
public_thing : { :; }

internal_thing : { :; }
`)
	out := printListString(t, b, true)
	for _, want := range []string{"public_thing", "internal_thing", "all"} {
		if !strings.Contains(out, want) {
			t.Errorf("-all should include %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "hidden") {
		t.Errorf("-all should not show a 'hidden' footer; got:\n%s", out)
	}
}

func TestPrintList_AllAlwaysShownEvenWithoutDocstring(t *testing.T) {
	b := newBuild(t, `
all : foo
foo : { :; }
bar : { :; }
`)
	out := printListString(t, b, false)
	if !strings.Contains(out, "  all") {
		t.Errorf("'all' should always appear even without a docstring; got:\n%s", out)
	}
	// Without -all and with no docstrings on foo or bar, neither should
	// appear as its own row. (foo IS expected to appear inside all's
	// `→ foo` dep annotation, so we check row-prefix not substring.)
	for _, line := range strings.Split(out, "\n") {
		for _, hidden := range []string{"foo", "bar"} {
			if strings.HasPrefix(line, "  "+hidden+" ") || strings.HasPrefix(line, "  "+hidden+"\t") {
				t.Errorf("undocumented %q should be hidden as a row; got line: %q", hidden, line)
			}
		}
	}
}

func TestPrintList_PatternsHiddenByDefault(t *testing.T) {
	b := newBuild(t, `
'(.*)\.o' : $1.c { cc -c $1.c -o $target; }

## A docstringed target.
foo : { :; }
`)
	out := printListString(t, b, false)
	if strings.Contains(out, "Patterns:") {
		t.Errorf("default -list should hide Patterns section when nothing is docstringed; got:\n%s", out)
	}
	if !strings.Contains(out, "1 pattern hidden") {
		t.Errorf("expected '1 pattern hidden' footer; got:\n%s", out)
	}
}

func TestPrintList_DocstringedPatternShowsByDefault(t *testing.T) {
	b := newBuild(t, `
## Build .o from .c.
'(.*)\.o' : $1.c { cc -c $1.c -o $target; }

## A docstringed target.
foo : { :; }
`)
	out := printListString(t, b, false)
	if !strings.Contains(out, "Patterns:") {
		t.Errorf("docstringed pattern should surface the Patterns section; got:\n%s", out)
	}
	if !strings.Contains(out, "Build .o from .c.") {
		t.Errorf("docstringed pattern's description should appear; got:\n%s", out)
	}
}

func TestPrintList_VerbTargetsFiltered(t *testing.T) {
	// `clean` applies to every file target via the built-in defbody. Only
	// the docstringed target should appear; the rest are summarised.
	b := newBuild(t, `
## A user-facing artifact.
file public_artifact : { :; }

file internal_a : { :; }
file internal_b : { :; }
`)
	out := printListString(t, b, false)

	// Find the line for the clean verb.
	var cleanLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "clean") && strings.Contains(line, "internal targets") {
			cleanLine = line
			break
		}
		if strings.HasPrefix(strings.TrimSpace(line), "clean") {
			cleanLine = line
		}
	}
	if cleanLine == "" {
		t.Fatalf("no line for verb 'clean' found in:\n%s", out)
	}
	if !strings.Contains(cleanLine, "public_artifact") {
		t.Errorf("clean's verb line should list the docstringed target; got: %q", cleanLine)
	}
	if strings.Contains(cleanLine, "internal_a") || strings.Contains(cleanLine, "internal_b") {
		t.Errorf("clean's verb line should not list internal targets; got: %q", cleanLine)
	}
	if !strings.Contains(cleanLine, "+ 2 internal targets") {
		t.Errorf("clean's verb line should summarise '+ 2 internal targets'; got: %q", cleanLine)
	}
}

func TestPrintList_VerbTargetsAllShowsEverything(t *testing.T) {
	b := newBuild(t, `
## docstringed.
file public_artifact : { :; }

file internal_a : { :; }
`)
	out := printListString(t, b, true)
	for _, want := range []string{"public_artifact", "internal_a"} {
		if !strings.Contains(out, want) {
			t.Errorf("-all should list %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "internal targets") {
		t.Errorf("-all should not summarise internal targets; got:\n%s", out)
	}
}

func TestPrintList_GroupHiddenWithoutDocstring(t *testing.T) {
	b := newBuild(t, `
group g
foo into g : { :; }

## docstringed.
bar : { :; }
`)
	out := printListString(t, b, false)
	if strings.Contains(out, "  g ") || strings.Contains(out, "  g\t") {
		t.Errorf("undocumented group 'g' should be hidden by default; got:\n%s", out)
	}
}

func TestPrintList_GroupShowsWithDocstring(t *testing.T) {
	b := newBuild(t, `
## A pool of test cases.
group tests
foo into tests : { :; }
`)
	out := printListString(t, b, false)
	if !strings.Contains(out, "tests") || !strings.Contains(out, "A pool of test cases.") {
		t.Errorf("docstringed group should appear with its description; got:\n%s", out)
	}
}

func printTypesString(t *testing.T, b *Build, all bool) string {
	t.Helper()
	var buf strings.Builder
	b.PrintTypes(&buf, all)
	return buf.String()
}

func TestPrintTypes_ShowsDocOptionsAndVerbs(t *testing.T) {
	b := newBuild(t, `
## Builds a widget.
deftype widget flavor= { echo 1 }

defbody widget { true }

## Removes the built widget.
defbody widget clean { true }
`)
	out := printTypesString(t, b, false)
	if !strings.Contains(out, "widget") || !strings.Contains(out, "Builds a widget.") {
		t.Errorf("expected widget's docstring; got:\n%s", out)
	}
	if !strings.Contains(out, "Options: flavor=") {
		t.Errorf("expected declared option flavor= listed; got:\n%s", out)
	}
	if !strings.Contains(out, "build (default)") {
		t.Errorf("expected default build verb listed; got:\n%s", out)
	}
	if !strings.Contains(out, "clean") || !strings.Contains(out, "Removes the built widget.") {
		t.Errorf("expected clean verb with its docstring; got:\n%s", out)
	}
}

func TestPrintTypes_DefaultHidesUndocumented(t *testing.T) {
	b := newBuild(t, `
deftype widget { echo 1 }
defbody widget { true }
`)
	out := printTypesString(t, b, false)
	if strings.Contains(out, "widget") {
		t.Errorf("undocumented type should be hidden by default; got:\n%s", out)
	}
	if !strings.Contains(out, "hidden") {
		t.Errorf("expected a hidden-count footer; got:\n%s", out)
	}
}

func TestPrintTypes_AllShowsUndocumentedTypesAndBuiltins(t *testing.T) {
	b := newBuild(t, `
deftype widget { echo 1 }
defbody widget { true }
`)
	out := printTypesString(t, b, true)
	if !strings.Contains(out, "widget") {
		t.Errorf("expected undocumented type to show with -all; got:\n%s", out)
	}
	if !strings.Contains(out, "file") || !strings.Contains(out, "image") {
		t.Errorf("expected built-in types 'file' and 'image' to be listed; got:\n%s", out)
	}
}

func TestPrintTypes_BuiltinTypesShowBuiltinVerbs(t *testing.T) {
	b := newBuild(t, `all : { :; }`)
	out := printTypesString(t, b, true)
	for _, name := range []string{"file", "image", "directory"} {
		idx := strings.Index(out, name+" ")
		if idx < 0 {
			idx = strings.Index(out, name+"\t")
		}
		if idx < 0 {
			t.Fatalf("expected built-in type %q in output; got:\n%s", name, out)
		}
		section := out[idx:]
		if end := strings.Index(section[1:], "\n\n"); end >= 0 {
			section = section[:end+1]
		}
		if !strings.Contains(section, "clean") {
			t.Errorf("expected built-in type %q to list its 'clean' verb; got section:\n%s", name, section)
		}
	}
}
