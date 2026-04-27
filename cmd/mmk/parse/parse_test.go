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

