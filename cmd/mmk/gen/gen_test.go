package gen

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knusbaum/mmk3/cmd/mmk/parse"
)

// bashValid checks the generated script with bash -n (syntax check only, no execution).
func bashValid(t *testing.T, src string) {
	t.Helper()
	f, err := os.CreateTemp("", "mmk-gen-*.sh")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(src); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	f.Close()

	out, err := exec.Command("bash", "-n", filepath.Clean(f.Name())).CombinedOutput()
	if err != nil {
		t.Errorf("bash syntax check failed:\n%s\nscript:\n%s", out, src)
	}
}

func generate(t *testing.T, src string) string {
	t.Helper()
	f, err := parse.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var sb strings.Builder
	if err := Generate(&sb, f, nil); err != nil {
		t.Fatalf("generate: %v", err)
	}
	return sb.String()
}

func TestValidateName(t *testing.T) {
	valid := []string{"foo", "main.o", "out/main.o", "ubuntu:latest", "a-b", "_foo", "a_b", "123"}
	for _, name := range valid {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) unexpected error: %v", name, err)
		}
	}

	invalid := []string{"", "foo bar", "foo$bar", "foo(bar", "foo)bar", "foo<bar", "foo>bar", "foo`bar", `foo"bar`, "foo'bar", `foo\bar`, "foo[bar", "foo=bar"}
	for _, name := range invalid {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) expected error, got nil", name)
		}
	}
}

func TestTargetWithBody(t *testing.T) {
	out := generate(t, `file main.o : main.c {
	cc -c main.c -o main.o
}`)
	assertContains(t, out, "__mmk_target_main.o()")
	assertContains(t, out, "cc -c main.c -o main.o")
	assertContains(t, out, "# target: main.o (type: file)")
	bashValid(t, out)
}

func TestTargetNoBody(t *testing.T) {
	out := generate(t, `all : foo bar`)
	assertContains(t, out, "__mmk_target_all()")
	// No-op body should be valid bash.
	assertContains(t, out, ":")
	bashValid(t, out)
}

func TestDefType(t *testing.T) {
	out := generate(t, `deftype file {
	[[ -f "$target" ]] || return 1
}`)
	assertContains(t, out, "__mmk_type_file()")
	assertContains(t, out, "# deftype file")
	bashValid(t, out)
}

func TestDefRunner(t *testing.T) {
	out := generate(t, `defrunner ubuntu {
	docker run --rm ubuntu:latest "$@"
}`)
	assertContains(t, out, "__mmk_runner_run_ubuntu()")
	assertContains(t, out, "# defrunner ubuntu run")
	bashValid(t, out)
}

func TestDefRunnerSetup(t *testing.T) {
	out := generate(t, `defrunner ubuntu setup { echo setup }`)
	assertContains(t, out, "__mmk_runner_setup_ubuntu()")
	assertContains(t, out, "# defrunner ubuntu setup")
	bashValid(t, out)
}

func TestBuiltinImageRunnerFunctions(t *testing.T) {
	// Any mmkfile that uses image type should get the built-in image runner functions.
	out := generate(t, `image buildimage:latest : Dockerfile`)
	assertContains(t, out, "__mmk_runner_setup_image()")
	assertContains(t, out, "__mmk_runner_run_image()")
	assertContains(t, out, "__mmk_runner_cleanup_image()")
	bashValid(t, out)
}

func TestFullFile(t *testing.T) {
	src := `
deftype file {
	[[ -f "$target" ]] || return 1
	for dep in $deps; do
		[[ "$dep" -nt "$target" ]] && return 1
	done
}

defrunner ubuntu {
	docker run --rm -v "$PWD:/work" -w /work ubuntu:latest "$@"
}

file main.o on ubuntu : main.c lib.h {
	cc -c main.c -o main.o
}

all : main.o
`
	out := generate(t, src)
	assertContains(t, out, "__mmk_type_file()")
	assertContains(t, out, "__mmk_runner_run_ubuntu()")
	assertContains(t, out, "__mmk_target_main.o()")
	assertContains(t, out, "__mmk_target_all()")
	bashValid(t, out)
}

func TestBodyWithNestedBraces(t *testing.T) {
	out := generate(t, `build {
	setup() { mkdir -p out; }
	setup
}`)
	assertContains(t, out, "__mmk_target_build()")
	bashValid(t, out)
}

func TestQuotedTargetName(t *testing.T) {
	out := generate(t, `"ubuntu:latest" {
	docker pull ubuntu:latest
}`)
	assertContains(t, out, "__mmk_target_ubuntu:latest()")
	bashValid(t, out)
}

func TestPassthroughEmittedVerbatim(t *testing.T) {
	out := generate(t, `
OBJECTS=main

file main.o : main.c {
	cc -c main.c -o main.o
}
`)
	assertContains(t, out, "OBJECTS=main")
	assertContains(t, out, "__mmk_target_main.o()")
	bashValid(t, out)
}

func TestBuiltinImageDefaultBody(t *testing.T) {
	// image target with no body should call __mmk_default_image.
	out := generate(t, `image buildimage:latest : Dockerfile`)
	assertContains(t, out, DefaultFunc("image")+"()")
	assertContains(t, out, "__mmk_target_buildimage:latest")
	assertContains(t, out, DefaultFunc("image"))
	bashValid(t, out)
}

func TestUserDefBodyOverridesBuiltin(t *testing.T) {
	out := generate(t, `defbody image {
	docker build --no-cache -t "$target" .
}
image myimg : Dockerfile`)
	// built-in image default must not appear; user defbody must.
	if strings.Contains(out, "built-in default body: image") {
		t.Error("built-in image default should be suppressed when user defbody image is present")
	}
	assertContains(t, out, "defbody image")
	assertContains(t, out, "--no-cache")
	bashValid(t, out)
}

func TestExplicitBodyOverridesDefault(t *testing.T) {
	// An explicit body on the target takes precedence over defbody.
	// The default function is still defined but not called by this target.
	out := generate(t, `image myimg : Dockerfile {
	docker build --squash -t "$target" .
}`)
	assertContains(t, out, "--squash")
	// The target function itself should contain the explicit body, not a call to the default.
	// Verify by checking --squash appears and the target function does not delegate to default.
	targetFn := "__mmk_target_myimg"
	targetIdx := strings.Index(out, targetFn+"()")
	if targetIdx < 0 {
		t.Fatal("target function not found in output")
	}
	targetBody := out[targetIdx:]
	if strings.Contains(targetBody[:strings.Index(targetBody, "}")+1], DefaultFunc("image")) {
		t.Error("target function should not call default when it has an explicit body")
	}
	bashValid(t, out)
}

func TestUserDefBody(t *testing.T) {
	out := generate(t, `deftype mytype {
	echo 0
}
defbody mytype {
	run-mytype $target
}
mytype mytarget : dep`)
	assertContains(t, out, DefaultFunc("mytype")+"()")
	assertContains(t, out, "run-mytype")
	assertContains(t, out, DefaultFunc("mytype"))
	bashValid(t, out)
}

func TestSingleLineBodyIsValidBash(t *testing.T) {
	// Body on the same line as '{' — must end with '\n' so bash accepts '}'.
	out := generate(t, `all : foo { true }`)
	bashValid(t, out)
}

func TestPassthroughBashFunctionIsValidBash(t *testing.T) {
	out := generate(t, `helper() {
	echo hello
}
all : { helper }`)
	assertContains(t, out, "helper()")
	assertContains(t, out, "__mmk_target_all()")
	bashValid(t, out)
}

func TestValidateDuplicates(t *testing.T) {
	f, _ := parse.Parse([]byte("foo {}\nfoo {}"))
	if err := ValidateDuplicates(f); err == nil {
		t.Fatal("expected duplicate error")
	}
}

func TestValidateNoDuplicates(t *testing.T) {
	f, _ := parse.Parse([]byte("foo {}\nbar {}"))
	if err := ValidateDuplicates(f); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuiltinFileCleanVerbBody(t *testing.T) {
	out := generate(t, `file main.o : main.c { cc -c main.c -o main.o }`)
	assertContains(t, out, DefaultVerbFunc("file", "clean")+"()")
	assertContains(t, out, "rm -f")
	bashValid(t, out)
}

func TestUserDefbodyFileCleanOverridesBuiltin(t *testing.T) {
	out := generate(t, `defbody file clean { : }
file main.o : main.c { cc -c main.c -o main.o }`)
	if strings.Contains(out, "built-in defbody file clean") {
		t.Error("built-in file clean should be suppressed when user defbody file clean is present")
	}
	bashValid(t, out)
}

func TestSourceTypeHasNoCleanVerbBody(t *testing.T) {
	// source type should not have a clean verb body emitted.
	out := generate(t, `all : foo`)
	if strings.Contains(out, DefaultVerbFunc("source", "clean")) {
		t.Error("source type should not have a built-in clean verb body")
	}
	bashValid(t, out)
}

// --- helpers ---

func assertContains(t *testing.T, s, substr string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Errorf("output does not contain %q\nfull output:\n%s", substr, s)
	}
}
