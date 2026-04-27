package runtime

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/knusbaum/mmk3/cmd/mmk/parse"
)

const (
	ptVarsSentinel  = "### MMK_PT VARS ###"
	ptFuncsSentinel = "### MMK_PT FUNCS ###"
	ptFuncEnd       = "### MMK_PT ENDF ###"
)

// evalPassthroughs runs all passthrough directives in f once in a bash
// subprocess and returns frozen replacement lines: pre-evaluated variable
// assignments and function definitions. These replace all passthrough lines
// in the generated script so they execute exactly once at build time rather
// than once per task. Returns nil if there are no passthrough directives.
func evalPassthroughs(f *parse.File) ([]string, error) {
	var ptLines []string
	for _, d := range f.Directives {
		if p, ok := d.(*parse.Passthrough); ok {
			ptLines = append(ptLines, p.Line)
		}
	}
	if len(ptLines) == 0 {
		return nil, nil
	}
	out, err := exec.Command("bash", "-c", buildEvalScript(ptLines)).Output()
	if err != nil {
		return nil, fmt.Errorf("evaluate passthroughs: %w", err)
	}
	return parseEvalOutput(string(out))
}

func buildEvalScript(ptLines []string) string {
	var sb strings.Builder
	// Pre-declare sentinels so they appear in the baseline snapshot and are
	// excluded from the new-variable diff.
	sb.WriteString("__mmk_bv='' __mmk_bf=''\n")
	sb.WriteString("__mmk_bv=$(compgen -v | sort)\n")
	sb.WriteString("__mmk_bf=$(compgen -A function | sort)\n")
	for _, line := range ptLines {
		sb.WriteString(line + "\n")
	}
	fmt.Fprintf(&sb, "printf '%s\\n'\n", ptVarsSentinel)
	sb.WriteString("while IFS= read -r __mmk_v; do\n")
	sb.WriteString("    printf '%s=%q\\n' \"$__mmk_v\" \"${!__mmk_v}\"\n")
	sb.WriteString("done < <(comm -13 <(echo \"$__mmk_bv\") <(compgen -v | sort) | grep -v '^__mmk_')\n")
	fmt.Fprintf(&sb, "printf '%s\\n'\n", ptFuncsSentinel)
	sb.WriteString("while IFS= read -r __mmk_f; do\n")
	sb.WriteString("    declare -f \"$__mmk_f\"\n")
	fmt.Fprintf(&sb, "    printf '%s\\n'\n", ptFuncEnd)
	sb.WriteString("done < <(comm -13 <(echo \"$__mmk_bf\") <(compgen -A function | sort) | grep -v '^__mmk_')\n")
	return sb.String()
}

func parseEvalOutput(output string) ([]string, error) {
	varsStart := strings.Index(output, ptVarsSentinel+"\n")
	funcsStart := strings.Index(output, ptFuncsSentinel+"\n")
	if varsStart < 0 || funcsStart < 0 {
		return nil, fmt.Errorf("evalPassthroughs: unexpected output")
	}
	varSection := output[varsStart+len(ptVarsSentinel)+1 : funcsStart]
	funcSection := output[funcsStart+len(ptFuncsSentinel)+1:]

	var frozen []string
	for _, line := range strings.Split(strings.TrimRight(varSection, "\n"), "\n") {
		if line != "" {
			frozen = append(frozen, line)
		}
	}
	for _, block := range strings.Split(funcSection, ptFuncEnd+"\n") {
		if block = strings.TrimRight(block, "\n"); block != "" {
			frozen = append(frozen, block)
		}
	}
	return frozen, nil
}
