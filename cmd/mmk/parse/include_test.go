package parse

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- include parsing (AST-only, no resolution) ---

func TestIncludeBareWord(t *testing.T) {
	f := mustParse(t, `include lib/build.mmk`)
	if len(f.Directives) != 1 {
		t.Fatalf("expected 1 directive, got %d", len(f.Directives))
	}
	inc, ok := f.Directives[0].(*Include)
	if !ok {
		t.Fatalf("expected *Include, got %T", f.Directives[0])
	}
	expect(t, "Path", inc.Path, "lib/build.mmk")
}

func TestIncludeQuotedString(t *testing.T) {
	f := mustParse(t, `include "lib/has spaces.mmk"`)
	inc, ok := f.Directives[0].(*Include)
	if !ok {
		t.Fatalf("expected *Include, got %T", f.Directives[0])
	}
	expect(t, "Path", inc.Path, "lib/has spaces.mmk")
}

func TestIncludeWithVariable(t *testing.T) {
	f := mustParse(t, `include $LIBDIR/build.mmk`)
	inc, ok := f.Directives[0].(*Include)
	if !ok {
		t.Fatalf("expected *Include, got %T", f.Directives[0])
	}
	expect(t, "Path", inc.Path, "$LIBDIR/build.mmk")
}

func TestIncludeMultipleDirectives(t *testing.T) {
	f := mustParse(t, "include a.mmk\ninclude b.mmk\ninclude c.mmk\n")
	if len(f.Directives) != 3 {
		t.Fatalf("expected 3 directives, got %d", len(f.Directives))
	}
	wants := []string{"a.mmk", "b.mmk", "c.mmk"}
	for i, want := range wants {
		inc, ok := f.Directives[i].(*Include)
		if !ok {
			t.Fatalf("directive %d: expected *Include, got %T", i, f.Directives[i])
		}
		if inc.Path != want {
			t.Errorf("directive %d Path: got %q, want %q", i, inc.Path, want)
		}
	}
}

func TestIncludeMissingPathError(t *testing.T) {
	expectError(t, "include\n", "expected path")
}

func TestIncludeRejectsSingleQuote(t *testing.T) {
	expectError(t, `include 'lib.mmk'`, "single-quoted")
}

func TestIncludeRejectsTrailingTokens(t *testing.T) {
	expectError(t, `include lib.mmk extra`, "unexpected token")
}

// --- include resolution (file-based) ---

// writeFile is a tiny helper to set up include-resolution test fixtures.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return full
}

// directiveTargets returns the concrete target names from f's TargetRules.
func directiveTargets(f *File) []string {
	var out []string
	for _, d := range f.Directives {
		if r, ok := d.(*TargetRule); ok && r.Target != "" {
			out = append(out, r.Target)
		}
	}
	return out
}

func TestParseFile_NoIncludes(t *testing.T) {
	dir := t.TempDir()
	root := writeFile(t, dir, "mmkfile", "all : foo\nfoo {\n echo hi\n}\n")
	f, err := ParseFile(root)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	got := directiveTargets(f)
	if len(got) != 2 || got[0] != "all" || got[1] != "foo" {
		t.Errorf("targets: got %v, want [all foo]", got)
	}
}

func TestParseFile_SimpleInclude(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "lib/build.mmk", "build : { echo build; }\n")
	root := writeFile(t, dir, "mmkfile", "include lib/build.mmk\nall : build\n")
	f, err := ParseFile(root)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	got := directiveTargets(f)
	if len(got) != 2 || got[0] != "build" || got[1] != "all" {
		t.Errorf("targets: got %v, want [build all]", got)
	}
}

func TestParseFile_QuotedPath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "has spaces.mmk", "task : { :; }\n")
	root := writeFile(t, dir, "mmkfile", `include "has spaces.mmk"`+"\n")
	f, err := ParseFile(root)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if got := directiveTargets(f); len(got) != 1 || got[0] != "task" {
		t.Errorf("targets: got %v, want [task]", got)
	}
}

func TestParseFile_RelativePathFromIncludingFile(t *testing.T) {
	// lib/a.mmk does `include b.mmk`; b.mmk lives next to a.mmk in lib/, NOT
	// at the parent's working dir. Path resolution must use the including
	// file's directory.
	dir := t.TempDir()
	writeFile(t, dir, "lib/a.mmk", "include b.mmk\nfrom_a : { :; }\n")
	writeFile(t, dir, "lib/b.mmk", "from_b : { :; }\n")
	root := writeFile(t, dir, "mmkfile", "include lib/a.mmk\n")
	f, err := ParseFile(root)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	got := directiveTargets(f)
	wantAny := map[string]bool{"from_a": false, "from_b": false}
	for _, n := range got {
		if _, ok := wantAny[n]; ok {
			wantAny[n] = true
		}
	}
	for k, v := range wantAny {
		if !v {
			t.Errorf("expected target %q from transitively-included file; got %v", k, got)
		}
	}
}

func TestParseFile_VariableExpansionInPath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "lib/build.mmk", "from_lib : { :; }\n")
	root := writeFile(t, dir, "mmkfile", "LIBDIR=lib\ninclude $LIBDIR/build.mmk\n")
	f, err := ParseFile(root)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if got := directiveTargets(f); len(got) != 1 || got[0] != "from_lib" {
		t.Errorf("targets: got %v, want [from_lib]", got)
	}
}

func TestParseFile_DuplicateIncludeIsNoop(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "lib.mmk", "shared : { :; }\n")
	root := writeFile(t, dir, "mmkfile", "include lib.mmk\ninclude lib.mmk\nrest : shared\n")
	f, err := ParseFile(root)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	got := directiveTargets(f)
	count := 0
	for _, n := range got {
		if n == "shared" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 'shared' to appear once after duplicate include; got %d times: %v", count, got)
	}
}

func TestParseFile_CycleHandled(t *testing.T) {
	// a includes b includes a — ParseFile must not loop forever.
	dir := t.TempDir()
	writeFile(t, dir, "a.mmk", "include b.mmk\nfrom_a : { :; }\n")
	writeFile(t, dir, "b.mmk", "include a.mmk\nfrom_b : { :; }\n")
	root := writeFile(t, dir, "mmkfile", "include a.mmk\n")
	f, err := ParseFile(root)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	// Both a's and b's contents should appear once each.
	got := directiveTargets(f)
	want := map[string]int{"from_a": 1, "from_b": 1}
	have := map[string]int{}
	for _, n := range got {
		have[n]++
	}
	for k, v := range want {
		if have[k] != v {
			t.Errorf("target %q: got %d occurrence(s), want %d (full: %v)", k, have[k], v, got)
		}
	}
}

func TestParseFile_MissingFileError(t *testing.T) {
	dir := t.TempDir()
	root := writeFile(t, dir, "mmkfile", "include not-there.mmk\n")
	_, err := ParseFile(root)
	if err == nil {
		t.Fatal("expected error for missing included file")
	}
	if !strings.Contains(err.Error(), "not-there.mmk") {
		t.Errorf("error should mention the missing path; got: %v", err)
	}
}

func TestParseFile_MultiWordExpansionError(t *testing.T) {
	dir := t.TempDir()
	root := writeFile(t, dir, "mmkfile", `MULTI="a b"`+"\ninclude $MULTI\n")
	_, err := ParseFile(root)
	if err == nil {
		t.Fatal("expected error when include path expands to multiple words")
	}
	if !strings.Contains(err.Error(), "must be exactly one") {
		t.Errorf("error should say 'must be exactly one'; got: %v", err)
	}
}

func TestParseFile_PassthroughVisibleAcrossIncludes(t *testing.T) {
	// Variables defined in an earlier-included file should be visible in
	// later includes' paths. Lexical splice semantics: by the time we're
	// resolving the second include, the first include's passthroughs have
	// been spliced in above.
	dir := t.TempDir()
	writeFile(t, dir, "vars.mmk", "LIBDIR=lib\n")
	writeFile(t, dir, "lib/build.mmk", "from_lib : { :; }\n")
	root := writeFile(t, dir, "mmkfile", "include vars.mmk\ninclude $LIBDIR/build.mmk\n")
	f, err := ParseFile(root)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if got := directiveTargets(f); len(got) != 1 || got[0] != "from_lib" {
		t.Errorf("targets: got %v, want [from_lib]", got)
	}
}

// --- include search path (MMK_LIB_PATH) ---

func TestParseFile_LibSearchPath_FindsBareInclude(t *testing.T) {
	dir := t.TempDir()
	libDir := t.TempDir()
	writeFile(t, libDir, "go.mmk", "from_stdlib : { :; }\n")
	root := writeFile(t, dir, "mmkfile", "include go.mmk\n")

	t.Setenv("MMK_LIB_PATH", libDir)
	f, err := ParseFile(root)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if got := directiveTargets(f); len(got) != 1 || got[0] != "from_stdlib" {
		t.Errorf("targets: got %v, want [from_stdlib]", got)
	}
}

func TestParseFile_LibSearchPath_LocalShadowsStdlib(t *testing.T) {
	// If both a local file and a stdlib file with the same name exist, the
	// local one wins (relative resolution is tried first).
	dir := t.TempDir()
	libDir := t.TempDir()
	writeFile(t, libDir, "go.mmk", "from_stdlib : { :; }\n")
	writeFile(t, dir, "go.mmk", "from_local : { :; }\n")
	root := writeFile(t, dir, "mmkfile", "include go.mmk\n")

	t.Setenv("MMK_LIB_PATH", libDir)
	f, err := ParseFile(root)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if got := directiveTargets(f); len(got) != 1 || got[0] != "from_local" {
		t.Errorf("local should shadow stdlib; got %v, want [from_local]", got)
	}
}

func TestParseFile_LibSearchPath_MultipleDirsSearchedInOrder(t *testing.T) {
	dir := t.TempDir()
	libA := t.TempDir()
	libB := t.TempDir()
	writeFile(t, libA, "lib.mmk", "from_a : { :; }\n")
	writeFile(t, libB, "lib.mmk", "from_b : { :; }\n")
	root := writeFile(t, dir, "mmkfile", "include lib.mmk\n")

	t.Setenv("MMK_LIB_PATH", libA+":"+libB)
	f, err := ParseFile(root)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if got := directiveTargets(f); len(got) != 1 || got[0] != "from_a" {
		t.Errorf("first dir should win; got %v, want [from_a]", got)
	}
}

func TestParseFile_LibSearchPath_SubdirInLibDir(t *testing.T) {
	// `include lang/c.mmk` should search MMK_LIB_PATH for lang/c.mmk.
	dir := t.TempDir()
	libDir := t.TempDir()
	writeFile(t, libDir, "lang/c.mmk", "from_c_stdlib : { :; }\n")
	root := writeFile(t, dir, "mmkfile", "include lang/c.mmk\n")

	t.Setenv("MMK_LIB_PATH", libDir)
	f, err := ParseFile(root)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if got := directiveTargets(f); len(got) != 1 || got[0] != "from_c_stdlib" {
		t.Errorf("targets: got %v, want [from_c_stdlib]", got)
	}
}

func TestParseFile_TransitivePassthroughVisible(t *testing.T) {
	// vars.mmk is included from sub.mmk, which is included from the root.
	// A sibling include (still in the root) using $LIBDIR should resolve
	// because vars.mmk's passthroughs are spliced in above it.
	dir := t.TempDir()
	writeFile(t, dir, "vars.mmk", "LIBDIR=lib\n")
	writeFile(t, dir, "sub.mmk", "include vars.mmk\nfrom_sub : { :; }\n")
	writeFile(t, dir, "lib/build.mmk", "from_lib : { :; }\n")
	root := writeFile(t, dir, "mmkfile", "include sub.mmk\ninclude $LIBDIR/build.mmk\n")
	f, err := ParseFile(root)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	got := directiveTargets(f)
	for _, want := range []string{"from_sub", "from_lib"} {
		found := false
		for _, n := range got {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected target %q in result; got %v", want, got)
		}
	}
}
