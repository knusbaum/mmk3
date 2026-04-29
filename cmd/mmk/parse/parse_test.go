package parse

import (
	"strings"
	"testing"
)

func TestTargetNoTypeNoDepsNoBody(t *testing.T) {
	// A bare word with no ':' or '{' is passthrough; use 'clean :' for a
	// target with an explicit (empty) dep list and no body.
	f := mustParse(t, `clean :`)
	requireRules(t, f, 1)
	r := asRule(t, f.Directives[0])
	expect(t, "Type", r.Type, "")
	expect(t, "Target", r.Target, "clean")
	expect(t, "Runner", r.Runner, "")
	expectDeps(t, r.Deps)
	expect(t, "Body", r.Body, "")
}

func TestTargetWithBody(t *testing.T) {
	f := mustParse(t, `clean {
	rm -f *.o
}`)
	r := asRule(t, f.Directives[0])
	expect(t, "Target", r.Target, "clean")
	if r.Body == "" {
		t.Fatal("expected non-empty body")
	}
}

func TestTargetWithDeps(t *testing.T) {
	f := mustParse(t, `all : foo bar baz`)
	r := asRule(t, f.Directives[0])
	expect(t, "Target", r.Target, "all")
	expectDeps(t, r.Deps, "foo", "bar", "baz")
}

func TestTargetWithType(t *testing.T) {
	f := mustParse(t, `file main.o : main.c lib.h {
	cc -c main.c -o main.o
}`)
	r := asRule(t, f.Directives[0])
	expect(t, "Type", r.Type, "file")
	expect(t, "Target", r.Target, "main.o")
	expectDeps(t, r.Deps, "main.c", "lib.h")
}

func TestTargetWithRunner(t *testing.T) {
	f := mustParse(t, `main.o on ubuntu : main.c {
	cc -c main.c -o main.o
}`)
	r := asRule(t, f.Directives[0])
	expect(t, "Type", r.Type, "")
	expect(t, "Target", r.Target, "main.o")
	expect(t, "Runner", r.Runner, "ubuntu")
	expectDeps(t, r.Deps, "main.c")
}

func TestTargetWithTypeAndRunner(t *testing.T) {
	f := mustParse(t, `file main.o on ubuntu : main.c lib.h {
	cc -c main.c -o main.o
}`)
	r := asRule(t, f.Directives[0])
	expect(t, "Type", r.Type, "file")
	expect(t, "Target", r.Target, "main.o")
	expect(t, "Runner", r.Runner, "ubuntu")
	expectDeps(t, r.Deps, "main.c", "lib.h")
}

// --- pattern targets ---

func TestPatternTargetOnly(t *testing.T) {
	f := mustParse(t, `'(.*)\.o' : $1.c {
	cc -c $1.c -o $target
}`)
	r := asRule(t, f.Directives[0])
	expect(t, "Pattern", r.Pattern, `(.*)\.o`)
	expect(t, "Target", r.Target, "")
	expectDeps(t, r.Deps, "$1.c")
}

func TestPatternWithType(t *testing.T) {
	f := mustParse(t, `file '(.*)\.o' : $1.c {
	cc -c $1.c -o $target
}`)
	r := asRule(t, f.Directives[0])
	expect(t, "Type", r.Type, "file")
	expect(t, "Pattern", r.Pattern, `(.*)\.o`)
	expect(t, "Target", r.Target, "")
}

func TestPatternWithRunner(t *testing.T) {
	f := mustParse(t, `'(.*)\.o' on ubuntu : $1.c {
	cc -c $1.c -o $target
}`)
	r := asRule(t, f.Directives[0])
	expect(t, "Pattern", r.Pattern, `(.*)\.o`)
	expect(t, "Runner", r.Runner, "ubuntu")
}

func TestPatternWithTypeAndRunner(t *testing.T) {
	f := mustParse(t, `file '(.*)\.o' on ubuntu : $1.c {
	cc -c $1.c -o $target
}`)
	r := asRule(t, f.Directives[0])
	expect(t, "Type", r.Type, "file")
	expect(t, "Pattern", r.Pattern, `(.*)\.o`)
	expect(t, "Runner", r.Runner, "ubuntu")
}

func TestPatternNoDeps(t *testing.T) {
	f := mustParse(t, `'.*\.phony' {
	echo $target
}`)
	r := asRule(t, f.Directives[0])
	expect(t, "Pattern", r.Pattern, `.*\.phony`)
	expectDeps(t, r.Deps)
}

func TestPatternDepsAreConcreteOrVars(t *testing.T) {
	// deps may contain $1, $2 etc — they're just regular words to the parser
	f := mustParse(t, `'(.*)-(.*).o' : $1.c $2.c`)
	r := asRule(t, f.Directives[0])
	expect(t, "Pattern", r.Pattern, `(.*)-(.*).o`)
	expectDeps(t, r.Deps, "$1.c", "$2.c")
}

func TestDepArithmeticExpansion(t *testing.T) {
	// $((...)) in dep position must be parsed as a single token.
	f := mustParse(t, `'([0-9]+)' : $(( $1 - 1 )) $(( $1 - 2 )) {
	echo $target
}`)
	r := asRule(t, f.Directives[0])
	expectDeps(t, r.Deps, "$(( $1 - 1 ))", "$(( $1 - 2 ))")
	if r.Body == "" {
		t.Fatal("expected non-empty body")
	}
}

func TestDepCommandSubstitution(t *testing.T) {
	// $(...) command substitution in dep position is a single token.
	f := mustParse(t, `all : $(find . -name '*.c')`)
	r := asRule(t, f.Directives[0])
	expectDeps(t, r.Deps, "$(find . -name '*.c')")
}

func TestConcreteTargetUnaffected(t *testing.T) {
	// Existing concrete target behaviour must not change.
	f := mustParse(t, `file main.o : main.c lib.h {
	cc -c main.c -o main.o
}`)
	r := asRule(t, f.Directives[0])
	expect(t, "Target", r.Target, "main.o")
	expect(t, "Pattern", r.Pattern, "")
}

func TestErrorPatternAsType(t *testing.T) {
	expectError(t, `'file' main.o : dep {}`, "type cannot be a pattern")
}

func TestErrorPatternAsRunner(t *testing.T) {
	expectError(t, `foo on 'ubuntu' : dep {}`, "runner name cannot be a pattern")
}

func TestErrorPatternAsDep(t *testing.T) {
	expectError(t, `foo : 'dep' {}`, "single-quoted patterns are only valid as target names")
}

func TestErrorUnterminatedPattern(t *testing.T) {
	expectError(t, `'(.*)\.o`, "unterminated pattern string")
}

func TestDefType(t *testing.T) {
	f := mustParse(t, `deftype file {
	[[ -f "$target" ]] || return 1
}`)
	requireRules(t, f, 1)
	dt, ok := f.Directives[0].(*DefType)
	if !ok {
		t.Fatalf("expected *DefType, got %T", f.Directives[0])
	}
	expect(t, "Name", dt.Name, "file")
	if dt.Body == "" {
		t.Fatal("expected non-empty body")
	}
}

func TestDefRunner(t *testing.T) {
	f := mustParse(t, `defrunner ubuntu {
	docker run --rm ubuntu:latest "$@"
}`)
	requireRules(t, f, 1)
	dr, ok := f.Directives[0].(*DefRunner)
	if !ok {
		t.Fatalf("expected *DefRunner, got %T", f.Directives[0])
	}
	expect(t, "Name", dr.Name, "ubuntu")
	expect(t, "Phase", dr.Phase, "")
}

func TestDefRunnerSetup(t *testing.T) {
	f := mustParse(t, `defrunner ubuntu setup { echo setup }`)
	requireRules(t, f, 1)
	dr, ok := f.Directives[0].(*DefRunner)
	if !ok {
		t.Fatalf("expected *DefRunner, got %T", f.Directives[0])
	}
	expect(t, "Name", dr.Name, "ubuntu")
	expect(t, "Phase", dr.Phase, "setup")
}

func TestDefRunnerCleanup(t *testing.T) {
	f := mustParse(t, `defrunner ubuntu cleanup { echo cleanup }`)
	dr := f.Directives[0].(*DefRunner)
	expect(t, "Phase", dr.Phase, "cleanup")
}

func TestDefRunnerUnknownPhaseError(t *testing.T) {
	expectError(t, `defrunner ubuntu badphase { echo hi }`, "unknown phase")
}

func TestQuotedTargetName(t *testing.T) {
	f := mustParse(t, `"ubuntu:latest" : foo`)
	r := asRule(t, f.Directives[0])
	expect(t, "Target", r.Target, "ubuntu:latest")
	expectDeps(t, r.Deps, "foo")
}

func TestBareColonInTargetName(t *testing.T) {
	f := mustParse(t, `image buildimage:latest : buildcontainer/Dockerfile`)
	r := asRule(t, f.Directives[0])
	expect(t, "Type", r.Type, "image")
	expect(t, "Target", r.Target, "buildimage:latest")
	expectDeps(t, r.Deps, "buildcontainer/Dockerfile")
}

func TestBareColonInDepName(t *testing.T) {
	f := mustParse(t, `image newimage : baseimage:latest`)
	r := asRule(t, f.Directives[0])
	expect(t, "Target", r.Target, "newimage")
	expectDeps(t, r.Deps, "baseimage:latest")
}

func TestBareColonInRunnerName(t *testing.T) {
	f := mustParse(t, `file executable on buildimage:latest : main.c`)
	r := asRule(t, f.Directives[0])
	expect(t, "Target", r.Target, "executable")
	expect(t, "Runner", r.Runner, "buildimage:latest")
	expectDeps(t, r.Deps, "main.c")
}

func TestColonSeparatorWithNoSpace(t *testing.T) {
	// ':' immediately followed by space is still the separator, not part of the name.
	f := mustParse(t, `buildimage: dep`)
	r := asRule(t, f.Directives[0])
	expect(t, "Target", r.Target, "buildimage")
	expectDeps(t, r.Deps, "dep")
}

func TestMultipleDirectives(t *testing.T) {
	src := `
deftype file {
	[[ -f "$target" ]] || return 1
}

defrunner ubuntu {
	docker run --rm ubuntu:latest "$@"
}

file main.o : main.c lib.h {
	cc -c main.c -o main.o
}

all : main.o
`
	f := mustParse(t, src)
	requireRules(t, f, 4)
	if _, ok := f.Directives[0].(*DefType); !ok {
		t.Errorf("directive 0: expected *DefType, got %T", f.Directives[0])
	}
	if _, ok := f.Directives[1].(*DefRunner); !ok {
		t.Errorf("directive 1: expected *DefRunner, got %T", f.Directives[1])
	}
	asRule(t, f.Directives[2])
	asRule(t, f.Directives[3])
}

func TestComments(t *testing.T) {
	f := mustParse(t, `
# this is a comment
file main.o : main.c # inline comment
`)
	r := asRule(t, f.Directives[0])
	expect(t, "Target", r.Target, "main.o")
	expectDeps(t, r.Deps, "main.c")
}

func TestBodyOnNextLine(t *testing.T) {
	f := mustParse(t, `clean
{
	rm -f *.o
}`)
	r := asRule(t, f.Directives[0])
	expect(t, "Target", r.Target, "clean")
	if r.Body == "" {
		t.Fatal("expected body")
	}
}

func TestNestedBracesInBody(t *testing.T) {
	f := mustParse(t, `foo {
	if [[ -f bar ]]; then
		echo yes
	fi
}`)
	r := asRule(t, f.Directives[0])
	if r.Body == "" {
		t.Fatal("expected body")
	}
}

func TestNestedFunctionInBody(t *testing.T) {
	f := mustParse(t, `file foo on docker : dep {
	function whatever {
		echo hello;
	}
	whatever;
}`)
	r := asRule(t, f.Directives[0])
	expect(t, "Type", r.Type, "file")
	expect(t, "Target", r.Target, "foo")
	expect(t, "Runner", r.Runner, "docker")
	expectDeps(t, r.Deps, "dep")
	if r.Body == "" {
		t.Fatal("expected body")
	}
}

// Unbalanced braces inside double-quoted strings must not confuse the parser.
func TestUnbalancedBraceInDoubleQuote(t *testing.T) {
	f := mustParse(t, `foo {
	echo "{"
	echo "}"
}

bar {
	echo done
}`)
	if len(f.Directives) != 2 {
		t.Fatalf("expected 2 directives, got %d", len(f.Directives))
	}
	asRule(t, f.Directives[0])
	asRule(t, f.Directives[1])
}

// Unbalanced braces inside single-quoted strings must not confuse the parser.
func TestUnbalancedBraceInSingleQuote(t *testing.T) {
	f := mustParse(t, `foo {
	echo '{'
}

bar : foo`)
	if len(f.Directives) != 2 {
		t.Fatalf("expected 2 directives, got %d", len(f.Directives))
	}
}

// Braces in bash # comments must not confuse the parser.
func TestBraceInComment(t *testing.T) {
	f := mustParse(t, `foo {
	# open { and close } braces in comment
	echo done
}`)
	r := asRule(t, f.Directives[0])
	if r.Body == "" {
		t.Fatal("expected body")
	}
}

func TestUnbalancedBraceInBacktick(t *testing.T) {
	f := mustParse(t, "foo {\n\tresult=`echo {`\n}")
	r := asRule(t, f.Directives[0])
	if r.Body == "" {
		t.Fatal("expected body")
	}
}

func TestMultipleNestedFunctions(t *testing.T) {
	f := mustParse(t, `build {
	setup() { mkdir -p out; }
	compile() {
		cc -o out/a main.c
	}
	setup
	compile
}`)
	r := asRule(t, f.Directives[0])
	if r.Body == "" {
		t.Fatal("expected body")
	}
}

// --- passthrough bash function definitions ---

func TestDefBody(t *testing.T) {
	f := mustParse(t, `defbody image {
	docker build -t $target -f ${deps%% *} .
}`)
	requireRules(t, f, 1)
	db, ok := f.Directives[0].(*DefBody)
	if !ok {
		t.Fatalf("expected *DefBody, got %T", f.Directives[0])
	}
	expect(t, "Type", db.Type, "image")
	if db.Body == "" {
		t.Fatal("expected non-empty body")
	}
}

func TestBashFunctionDefIsPassthrough(t *testing.T) {
	src := `dockerimage() {
	docker build -t $target .
}`
	f := mustParse(t, src)
	if len(f.Directives) != 3 {
		t.Fatalf("expected 3 directives (open-brace line + body + close-brace), got %d", len(f.Directives))
	}
	for i, d := range f.Directives {
		if _, ok := d.(*Passthrough); !ok {
			t.Errorf("directive %d: expected *Passthrough, got %T", i, d)
		}
	}
}

func TestBashFunctionDefWithSpaceBeforeParen(t *testing.T) {
	src := `dockerimage () { docker build; }`
	f := mustParse(t, src)
	if len(f.Directives) != 1 {
		t.Fatalf("expected 1 passthrough directive, got %d", len(f.Directives))
	}
	if _, ok := f.Directives[0].(*Passthrough); !ok {
		t.Errorf("expected *Passthrough, got %T", f.Directives[0])
	}
}

// --- passthrough ---

func TestPassthroughVariableAssignment(t *testing.T) {
	f := mustParse(t, `FOO=bar`)
	if len(f.Directives) != 1 {
		t.Fatalf("expected 1 directive, got %d", len(f.Directives))
	}
	pt, ok := f.Directives[0].(*Passthrough)
	if !ok {
		t.Fatalf("expected *Passthrough, got %T", f.Directives[0])
	}
	if pt.Line != "FOO=bar" {
		t.Errorf("Line: got %q, want %q", pt.Line, "FOO=bar")
	}
}

func TestTargetRuleOptions(t *testing.T) {
	f := mustParse(t, `image myimg:1 platform=linux/amd64 user=1000:1000 : Dockerfile`)
	r := asRule(t, f.Directives[0])
	if r.Type != "image" || r.Target != "myimg:1" {
		t.Fatalf("unexpected header: type=%q target=%q", r.Type, r.Target)
	}
	want := []Option{{Key: "platform", Value: "linux/amd64"}, {Key: "user", Value: "1000:1000"}}
	if len(r.Options) != len(want) {
		t.Fatalf("options: got %v, want %v", r.Options, want)
	}
	for i, opt := range r.Options {
		if opt != want[i] {
			t.Errorf("Options[%d]: got %v, want %v", i, opt, want[i])
		}
	}
	if len(r.Deps) != 1 || r.Deps[0].Target != "Dockerfile" {
		t.Errorf("deps: got %v, want [Dockerfile]", r.Deps)
	}
}

func TestTargetRuleOptionsAfterRunner(t *testing.T) {
	f := mustParse(t, `windows-shell on myimg:1 tty=true { bash }`)
	r := asRule(t, f.Directives[0])
	if r.Runner != "myimg:1" {
		t.Errorf("Runner: got %q, want %q", r.Runner, "myimg:1")
	}
	if len(r.Options) != 1 || r.Options[0].Key != "tty" || r.Options[0].Value != "true" {
		t.Errorf("Options: got %v, want [{tty true}]", r.Options)
	}
}

func TestTargetRuleOptionInterspersed(t *testing.T) {
	// Options can appear before or after the `on` clause.
	f := mustParse(t, `build platform=arm on myimg:1 mode=debug { :; }`)
	r := asRule(t, f.Directives[0])
	if r.Target != "build" || r.Runner != "myimg:1" {
		t.Errorf("header parse: target=%q runner=%q", r.Target, r.Runner)
	}
	if len(r.Options) != 2 {
		t.Fatalf("Options len: got %d, want 2: %v", len(r.Options), r.Options)
	}
}

func TestTargetRuleOptionQuotedValue(t *testing.T) {
	f := mustParse(t, `image myimg forward_env="A B C" : Dockerfile`)
	r := asRule(t, f.Directives[0])
	if len(r.Options) != 1 {
		t.Fatalf("Options: got %v", r.Options)
	}
	if r.Options[0].Key != "forward_env" || r.Options[0].Value != "A B C" {
		t.Errorf("Option: got %v, want {forward_env A B C}", r.Options[0])
	}
}

func TestTargetRuleReservedOptionKey(t *testing.T) {
	expectError(t, `foo target=oops : bar`, "reserved")
	expectError(t, `foo deps=oops : bar`, "reserved")
	expectError(t, `foo MMK_FOO=x : bar`, "reserved")
}

func TestDocstringAttachesToTargetRule(t *testing.T) {
	src := `
## Build the C library.
foo : { :; }
`
	f := mustParse(t, src)
	r := asRule(t, f.Directives[0])
	if r.Description != "Build the C library." {
		t.Errorf("Description: got %q, want %q", r.Description, "Build the C library.")
	}
}

func TestDocstringMultilineConcatenates(t *testing.T) {
	src := `
## First line.
## Second line.
foo : { :; }
`
	f := mustParse(t, src)
	r := asRule(t, f.Directives[0])
	want := "First line.\nSecond line."
	if r.Description != want {
		t.Errorf("Description: got %q, want %q", r.Description, want)
	}
}

func TestDocstringResetByRegularComment(t *testing.T) {
	src := `
## First doc.
# regular comment
foo : { :; }
`
	f := mustParse(t, src)
	r := asRule(t, f.Directives[0])
	if r.Description != "" {
		t.Errorf("Description: got %q, want empty (reset by regular comment)", r.Description)
	}
}

func TestDocstringSurvivesBlankLines(t *testing.T) {
	src := `
## Doc.

foo : { :; }
`
	f := mustParse(t, src)
	r := asRule(t, f.Directives[0])
	if r.Description != "Doc." {
		t.Errorf("Description: got %q, want %q", r.Description, "Doc.")
	}
}

func TestDocstringDoesNotLeakBetweenDirectives(t *testing.T) {
	src := `
## Doc for foo.
foo : { :; }

bar : { :; }
`
	f := mustParse(t, src)
	foo := asRule(t, f.Directives[0])
	bar := asRule(t, f.Directives[1])
	if foo.Description != "Doc for foo." {
		t.Errorf("foo.Description: got %q", foo.Description)
	}
	if bar.Description != "" {
		t.Errorf("bar.Description should be empty; got %q", bar.Description)
	}
}

func TestSubprojectBare(t *testing.T) {
	f := mustParse(t, `subproject src`)
	if len(f.Directives) != 1 {
		t.Fatalf("expected 1 directive, got %d", len(f.Directives))
	}
	sp, ok := f.Directives[0].(*Subproject)
	if !ok {
		t.Fatalf("expected *Subproject, got %T", f.Directives[0])
	}
	expect(t, "Target", sp.Target, "src")
	expect(t, "Runner", sp.Runner, "")
}

func TestSubprojectWithRunner(t *testing.T) {
	f := mustParse(t, `subproject src on injector-build:1`)
	sp := f.Directives[0].(*Subproject)
	expect(t, "Target", sp.Target, "src")
	expect(t, "Runner", sp.Runner, "injector-build:1")
}

func TestSubprojectWithOptions(t *testing.T) {
	f := mustParse(t, `subproject src path=lib on $IMG`)
	sp := f.Directives[0].(*Subproject)
	expect(t, "Target", sp.Target, "src")
	expect(t, "Runner", sp.Runner, "$IMG")
	if len(sp.Options) != 1 || sp.Options[0].Key != "path" || sp.Options[0].Value != "lib" {
		t.Errorf("Options: got %v, want [{path lib}]", sp.Options)
	}
}

func TestSubprojectRejectsBody(t *testing.T) {
	expectError(t, `subproject src { foo }`, "does not take a body")
}

func TestVerbRuleAugmentDeps(t *testing.T) {
	f := mustParse(t, `[clean all] :+ extra1 extra2`)
	r := asRule(t, f.Directives[0])
	if r.Verb != "clean" || r.Target != "all" {
		t.Fatalf("unexpected header: verb=%q target=%q", r.Verb, r.Target)
	}
	if !r.HasDepSep {
		t.Error("HasDepSep: got false, want true")
	}
	if !r.AugmentDeps {
		t.Error("AugmentDeps: got false, want true")
	}
	expectDeps(t, r.Deps, "extra1", "extra2")
}

func TestVerbRuleColonOnly(t *testing.T) {
	// Plain ':' is not augment.
	f := mustParse(t, `[clean all] : extra`)
	r := asRule(t, f.Directives[0])
	if !r.HasDepSep || r.AugmentDeps {
		t.Errorf("HasDepSep=%v AugmentDeps=%v; want true,false", r.HasDepSep, r.AugmentDeps)
	}
}

func TestAugmentSepRejectedOnNonVerbRule(t *testing.T) {
	expectError(t, `foo :+ bar`, ":+")
}

func TestPassthroughVariableAssignmentWithColon(t *testing.T) {
	// Without the IDENT=... heuristic this gets parsed as a target rule
	// because of the embedded ':'.
	f := mustParse(t, `IMG=ubuntu:latest`)
	if len(f.Directives) != 1 {
		t.Fatalf("expected 1 directive, got %d", len(f.Directives))
	}
	pt, ok := f.Directives[0].(*Passthrough)
	if !ok {
		t.Fatalf("expected *Passthrough, got %T", f.Directives[0])
	}
	if pt.Line != "IMG=ubuntu:latest" {
		t.Errorf("Line: got %q, want %q", pt.Line, "IMG=ubuntu:latest")
	}
}

func TestPassthroughForLoop(t *testing.T) {
	src := `
FOO=bar
for o in $FOO; do
clean :
done
`
	f := mustParse(t, src)
	if len(f.Directives) != 4 {
		t.Fatalf("expected 4 directives, got %d", len(f.Directives))
	}
	if _, ok := f.Directives[0].(*Passthrough); !ok {
		t.Errorf("directive 0: expected *Passthrough, got %T", f.Directives[0])
	}
	if _, ok := f.Directives[1].(*Passthrough); !ok {
		t.Errorf("directive 1: expected *Passthrough, got %T", f.Directives[1])
	}
	asRule(t, f.Directives[2])
	if _, ok := f.Directives[3].(*Passthrough); !ok {
		t.Errorf("directive 3: expected *Passthrough, got %T", f.Directives[3])
	}
}

func TestPassthroughMixedWithDirectives(t *testing.T) {
	src := `
OBJECTS=main

file main.o : main.c {
	cc -c main.c -o main.o
}
`
	f := mustParse(t, src)
	if len(f.Directives) != 2 {
		t.Fatalf("expected 2 directives, got %d", len(f.Directives))
	}
	if _, ok := f.Directives[0].(*Passthrough); !ok {
		t.Errorf("directive 0: expected *Passthrough, got %T", f.Directives[0])
	}
	asRule(t, f.Directives[1])
}

// --- verb rules ---

func TestVerbRuleDeclaration(t *testing.T) {
	f := mustParse(t, `[clean executable] :`)
	r := asRule(t, f.Directives[0])
	expect(t, "Verb", r.Verb, "clean")
	expect(t, "Target", r.Target, "executable")
	expectDeps(t, r.Deps)
}

func TestVerbRuleWithBody(t *testing.T) {
	f := mustParse(t, `[clean executable] : [delete main.o] {
	rm executable
}`)
	r := asRule(t, f.Directives[0])
	expect(t, "Verb", r.Verb, "clean")
	expect(t, "Target", r.Target, "executable")
	if r.Body == "" {
		t.Fatal("expected non-empty body")
	}
	if len(r.Deps) != 1 || r.Deps[0].Target != "main.o" || r.Deps[0].Verb != "delete" {
		t.Errorf("deps: got %v, want [{main.o delete}]", r.Deps)
	}
}

func TestVerbDepInDepList(t *testing.T) {
	f := mustParse(t, `[install all] : [install foo] bar`)
	r := asRule(t, f.Directives[0])
	expect(t, "Verb", r.Verb, "install")
	expect(t, "Target", r.Target, "all")
	if len(r.Deps) != 2 {
		t.Fatalf("expected 2 deps, got %d", len(r.Deps))
	}
	if r.Deps[0].Target != "foo" || r.Deps[0].Verb != "install" {
		t.Errorf("deps[0]: got %+v, want {foo install}", r.Deps[0])
	}
	if r.Deps[1].Target != "bar" || r.Deps[1].Verb != "" {
		t.Errorf("deps[1]: got %+v, want {bar }", r.Deps[1])
	}
}

func TestDefBodyWithVerb(t *testing.T) {
	f := mustParse(t, `defbody file clean {
	rm "$target"
}`)
	requireRules(t, f, 1)
	db, ok := f.Directives[0].(*DefBody)
	if !ok {
		t.Fatalf("expected *DefBody, got %T", f.Directives[0])
	}
	expect(t, "Type", db.Type, "file")
	expect(t, "Verb", db.Verb, "clean")
	if db.Body == "" {
		t.Fatal("expected non-empty body")
	}
}

func TestDefBodyNoVerbUnchanged(t *testing.T) {
	f := mustParse(t, `defbody image {
	docker build -t "$target" .
}`)
	db := f.Directives[0].(*DefBody)
	expect(t, "Type", db.Type, "image")
	expect(t, "Verb", db.Verb, "")
}

// --- error cases ---

func TestErrorMissingTarget(t *testing.T) {
	expectError(t, `{}`, "expected target")
}

func TestErrorUnclosedBody(t *testing.T) {
	err := expectError(t, `foo {`, "")
	assertContains(t, err, "line 1")
}

func TestErrorUnclosedBodyReportsOpenLine(t *testing.T) {
	// Body opens on line 3; EOF on line 5. Error should reference line 3.
	src := "\n\nfoo {\n\techo hi\n"
	err := expectError(t, src, "")
	assertContains(t, err, "line 3")
}

func TestErrorUnclosedString(t *testing.T) {
	expectError(t, `"foo`, "")
}

func TestErrorUnclosedStringInBody(t *testing.T) {
	err := expectError(t, "foo {\n\techo \"hello\n}", "")
	assertContains(t, err, "line 2") // string opened on line 2
}

func TestErrorUnclosedSingleQuoteInBody(t *testing.T) {
	expectError(t, "foo {\n\techo 'hello\n}", "")
}

func TestErrorTooManyHeaderTokens(t *testing.T) {
	expectError(t, `type target extra : dep {}`, "unexpected tokens")
}

func TestErrorMissingRunnerAfterOn(t *testing.T) {
	expectError(t, `foo on : dep {}`, "runner name after 'on'")
}

func TestErrorTooManyRunnersAfterOn(t *testing.T) {
	expectError(t, `foo on r1 r2 : dep {}`, "runner name after 'on'")
}

func TestErrorMissingTargetBeforeOn(t *testing.T) {
	expectError(t, `on ubuntu : dep {}`, "target name before 'on'")
}

func TestErrorDefTypeMissingName(t *testing.T) {
	expectError(t, `deftype {`, "expected name")
}

func TestErrorDefTypeMissingBrace(t *testing.T) {
	expectError(t, "deftype file\nnext : dep", "expected '{'")
}

func TestErrorDefRunnerMissingBrace(t *testing.T) {
	expectError(t, "defrunner ubuntu\nnext : dep", "expected '{'")
}

func TestErrorLineNumberInHeader(t *testing.T) {
	// Error on line 3
	src := "\n\ntype target extra : dep {}"
	err := expectError(t, src, "unexpected tokens")
	assertContains(t, err, "line 3")
}

// --- matrix: for clauses ---

func TestMatrixSingleForClause(t *testing.T) {
	f := mustParse(t, `build for os in [linux macos] { echo $os }`)
	r := asRule(t, f.Directives[0])
	expect(t, "Target", r.Target, "build")
	if len(r.ForClauses) != 1 {
		t.Fatalf("ForClauses: got %d, want 1", len(r.ForClauses))
	}
	if r.ForClauses[0].Var != "os" {
		t.Errorf("ForClauses[0].Var: got %q, want %q", r.ForClauses[0].Var, "os")
	}
	if r.ForClauses[0].Expr != "linux macos" {
		t.Errorf("ForClauses[0].Expr: got %q, want %q", r.ForClauses[0].Expr, "linux macos")
	}
	if r.Body == "" {
		t.Error("expected non-empty body")
	}
}

func TestMatrixMultipleForClauses(t *testing.T) {
	f := mustParse(t, `build for os in [linux macos] for go in [1.20 1.21] : src { echo hi }`)
	r := asRule(t, f.Directives[0])
	if len(r.ForClauses) != 2 {
		t.Fatalf("ForClauses: got %d, want 2", len(r.ForClauses))
	}
	if r.ForClauses[0].Var != "os" || r.ForClauses[0].Expr != "linux macos" {
		t.Errorf("ForClauses[0]: got {%q %q}", r.ForClauses[0].Var, r.ForClauses[0].Expr)
	}
	if r.ForClauses[1].Var != "go" || r.ForClauses[1].Expr != "1.20 1.21" {
		t.Errorf("ForClauses[1]: got {%q %q}", r.ForClauses[1].Var, r.ForClauses[1].Expr)
	}
	expectDeps(t, r.Deps, "src")
}

func TestMatrixForClauseWithType(t *testing.T) {
	f := mustParse(t, `file build for os in [linux] : src { :; }`)
	r := asRule(t, f.Directives[0])
	expect(t, "Type", r.Type, "file")
	expect(t, "Target", r.Target, "build")
	if len(r.ForClauses) != 1 || r.ForClauses[0].Var != "os" {
		t.Errorf("ForClauses: got %v", r.ForClauses)
	}
}

func TestMatrixForClauseBeforeOn(t *testing.T) {
	f := mustParse(t, `build for os in [linux macos] on myrunner : src { :; }`)
	r := asRule(t, f.Directives[0])
	expect(t, "Target", r.Target, "build")
	expect(t, "Runner", r.Runner, "myrunner")
	if len(r.ForClauses) != 1 || r.ForClauses[0].Var != "os" {
		t.Fatalf("ForClauses: got %v", r.ForClauses)
	}
}

func TestMatrixForClauseOnWithVar(t *testing.T) {
	f := mustParse(t, `build for os in [linux macos] on runner-$os : src { :; }`)
	r := asRule(t, f.Directives[0])
	expect(t, "Runner", r.Runner, "runner-$os")
	if len(r.ForClauses) != 1 {
		t.Fatalf("ForClauses: got %d, want 1", len(r.ForClauses))
	}
}

func TestMatrixForClauseKeywordsAsValues(t *testing.T) {
	// 'on', 'in', 'for', 'exclude' must be usable as values inside [...]
	f := mustParse(t, `build for word in [on in for exclude] { :; }`)
	r := asRule(t, f.Directives[0])
	if len(r.ForClauses) != 1 {
		t.Fatalf("ForClauses: got %d, want 1", len(r.ForClauses))
	}
	if r.ForClauses[0].Expr != "on in for exclude" {
		t.Errorf("Expr: got %q, want %q", r.ForClauses[0].Expr, "on in for exclude")
	}
}

func TestMatrixForClauseWithBashVar(t *testing.T) {
	f := mustParse(t, `build for os in [$PLATFORMS] { :; }`)
	r := asRule(t, f.Directives[0])
	if len(r.ForClauses) != 1 || r.ForClauses[0].Expr != "$PLATFORMS" {
		t.Errorf("ForClauses[0].Expr: got %q, want %q", r.ForClauses[0].Expr, "$PLATFORMS")
	}
}

func TestMatrixForClauseWithSubshell(t *testing.T) {
	f := mustParse(t, `build for os in [$(cat platforms.txt)] { :; }`)
	r := asRule(t, f.Directives[0])
	if len(r.ForClauses) != 1 || r.ForClauses[0].Expr != "$(cat platforms.txt)" {
		t.Errorf("ForClauses[0].Expr: got %q, want %q", r.ForClauses[0].Expr, "$(cat platforms.txt)")
	}
}

func TestMatrixForClauseNestedBrackets(t *testing.T) {
	// Nested [...] inside the expr must be balanced correctly.
	f := mustParse(t, `build for os in [linux [extra]] { :; }`)
	r := asRule(t, f.Directives[0])
	if len(r.ForClauses) != 1 || r.ForClauses[0].Expr != "linux [extra]" {
		t.Errorf("ForClauses[0].Expr: got %q, want %q", r.ForClauses[0].Expr, "linux [extra]")
	}
}

// --- matrix: exclude clauses ---

func TestMatrixExcludeSingleClause(t *testing.T) {
	f := mustParse(t, `build for os in [linux macos] for go in [1.20 1.21] exclude [os=macos go=1.20] { :; }`)
	r := asRule(t, f.Directives[0])
	if len(r.Excludes) != 1 {
		t.Fatalf("Excludes: got %d, want 1", len(r.Excludes))
	}
	exc := r.Excludes[0]
	if len(exc) != 2 {
		t.Fatalf("Excludes[0] len: got %d, want 2", len(exc))
	}
	if exc[0].Key != "os" || exc[0].Value != "macos" {
		t.Errorf("Excludes[0][0]: got %v, want {os macos}", exc[0])
	}
	if exc[1].Key != "go" || exc[1].Value != "1.20" {
		t.Errorf("Excludes[0][1]: got %v, want {go 1.20}", exc[1])
	}
}

func TestMatrixExcludeMultipleClauses(t *testing.T) {
	f := mustParse(t, `build for os in [linux macos] for go in [1.20 1.21] exclude [os=macos go=1.20] exclude [os=linux go=1.21] { :; }`)
	r := asRule(t, f.Directives[0])
	if len(r.Excludes) != 2 {
		t.Fatalf("Excludes: got %d, want 2", len(r.Excludes))
	}
}

func TestMatrixExcludePartialCombo(t *testing.T) {
	// An exclude with fewer keys than the full combo is valid (partial match).
	f := mustParse(t, `build for os in [linux macos] for go in [1.20 1.21] exclude [os=macos] { :; }`)
	r := asRule(t, f.Directives[0])
	if len(r.Excludes) != 1 || len(r.Excludes[0]) != 1 {
		t.Fatalf("Excludes: got %v", r.Excludes)
	}
	if r.Excludes[0][0].Key != "os" || r.Excludes[0][0].Value != "macos" {
		t.Errorf("Excludes[0][0]: got %v", r.Excludes[0][0])
	}
}

// --- matrix: dep combo references ---

func TestMatrixDepSingleWord(t *testing.T) {
	// [target] in dep list: no verb, no combo — new valid form.
	f := mustParse(t, `foo : [build]`)
	r := asRule(t, f.Directives[0])
	if len(r.Deps) != 1 {
		t.Fatalf("Deps: got %d, want 1", len(r.Deps))
	}
	d := r.Deps[0]
	if d.Target != "build" {
		t.Errorf("Target: got %q, want %q", d.Target, "build")
	}
	if d.Verb != "" {
		t.Errorf("Verb: got %q, want empty", d.Verb)
	}
	if len(d.Combo) != 0 {
		t.Errorf("Combo: got %v, want empty", d.Combo)
	}
}

func TestMatrixDepComboNoVerb(t *testing.T) {
	f := mustParse(t, `foo : [build @ os=linux go=1.20]`)
	r := asRule(t, f.Directives[0])
	if len(r.Deps) != 1 {
		t.Fatalf("Deps: got %d, want 1", len(r.Deps))
	}
	d := r.Deps[0]
	if d.Target != "build" {
		t.Errorf("Target: got %q, want %q", d.Target, "build")
	}
	if d.Verb != "" {
		t.Errorf("Verb: got %q, want empty", d.Verb)
	}
	if len(d.Combo) != 2 {
		t.Fatalf("Combo len: got %d, want 2", len(d.Combo))
	}
	if d.Combo[0].Key != "os" || d.Combo[0].Value != "linux" {
		t.Errorf("Combo[0]: got %v, want {os linux}", d.Combo[0])
	}
	if d.Combo[1].Key != "go" || d.Combo[1].Value != "1.20" {
		t.Errorf("Combo[1]: got %v, want {go 1.20}", d.Combo[1])
	}
}

func TestMatrixDepComboWithVerb(t *testing.T) {
	f := mustParse(t, `foo : [check build @ os=linux]`)
	r := asRule(t, f.Directives[0])
	if len(r.Deps) != 1 {
		t.Fatalf("Deps: got %d, want 1", len(r.Deps))
	}
	d := r.Deps[0]
	if d.Verb != "check" {
		t.Errorf("Verb: got %q, want %q", d.Verb, "check")
	}
	if d.Target != "build" {
		t.Errorf("Target: got %q, want %q", d.Target, "build")
	}
	if len(d.Combo) != 1 || d.Combo[0].Key != "os" || d.Combo[0].Value != "linux" {
		t.Errorf("Combo: got %v, want [{os linux}]", d.Combo)
	}
}

func TestMatrixDepMixedWithPlainDeps(t *testing.T) {
	f := mustParse(t, `foo : [build @ os=linux] plain [check other] dep2`)
	r := asRule(t, f.Directives[0])
	if len(r.Deps) != 4 {
		t.Fatalf("Deps: got %d, want 4", len(r.Deps))
	}
	if r.Deps[0].Target != "build" || len(r.Deps[0].Combo) != 1 {
		t.Errorf("Deps[0]: got %+v", r.Deps[0])
	}
	if r.Deps[1].Target != "plain" || len(r.Deps[1].Combo) != 0 {
		t.Errorf("Deps[1]: got %+v", r.Deps[1])
	}
	if r.Deps[2].Target != "other" || r.Deps[2].Verb != "check" {
		t.Errorf("Deps[2]: got %+v", r.Deps[2])
	}
	if r.Deps[3].Target != "dep2" {
		t.Errorf("Deps[3]: got %+v", r.Deps[3])
	}
}

func TestMatrixMultipleForClausesWithVarOnClause(t *testing.T) {
	// Two for clauses combined with 'on runner-$os' (var from first for).
	f := mustParse(t, `build for os in [linux macos] for go in [1.20 1.21] on runner-$os : src { :; }`)
	r := asRule(t, f.Directives[0])
	if len(r.ForClauses) != 2 {
		t.Fatalf("ForClauses: got %d, want 2", len(r.ForClauses))
	}
	if r.ForClauses[0].Var != "os" || r.ForClauses[0].Expr != "linux macos" {
		t.Errorf("ForClauses[0]: got {%q %q}", r.ForClauses[0].Var, r.ForClauses[0].Expr)
	}
	if r.ForClauses[1].Var != "go" || r.ForClauses[1].Expr != "1.20 1.21" {
		t.Errorf("ForClauses[1]: got {%q %q}", r.ForClauses[1].Var, r.ForClauses[1].Expr)
	}
	expect(t, "Runner", r.Runner, "runner-$os")
}

func TestMatrixDepComboThreeKeyVals(t *testing.T) {
	// Three key=val pairs in a combo dep specifier.
	f := mustParse(t, `foo : [build @ os=linux libc=musl go=1.21]`)
	r := asRule(t, f.Directives[0])
	if len(r.Deps) != 1 {
		t.Fatalf("Deps: got %d, want 1", len(r.Deps))
	}
	d := r.Deps[0]
	if d.Target != "build" {
		t.Errorf("Target: got %q, want %q", d.Target, "build")
	}
	if len(d.Combo) != 3 {
		t.Fatalf("Combo len: got %d, want 3", len(d.Combo))
	}
	if d.Combo[0].Key != "os" || d.Combo[0].Value != "linux" {
		t.Errorf("Combo[0]: got %v", d.Combo[0])
	}
	if d.Combo[1].Key != "libc" || d.Combo[1].Value != "musl" {
		t.Errorf("Combo[1]: got %v", d.Combo[1])
	}
	if d.Combo[2].Key != "go" || d.Combo[2].Value != "1.21" {
		t.Errorf("Combo[2]: got %v", d.Combo[2])
	}
}

// --- matrix: error cases ---

func TestMatrixErrorForMissingVarName(t *testing.T) {
	// 'in' gets consumed as the var name, then the parser can't find the 'in' keyword.
	expectError(t, `build for in [linux] { :; }`, "'in'")
}

func TestMatrixErrorForMissingIn(t *testing.T) {
	expectError(t, `build for os [linux] { :; }`, "'in'")
}

func TestMatrixErrorForMissingBracket(t *testing.T) {
	expectError(t, `build for os in linux macos { :; }`, "'['")
}

func TestMatrixErrorExcludeMissingBracket(t *testing.T) {
	expectError(t, `build for os in [linux] exclude os=linux { :; }`, "'['")
}

func TestMatrixErrorUnterminatedBracketExpr(t *testing.T) {
	expectError(t, `build for os in [linux macos { :; }`, "unterminated")
}

func TestMatrixErrorExcludeNonKeyVal(t *testing.T) {
	expectError(t, `build for os in [linux] exclude [linux] { :; }`, "key=value")
}

// --- group directives ---

func TestGroupDirective(t *testing.T) {
	f := mustParse(t, `group mygroup`)
	if len(f.Directives) != 1 {
		t.Fatalf("expected 1 directive, got %d", len(f.Directives))
	}
	g, ok := f.Directives[0].(*Group)
	if !ok {
		t.Fatalf("expected *Group, got %T", f.Directives[0])
	}
	expect(t, "Name", g.Name, "mygroup")
}

func TestGroupDirectiveWithDocstring(t *testing.T) {
	src := `
## All test cases.
group tests
`
	f := mustParse(t, src)
	g, ok := f.Directives[0].(*Group)
	if !ok {
		t.Fatalf("expected *Group, got %T", f.Directives[0])
	}
	expect(t, "Name", g.Name, "tests")
	expect(t, "Description", g.Description, "All test cases.")
}

func TestGroupDirectiveMultiple(t *testing.T) {
	f := mustParse(t, "group g1\ngroup g2\n")
	if len(f.Directives) != 2 {
		t.Fatalf("expected 2 directives, got %d", len(f.Directives))
	}
	g1, ok := f.Directives[0].(*Group)
	if !ok {
		t.Fatalf("directive 0: expected *Group, got %T", f.Directives[0])
	}
	g2, ok2 := f.Directives[1].(*Group)
	if !ok2 {
		t.Fatalf("directive 1: expected *Group, got %T", f.Directives[1])
	}
	expect(t, "g1.Name", g1.Name, "g1")
	expect(t, "g2.Name", g2.Name, "g2")
}

// --- into clauses ---

func TestIntoClause(t *testing.T) {
	f := mustParse(t, `mytarget into mygroup :`)
	r := asRule(t, f.Directives[0])
	expect(t, "Target", r.Target, "mytarget")
	if len(r.Groups) != 1 || r.Groups[0] != "mygroup" {
		t.Errorf("Groups: got %v, want [mygroup]", r.Groups)
	}
}

func TestIntoClauseMultiple(t *testing.T) {
	f := mustParse(t, `mytarget into g1 into g2 :`)
	r := asRule(t, f.Directives[0])
	if len(r.Groups) != 2 {
		t.Fatalf("Groups len: got %d, want 2", len(r.Groups))
	}
	if r.Groups[0] != "g1" || r.Groups[1] != "g2" {
		t.Errorf("Groups: got %v, want [g1 g2]", r.Groups)
	}
}

func TestIntoClauseWithForClauses(t *testing.T) {
	f := mustParse(t, `build for os in [linux macos] into mygroup { :; }`)
	r := asRule(t, f.Directives[0])
	if len(r.ForClauses) != 1 || r.ForClauses[0].Var != "os" {
		t.Fatalf("ForClauses: got %v", r.ForClauses)
	}
	if len(r.Groups) != 1 || r.Groups[0] != "mygroup" {
		t.Errorf("Groups: got %v, want [mygroup]", r.Groups)
	}
}

func TestIntoClauseWithBody(t *testing.T) {
	f := mustParse(t, `mytarget into mygroup { echo hi }`)
	r := asRule(t, f.Directives[0])
	if len(r.Groups) != 1 || r.Groups[0] != "mygroup" {
		t.Errorf("Groups: got %v, want [mygroup]", r.Groups)
	}
	if r.Body == "" {
		t.Error("expected non-empty body")
	}
}

// --- group projection deps ---

func TestGroupProjectionDepSingleDim(t *testing.T) {
	f := mustParse(t, `foo : [mygroup @ os]`)
	r := asRule(t, f.Directives[0])
	if len(r.Deps) != 1 {
		t.Fatalf("Deps len: got %d, want 1", len(r.Deps))
	}
	d := r.Deps[0]
	expect(t, "Target", d.Target, "mygroup")
	if len(d.Combo) != 0 {
		t.Errorf("Combo should be empty, got %v", d.Combo)
	}
	if len(d.GroupDims) != 1 || d.GroupDims[0] != "os" {
		t.Errorf("GroupDims: got %v, want [os]", d.GroupDims)
	}
}

func TestGroupProjectionDepMultipleDims(t *testing.T) {
	f := mustParse(t, `foo : [mygroup @ os arch]`)
	r := asRule(t, f.Directives[0])
	if len(r.Deps) != 1 {
		t.Fatalf("Deps len: got %d, want 1", len(r.Deps))
	}
	d := r.Deps[0]
	expect(t, "Target", d.Target, "mygroup")
	if len(d.GroupDims) != 2 || d.GroupDims[0] != "os" || d.GroupDims[1] != "arch" {
		t.Errorf("GroupDims: got %v, want [os arch]", d.GroupDims)
	}
	if len(d.Combo) != 0 {
		t.Errorf("Combo should be empty for group projection dep, got %v", d.Combo)
	}
}

func TestGroupProjectionDepMixedWithPlainDeps(t *testing.T) {
	f := mustParse(t, `foo : plain [mygroup @ os] other`)
	r := asRule(t, f.Directives[0])
	if len(r.Deps) != 3 {
		t.Fatalf("Deps len: got %d, want 3", len(r.Deps))
	}
	if r.Deps[0].Target != "plain" || len(r.Deps[0].GroupDims) != 0 {
		t.Errorf("Deps[0]: got %+v", r.Deps[0])
	}
	if r.Deps[1].Target != "mygroup" || len(r.Deps[1].GroupDims) != 1 || r.Deps[1].GroupDims[0] != "os" {
		t.Errorf("Deps[1]: got %+v", r.Deps[1])
	}
	if r.Deps[2].Target != "other" || len(r.Deps[2].GroupDims) != 0 {
		t.Errorf("Deps[2]: got %+v", r.Deps[2])
	}
}

func TestIntoClauseWithForAndOn(t *testing.T) {
	f := mustParse(t, `build for os in [linux macos] into mygroup on runner-$os { :; }`)
	r := asRule(t, f.Directives[0])
	expect(t, "Target", r.Target, "build")
	expect(t, "Runner", r.Runner, "runner-$os")
	if len(r.ForClauses) != 1 || r.ForClauses[0].Var != "os" {
		t.Fatalf("ForClauses: got %v", r.ForClauses)
	}
	if len(r.Groups) != 1 || r.Groups[0] != "mygroup" {
		t.Errorf("Groups: got %v, want [mygroup]", r.Groups)
	}
}

func TestGroupDirectiveMissingNameError(t *testing.T) {
	expectError(t, "group\n", "")
}

func TestIntoMissingNameError(t *testing.T) {
	expectError(t, `mytarget into :`, "")
}

func TestGroupBracketFlatDepNotProjection(t *testing.T) {
	// [groupname] with no @ is a plain bracket dep, not a group projection.
	f := mustParse(t, `foo : [mygroup]`)
	r := asRule(t, f.Directives[0])
	if len(r.Deps) != 1 {
		t.Fatalf("Deps len: got %d, want 1", len(r.Deps))
	}
	d := r.Deps[0]
	expect(t, "Target", d.Target, "mygroup")
	if len(d.GroupDims) != 0 {
		t.Errorf("GroupDims should be empty for plain bracket dep, got %v", d.GroupDims)
	}
	if len(d.Combo) != 0 {
		t.Errorf("Combo should be empty, got %v", d.Combo)
	}
}

func TestGroupProjectionDepMultipleGroups(t *testing.T) {
	// Two group projection deps in the same dep list.
	f := mustParse(t, `foo : [g1 @ input] [g2 @ somevar]`)
	r := asRule(t, f.Directives[0])
	if len(r.Deps) != 2 {
		t.Fatalf("Deps len: got %d, want 2", len(r.Deps))
	}
	if r.Deps[0].Target != "g1" || len(r.Deps[0].GroupDims) != 1 || r.Deps[0].GroupDims[0] != "input" {
		t.Errorf("Deps[0]: got %+v", r.Deps[0])
	}
	if r.Deps[1].Target != "g2" || len(r.Deps[1].GroupDims) != 1 || r.Deps[1].GroupDims[0] != "somevar" {
		t.Errorf("Deps[1]: got %+v", r.Deps[1])
	}
}

// Existing combo deps must still work (k=v form, not bare dim form).
func TestComboDepStillWorksAfterGroupProjectionParsing(t *testing.T) {
	f := mustParse(t, `foo : [build @ os=linux go=1.20]`)
	r := asRule(t, f.Directives[0])
	d := r.Deps[0]
	if d.Target != "build" {
		t.Errorf("Target: got %q, want %q", d.Target, "build")
	}
	if len(d.GroupDims) != 0 {
		t.Errorf("GroupDims should be empty for k=v dep, got %v", d.GroupDims)
	}
	if len(d.Combo) != 2 {
		t.Fatalf("Combo len: got %d, want 2", len(d.Combo))
	}
}

// --- helpers ---

func mustParse(t *testing.T, src string) *File {
	t.Helper()
	f, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	return f
}

func expectError(t *testing.T, src string, contains string) string {
	t.Helper()
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatalf("expected parse error for input %q", src)
	}
	msg := err.Error()
	if contains != "" {
		assertContains(t, msg, contains)
	}
	return msg
}

func assertContains(t *testing.T, s, substr string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Errorf("expected %q to contain %q", s, substr)
	}
}

func requireRules(t *testing.T, f *File, n int) {
	t.Helper()
	if len(f.Directives) != n {
		t.Fatalf("expected %d directive(s), got %d", n, len(f.Directives))
	}
}

func asRule(t *testing.T, d Directive) *TargetRule {
	t.Helper()
	r, ok := d.(*TargetRule)
	if !ok {
		t.Fatalf("expected *TargetRule, got %T", d)
	}
	return r
}

func expect(t *testing.T, field, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %q, want %q", field, got, want)
	}
}

func expectDeps(t *testing.T, got []Dep, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("deps: got %v, want %v", got, want)
		return
	}
	for i := range want {
		if got[i].Target != want[i] {
			t.Errorf("deps[%d].Target: got %q, want %q", i, got[i].Target, want[i])
		}
	}
}

