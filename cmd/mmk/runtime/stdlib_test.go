package runtime

// Integration tests for the bundled stdlib mmkfiles in <repo>/lib/.
// These tests load each stdlib file via MMK_LIB_PATH and exercise its
// types against a fixture project, checking observable outcomes (files
// created, exit codes, content of outputs).

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// libDir returns the absolute path to <repo>/lib, derived from this test
// file's location at compile time. This avoids assumptions about the
// process's working directory.
func libDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// file = <repo>/cmd/mmk/runtime/stdlib_test.go
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "lib")
}

// withStdlib sets MMK_LIB_PATH to the repo's lib/ directory for the test
// duration and returns to the prior value on cleanup.
func withStdlib(t *testing.T) {
	t.Helper()
	t.Setenv("MMK_LIB_PATH", libDir(t))
}

// --- go.mmk -----------------------------------------------------------------

// makeGoFixture creates a minimal Go module with a single command package.
// Returns the fixture root.
func makeGoFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustWrite(t, dir, "go.mod", "module example.com/test\n\ngo 1.21\n")
	mustWrite(t, dir, "cmd/hello/main.go", "package main\n\nfunc main() {}\n")
	mustWrite(t, dir, "main.go", "package test\n")
	return dir
}

func mustWrite(t *testing.T, dir, name, content string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newFileBuild loads the mmkfile at <dir>/mmkfile through include resolution
// (so `include go.mmk` etc. work via MMK_LIB_PATH) and registers cleanup.
func newFileBuild(t *testing.T, dir string) *Build {
	t.Helper()
	b, err := NewBuildFromFile(filepath.Join(dir, "mmkfile"))
	if err != nil {
		t.Fatalf("NewBuildFromFile: %v", err)
	}
	t.Cleanup(b.Close)
	return b
}

func TestStdlib_GoExe_Builds(t *testing.T) {
	withStdlib(t)
	dir := makeGoFixture(t)
	t.Chdir(dir)
	mustWrite(t, dir, "mmkfile", `include go.mmk

go_exe bin/hello pkg=./cmd/hello :
`)
	b := newFileBuild(t, dir)
	n, err := b.Resolve("bin/hello")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := runForTest(n); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bin/hello")); err != nil {
		t.Errorf("expected bin/hello to be built: %v", err)
	}
}

func TestStdlib_GoExe_AlwaysNeedsRun(t *testing.T) {
	// go_exe defers to the go tool's own incremental cache, so mmk's
	// freshness check should always report NeedsRun=true — even after a
	// successful build — rather than tracking the binary's mtime.
	withStdlib(t)
	dir := makeGoFixture(t)
	t.Chdir(dir)
	mustWrite(t, dir, "mmkfile", `include go.mmk

go_exe bin/hello pkg=./cmd/hello :
`)
	b := newFileBuild(t, dir)
	n, _ := b.Resolve("bin/hello")
	if err := runForTest(n); err != nil {
		t.Fatalf("first build: %v", err)
	}
	// Re-load to drop cached state.
	b2 := newFileBuild(t, dir)
	n2, _ := b2.Resolve("bin/hello")
	needs, err := n2.NeedsRun()
	if err != nil {
		t.Fatalf("NeedsRun: %v", err)
	}
	if !needs {
		t.Errorf("after build, go_exe should still report NeedsRun=true (defers to `go build`'s own cache)")
	}
}

func TestStdlib_GoExe_Clean(t *testing.T) {
	withStdlib(t)
	dir := makeGoFixture(t)
	t.Chdir(dir)
	mustWrite(t, dir, "mmkfile", `include go.mmk

go_exe bin/hello pkg=./cmd/hello :
`)
	b := newFileBuild(t, dir)
	n, _ := b.Resolve("bin/hello")
	if err := runForTest(n); err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bin/hello")); err != nil {
		t.Fatalf("binary missing before clean: %v", err)
	}
	cleanNode, err := b.ResolveVerb("bin/hello", "clean")
	if err != nil {
		t.Fatalf("ResolveVerb clean: %v", err)
	}
	if err := runForTest(cleanNode); err != nil {
		t.Fatalf("clean: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bin/hello")); err == nil {
		t.Errorf("expected bin/hello to be removed by clean")
	}
}

func TestStdlib_GoModule_TestVerb(t *testing.T) {
	// `mmk test <module>` should run `go test ./...` successfully.
	withStdlib(t)
	dir := makeGoFixture(t)
	t.Chdir(dir)
	mustWrite(t, dir, "test_test.go", `package test

import "testing"

func TestSmoke(t *testing.T) { _ = 1 }
`)
	mustWrite(t, dir, "mmkfile", `include go.mmk

go_module module :
`)
	b := newFileBuild(t, dir)
	testNode, err := b.ResolveVerb("module", "test")
	if err != nil {
		t.Fatalf("ResolveVerb test: %v", err)
	}
	if err := runForTest(testNode); err != nil {
		t.Fatalf("test verb: %v", err)
	}
}

func TestStdlib_GoModule_FmtCheckFailsOnUnformatted(t *testing.T) {
	withStdlib(t)
	dir := makeGoFixture(t)
	t.Chdir(dir)
	// Write a poorly-formatted Go file.
	mustWrite(t, dir, "bad.go", "package test\n\nfunc Bad(  ) {  }\n")
	mustWrite(t, dir, "mmkfile", `include go.mmk

go_module module :
`)
	b := newFileBuild(t, dir)
	n, err := b.ResolveVerb("module", "fmt-check")
	if err != nil {
		t.Fatalf("ResolveVerb fmt-check: %v", err)
	}
	err = runForTest(n)
	if err == nil {
		t.Error("expected fmt-check to fail on unformatted file")
	}
}

// --- cmake.mmk --------------------------------------------------------------

// makeCmakeFixture creates a minimal cmake project that doesn't require a
// compiler. The body just writes a sentinel file via cmake's file(WRITE ...)
// at configure time. Returns the fixture root.
func makeCmakeFixture(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("cmake"); err != nil {
		t.Skip("cmake not available")
	}
	dir := t.TempDir()
	mustWrite(t, dir, "CMakeLists.txt", `cmake_minimum_required(VERSION 3.10)
project(smoke NONE)
file(WRITE ${CMAKE_BINARY_DIR}/output.txt "cmake ran\n")
`)
	return dir
}

func TestStdlib_CmakeProject_Builds(t *testing.T) {
	withStdlib(t)
	dir := makeCmakeFixture(t)
	t.Chdir(dir)
	mustWrite(t, dir, "mmkfile", `include cmake.mmk

cmake_project smoke build_dir=build :
`)
	b := newFileBuild(t, dir)
	n, _ := b.Resolve("smoke")
	if err := runForTest(n); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "build/output.txt")); err != nil {
		t.Errorf("expected build/output.txt: %v", err)
	}
}

func TestStdlib_CmakeProject_Clean(t *testing.T) {
	withStdlib(t)
	dir := makeCmakeFixture(t)
	t.Chdir(dir)
	mustWrite(t, dir, "mmkfile", `include cmake.mmk

cmake_project smoke build_dir=build :
`)
	b := newFileBuild(t, dir)
	n, _ := b.Resolve("smoke")
	if err := runForTest(n); err != nil {
		t.Fatalf("build: %v", err)
	}
	cleanNode, _ := b.ResolveVerb("smoke", "clean")
	if err := runForTest(cleanNode); err != nil {
		t.Fatalf("clean: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "build")); err == nil {
		t.Errorf("expected build/ to be removed by clean")
	}
}

// --- git_source -------------------------------------------------------------

// makeGitUpstream creates a local bare-ish git repo with two tags v1 and v2,
// each pointing to a different content of file.txt. Returns the upstream path.
func makeGitUpstream(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	runCmd := func(args ...string) {
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@test",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@test",
		)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runCmd("init", "-b", "main", "-q")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("v1 content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd("add", ".")
	runCmd("commit", "-m", "v1", "-q")
	runCmd("tag", "v1")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("v2 content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd("commit", "-am", "v2", "-q")
	runCmd("tag", "v2")
	return dir
}

func TestStdlib_GitSource_Clones(t *testing.T) {
	withStdlib(t)
	upstream := makeGitUpstream(t)
	dir := t.TempDir()
	t.Chdir(dir)
	mustWrite(t, dir, "mmkfile", fmt.Sprintf(`include cmake.mmk

git_source vendor repo=%q tag=v1 :
`, upstream))
	b := newFileBuild(t, dir)
	n, _ := b.Resolve("vendor")
	if err := runForTest(n); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "vendor/file.txt"))
	if err != nil {
		t.Fatalf("read clone: %v", err)
	}
	if !strings.Contains(string(got), "v1 content") {
		t.Errorf("expected v1 content; got %q", got)
	}
}

func TestStdlib_GitSource_TagSwitchTriggersRebuild(t *testing.T) {
	withStdlib(t)
	upstream := makeGitUpstream(t)
	dir := t.TempDir()
	t.Chdir(dir)
	mustWrite(t, dir, "mmkfile", fmt.Sprintf(`include cmake.mmk

git_source vendor repo=%q tag=v1 :
`, upstream))
	b := newFileBuild(t, dir)
	n, _ := b.Resolve("vendor")
	if err := runForTest(n); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Re-load with tag=v2.
	mustWrite(t, dir, "mmkfile", fmt.Sprintf(`include cmake.mmk

git_source vendor repo=%q tag=v2 :
`, upstream))
	b2 := newFileBuild(t, dir)
	n2, _ := b2.Resolve("vendor")
	needs, err := n2.NeedsRun()
	if err != nil {
		t.Fatalf("NeedsRun: %v", err)
	}
	if !needs {
		t.Fatal("after tag change, git_source should need to re-run")
	}
	if err := runForTest(n2); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "vendor/file.txt"))
	if !strings.Contains(string(got), "v2 content") {
		t.Errorf("expected v2 content after re-checkout; got %q", got)
	}
}

func TestStdlib_GitSource_SameTagSkipsRebuild(t *testing.T) {
	withStdlib(t)
	upstream := makeGitUpstream(t)
	dir := t.TempDir()
	t.Chdir(dir)
	mustWrite(t, dir, "mmkfile", fmt.Sprintf(`include cmake.mmk

git_source vendor repo=%q tag=v1 :
`, upstream))
	b := newFileBuild(t, dir)
	n, _ := b.Resolve("vendor")
	if err := runForTest(n); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Re-load (same mmkfile), should be fresh.
	b2 := newFileBuild(t, dir)
	n2, _ := b2.Resolve("vendor")
	needs, err := n2.NeedsRun()
	if err != nil {
		t.Fatalf("NeedsRun: %v", err)
	}
	if needs {
		t.Errorf("with unchanged tag, git_source should report fresh")
	}
}

// --- c.mmk ------------------------------------------------------------------

// makeCFixture creates a minimal C project with two source files in src/
// and a main.c at the root.
func makeCFixture(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("gcc"); err != nil {
		if _, err := exec.LookPath("cc"); err != nil {
			t.Skip("no C compiler available")
		}
	}
	dir := t.TempDir()
	mustWrite(t, dir, "src/a.c", "int a_func() { return 1; }\n")
	mustWrite(t, dir, "src/b.c", "int b_func() { return 2; }\n")
	mustWrite(t, dir, "main.c", "int main() { return 0; }\n")
	return dir
}

func TestStdlib_CLibrary_BuildsArchiveFromDirectory(t *testing.T) {
	withStdlib(t)
	dir := makeCFixture(t)
	t.Chdir(dir)
	mustWrite(t, dir, "mmkfile", `include c.mmk

c_library libsrc.a source=./src :
`)
	b := newFileBuild(t, dir)
	n, _ := b.Resolve("libsrc.a")
	deps := n.Dependencies()
	// Expect two .o deps from defbody dep clause + ./src/*.c.
	gotDeps := depTargets(deps)
	wantSet := map[string]bool{"./src/a.o": true, "./src/b.o": true}
	for _, d := range gotDeps {
		if !wantSet[d] {
			// allow other infra deps; only enforce the .o set
		}
		delete(wantSet, d)
	}
	if len(wantSet) != 0 {
		t.Errorf("missing expected .o deps: %v (got %v)", wantSet, gotDeps)
	}
	if err := runForTest(n); err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "libsrc.a")); err != nil {
		t.Errorf("libsrc.a not produced: %v", err)
	}
	for _, o := range []string{"src/a.o", "src/b.o"} {
		if _, err := os.Stat(filepath.Join(dir, o)); err != nil {
			t.Errorf("expected %s built by .o pattern rule: %v", o, err)
		}
	}
}

func TestStdlib_CLibrary_CleanRemovesArchiveAndObjects(t *testing.T) {
	withStdlib(t)
	dir := makeCFixture(t)
	t.Chdir(dir)
	mustWrite(t, dir, "mmkfile", `include c.mmk

c_library libsrc.a source=./src :
`)
	b := newFileBuild(t, dir)
	n, _ := b.Resolve("libsrc.a")
	if err := runForTest(n); err != nil {
		t.Fatalf("build: %v", err)
	}
	cleanNode, _ := b.ResolveVerb("libsrc.a", "clean")
	if err := runForTest(cleanNode); err != nil {
		t.Fatalf("clean: %v", err)
	}
	for _, p := range []string{"libsrc.a", "src/a.o", "src/b.o"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err == nil {
			t.Errorf("expected %s removed by clean", p)
		}
	}
}

func TestStdlib_CExecutable_BuildsAndLinks(t *testing.T) {
	withStdlib(t)
	dir := makeCFixture(t)
	t.Chdir(dir)
	mustWrite(t, dir, "mmkfile", `include c.mmk

c_library libsrc.a source=./src :
c_executable myapp : main.o libsrc.a {
    ${CC:-gcc} -o "$target" main.o libsrc.a
}
`)
	b := newFileBuild(t, dir)
	n, _ := b.Resolve("myapp")
	if err := runForTest(n); err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "myapp")); err != nil {
		t.Errorf("myapp not produced: %v", err)
	}
}

func TestStdlib_CLibrary_RecursiveSourceDiscovery(t *testing.T) {
	withStdlib(t)
	dir := makeCFixture(t)
	t.Chdir(dir)
	// Add a nested source file.
	mustWrite(t, dir, "src/sub/c.c", "int c_func() { return 3; }\n")
	mustWrite(t, dir, "mmkfile", `include c.mmk

c_library libsrc.a source=./src recursive=1 :
`)
	b := newFileBuild(t, dir)
	n, _ := b.Resolve("libsrc.a")
	deps := n.Dependencies()
	got := depTargets(deps)
	// Recursive should pick up src/sub/c.o.
	found := false
	for _, d := range got {
		if d == "./src/sub/c.o" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("recursive=1 should discover src/sub/c.o; got %v", got)
	}
}

func TestStdlib_CLibrary_ObjPathPlacesOutputsUnderBuildTree(t *testing.T) {
	// obj_path=build retargets the discovered .o paths into a separate tree
	// so .o files don't litter the source dir. The user's '.o' pattern rule
	// (or a custom one) compiles source.c into build/source.o.
	withStdlib(t)
	dir := makeCFixture(t)
	t.Chdir(dir)
	mustWrite(t, dir, "mmkfile", `# Project's .o pattern targets build/<path>.o ← <path>.c. Declared
# before include c.mmk so it wins over the stdlib's generic '(.*)\.o' rule.
file 'build/(.+)\.o' : $1.c {
    mkdir -p "$(dirname "$target")"
    ${CC:-gcc} -c "$1.c" -o "$target"
}

include c.mmk

c_library build/libsrc.a source=src obj_path=build :
`)
	b := newFileBuild(t, dir)
	n, err := b.Resolve("build/libsrc.a")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got := depTargets(n.Dependencies())
	wantSet := map[string]bool{"build/src/a.o": true, "build/src/b.o": true}
	for _, d := range got {
		delete(wantSet, d)
	}
	if len(wantSet) != 0 {
		t.Errorf("missing expected build-tree .o deps: %v (got %v)", wantSet, got)
	}
	if err := runForTest(n); err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, p := range []string{"build/libsrc.a", "build/src/a.o", "build/src/b.o"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("expected %s built under obj_path tree: %v", p, err)
		}
	}
	// The default sibling-to-source path should NOT exist.
	for _, p := range []string{"src/a.o", "src/b.o"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err == nil {
			t.Errorf("obj_path should redirect output: %s should not exist", p)
		}
	}
}

func TestStdlib_CLibrary_MultiDirSource(t *testing.T) {
	// source="a b" scans multiple directories for .c files. All resulting
	// .o files end up in the same archive.
	withStdlib(t)
	dir := makeCFixture(t)
	t.Chdir(dir)
	mustWrite(t, dir, "extra/x.c", "int x_func() { return 7; }\n")
	mustWrite(t, dir, "mmkfile", `include c.mmk

c_library libcombined.a source="src extra" :
`)
	b := newFileBuild(t, dir)
	n, _ := b.Resolve("libcombined.a")
	got := depTargets(n.Dependencies())
	wantSet := map[string]bool{
		"src/a.o":   true,
		"src/b.o":   true,
		"extra/x.o": true,
	}
	for _, d := range got {
		delete(wantSet, d)
	}
	if len(wantSet) != 0 {
		t.Errorf("multi-dir source= missed: %v (got %v)", wantSet, got)
	}
	if err := runForTest(n); err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "libcombined.a")); err != nil {
		t.Errorf("libcombined.a not produced: %v", err)
	}
}

func TestStdlib_CExecutable_ObjPathAndMultiDir(t *testing.T) {
	// c_executable picks up obj_path and multi-dir source= the same way as
	// c_library — both go through c_objects in c.mmk.
	withStdlib(t)
	dir := makeCFixture(t)
	t.Chdir(dir)
	mustWrite(t, dir, "extra/x.c", "int x_func() { return 7; }\n")
	mustWrite(t, dir, "mmkfile", `file 'build/(.+)\.o' : $1.c {
    mkdir -p "$(dirname "$target")"
    ${CC:-gcc} -c "$1.c" -o "$target"
}

include c.mmk

c_executable build/myapp source="src extra" obj_path=build : build/main.o
`)
	b := newFileBuild(t, dir)
	n, err := b.Resolve("build/myapp")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got := depTargets(n.Dependencies())
	wantSet := map[string]bool{
		"build/src/a.o":   true,
		"build/src/b.o":   true,
		"build/extra/x.o": true,
		"build/main.o":    true,
	}
	for _, d := range got {
		delete(wantSet, d)
	}
	if len(wantSet) != 0 {
		t.Errorf("c_executable missed expected deps: %v (got %v)", wantSet, got)
	}
	if err := runForTest(n); err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "build/myapp")); err != nil {
		t.Errorf("build/myapp not produced: %v", err)
	}
}

