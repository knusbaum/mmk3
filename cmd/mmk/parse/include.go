package parse

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ParseFile reads path, parses it, and recursively resolves any `include`
// directives by splicing the included file's directives in place of the
// directive. Each absolute path is included at most once across the entire
// resolution; subsequent or cyclic includes of the same path are silent
// no-ops. Include paths are resolved relative to the file containing the
// directive.
//
// Variable references inside an include path (`$VAR`, `$(...)`) are
// evaluated by running every passthrough that lexically precedes the
// include — including passthroughs from already-spliced earlier includes —
// in a bash subprocess. The path must expand to exactly one word.
func ParseFile(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{abs: true}
	resolved, _, err := resolveIncludes(f, filepath.Dir(abs), seen, nil)
	if err != nil {
		return nil, err
	}
	return resolved, nil
}

// resolveIncludes returns a new File whose directives are f's with every
// Include splice-replaced by its target file's directives (recursively).
// baseDir is the directory of the file f was parsed from (for relative path
// resolution). seen is mutated to record every absolute path included so
// far. parentPT is the list of passthrough source lines that lexically
// precede this file's directives in the resolution order; included files
// inherit them so $VAR references in their includes can resolve against
// variables defined in earlier files.
//
// Returns the resolved file and the full passthrough list after this file
// (parentPT plus this file's own passthroughs, including transitively
// spliced ones).
func resolveIncludes(f *File, baseDir string, seen map[string]bool, parentPT []string) (*File, []string, error) {
	pt := append([]string{}, parentPT...)
	out := make([]Directive, 0, len(f.Directives))

	for _, d := range f.Directives {
		switch d := d.(type) {
		case *Passthrough:
			pt = append(pt, d.Line)
			out = append(out, d)
		case *Include:
			resolved, err := resolveIncludePath(d.Path, baseDir, pt)
			if err != nil {
				return nil, nil, fmt.Errorf("line %d: include %q: %w", d.Line, d.Path, err)
			}
			abs, err := filepath.Abs(resolved)
			if err != nil {
				return nil, nil, err
			}
			if seen[abs] {
				continue
			}
			seen[abs] = true

			data, err := os.ReadFile(resolved)
			if err != nil {
				return nil, nil, fmt.Errorf("line %d: include %q: %w", d.Line, d.Path, err)
			}
			subFile, err := Parse(data)
			if err != nil {
				return nil, nil, fmt.Errorf("line %d: include %q: %w", d.Line, d.Path, err)
			}
			subResolved, subPT, err := resolveIncludes(subFile, filepath.Dir(abs), seen, pt)
			if err != nil {
				return nil, nil, err
			}
			out = append(out, subResolved.Directives...)
			pt = subPT
		default:
			out = append(out, d)
		}
	}
	return &File{Directives: out}, pt, nil
}

// resolveIncludePath turns a raw include path (which may contain `$VAR`,
// `$(...)`, etc.) into an absolute filesystem path. If the path has no `$`,
// it's used verbatim — no bash needed. Otherwise, accumulated passthrough
// lines are run in a bash subprocess that echoes the path; the resulting
// single word is taken as the resolved path. Relative results are joined
// against baseDir.
func resolveIncludePath(path, baseDir string, passthroughs []string) (string, error) {
	expanded := path
	if strings.Contains(path, "$") {
		var sb strings.Builder
		for _, line := range passthroughs {
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
		// Echo the path so bash performs $-expansion. Word-splitting on the
		// output enforces the "exactly one word" rule.
		fmt.Fprintf(&sb, "echo %s\n", path)
		out, err := exec.Command("bash", "-c", sb.String()).Output()
		if err != nil {
			return "", fmt.Errorf("expand path: %w", err)
		}
		fields := strings.Fields(string(out))
		if len(fields) != 1 {
			return "", fmt.Errorf("expanded to %d words; must be exactly one", len(fields))
		}
		expanded = fields[0]
	}
	if filepath.IsAbs(expanded) {
		return expanded, nil
	}
	return filepath.Join(baseDir, expanded), nil
}
