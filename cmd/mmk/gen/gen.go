// Package gen generates a bash function file from a parsed mmk AST.
// The output is a plain bash script of function definitions that can be
// sourced by per-task invocation scripts.
package gen

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/knusbaum/mmk3/cmd/mmk/parse"
)

// bashInvalid is the set of bytes that bash disallows in function names.
// Determined empirically: everything else (including / . : - @ | & ; etc.) is allowed.
const bashInvalid = "$()<>`\"'\\ \t\n[="

// ValidateName returns an error if name contains any character that bash
// disallows in function names.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("name must not be empty")
	}
	for i, b := range []byte(name) {
		if strings.IndexByte(bashInvalid, b) >= 0 {
			return fmt.Errorf("invalid character %q at position %d in name %q", rune(b), i, name)
		}
	}
	return nil
}

// TargetFunc returns the bash function name for a target.
func TargetFunc(name string) string { return "__mmk_target_" + name }

// TypeFunc returns the bash function name for a deftype.
func TypeFunc(name string) string { return "__mmk_type_" + name }

// RunnerSetupFunc returns the bash function name for the setup phase of a runner type.
func RunnerSetupFunc(name string) string { return "__mmk_runner_setup_" + name }

// RunnerRunFunc returns the bash function name for the run phase of a runner type.
func RunnerRunFunc(name string) string { return "__mmk_runner_run_" + name }

// RunnerCleanupFunc returns the bash function name for the cleanup phase of a runner type.
func RunnerCleanupFunc(name string) string { return "__mmk_runner_cleanup_" + name }

// DefaultFunc returns the bash function name for a type's default body.
func DefaultFunc(typeName string) string { return "__mmk_default_" + typeName }

// VerbTargetFunc returns the bash function name for a verb rule on a target.
func VerbTargetFunc(verb, target string) string { return "__mmk_verb_" + verb + "_target_" + target }

// DefaultVerbFunc returns the bash function name for a type's default verb body.
func DefaultVerbFunc(typeName, verb string) string { return "__mmk_default_" + verb + "_" + typeName }

// BuiltinDefTypes contains the built-in deftype body (bash printing a timestamp)
// for each built-in type. A user deftype with the same name overrides these.
// Note: stat -c %Y is GNU coreutils (Linux); macOS users should override with stat -f %m.
var BuiltinDefTypes = map[string]string{
	"file":   "\n\tstat -c %Y \"$target\" 2>/dev/null || return 1\n",
	"image":  "\n\tdocker inspect --format '{{.Created}}' \"$target\" 2>/dev/null || return 1\n",
	"source": "\n\tstat -c %Y \"$target\" 2>/dev/null || return 1\n",
}

// builtinDefBodies contains the built-in default body for each built-in type.
// A user defbody for the same type name overrides these.
var builtinDefBodies = map[string]string{
	"file":   "\n\t[[ -e \"$target\" ]] && return 0\n\tprintf 'mmk: %s does not exist and has no rule to create it\\n' \"$target\" >&2; return 1\n",
	"image":  "\n\tdocker build -t \"$target\" -f \"${deps%% *}\" .\n",
	"source": "\n\t[[ -e \"$target\" ]] && return 0\n\tprintf 'mmk: %s does not exist and has no rule to create it\\n' \"$target\" >&2; return 1\n",
}

// BuiltinVerbBodies contains the built-in verb body for (type, verb) pairs.
// A user defbody for the same type+verb overrides these.
var BuiltinVerbBodies = map[string]map[string]string{
	"file":  {"clean": "\n\trm -f \"$target\"\n"},
	"image": {"clean": "\n\tdocker image inspect \"$target\" >/dev/null 2>&1 || return 0\n\tdocker image rm -f \"$target\"\n"},
}

// RunnerDefInfo describes which optional phases are defined for a built-in runner type.
// The run phase is always present for any valid runner type.
type RunnerDefInfo struct {
	HasSetup   bool
	HasCleanup bool
}

// BuiltinRunnerDefs maps type names that have built-in runner definitions to
// info about which phases exist. The runtime uses this to know which types are
// valid runners and whether to call setup/cleanup.
var BuiltinRunnerDefs = map[string]RunnerDefInfo{
	"image": {HasSetup: true, HasCleanup: true},
}

type runnerDefBodies struct {
	Setup   string
	Run     string
	Cleanup string
}

// builtinRunnerDefs holds the bash bodies for each built-in runner type.
// These are emitted as __mmk_runner_{setup,run,cleanup}_<type> functions unless
// the user overrides them with their own defrunner directives.
var builtinRunnerDefs = map[string]runnerDefBodies{
	"image": {
		Setup: `
	name="mmk-$(printf '%s' "$target" | tr -cs 'a-zA-Z0-9' '-')-$$"
	docker rm -f "$name" 2>/dev/null || true
	id=$(docker run -d --rm \
		--name "$name" \
		-v "$(pwd):/work" \
		-v "$MMK_GENFILE:/mmk-generated.sh:ro" \
		-w /work \
		"$target" \
		sleep infinity)
	printf '%s' "$id"
`,
		Run: `
	[ -t 0 ] && tty_flag=-t || tty_flag=
	docker exec -i $tty_flag \
		--user "$(id -u):$(id -g)" \
		-e "MMK_TARGET=$MMK_TARGET" \
		-e "MMK_DEPS=$MMK_DEPS" \
		"$MMK_RUNNER_STATE" \
		bash -c ". /mmk-generated.sh; target=\"\$MMK_TARGET\"; deps=\"\$MMK_DEPS\"; $MMK_FUNC"
`,
		Cleanup: `
	docker rm -f "$MMK_RUNNER_STATE" 2>/dev/null || true
`,
	},
}

// builtinRunnerOrder lists built-in runner types in a deterministic emit order.
var builtinRunnerOrder = []string{"image"}

// builtinOrder lists built-in types in a deterministic emit order.
var builtinOrder = []string{"source", "file", "image"}

// Generate writes bash function definitions for all directives in f to w.
func Generate(w io.Writer, f *parse.File) error {
	if _, err := fmt.Fprintln(w, "# Generated by mmk3. Do not edit."); err != nil {
		return err
	}

	// Collect user-defined deftypes, defbodies, and defrunner phases so built-ins
	// can be suppressed when the user provides their own definition.
	userDefType := make(map[string]bool)
	userDefBody := make(map[string]bool)
	userDefVerbBody := make(map[string]map[string]bool) // type → verb → true
	userRunnerPhase := make(map[string]map[string]bool) // type → phase → true
	for _, d := range f.Directives {
		switch d := d.(type) {
		case *parse.DefType:
			userDefType[d.Name] = true
		case *parse.DefBody:
			if d.Verb == "" {
				userDefBody[d.Type] = true
			} else {
				if userDefVerbBody[d.Type] == nil {
					userDefVerbBody[d.Type] = make(map[string]bool)
				}
				userDefVerbBody[d.Type][d.Verb] = true
			}
		case *parse.DefRunner:
			phase := d.Phase
			if phase == "" {
				phase = "run"
			}
			if userRunnerPhase[d.Name] == nil {
				userRunnerPhase[d.Name] = make(map[string]bool)
			}
			userRunnerPhase[d.Name][phase] = true
		}
	}

	// Emit built-in deftype functions for types not overridden by the user.
	for _, typeName := range builtinOrder {
		if userDefType[typeName] {
			continue
		}
		body := BuiltinDefTypes[typeName]
		if _, err := fmt.Fprintf(w, "\n# built-in deftype: %s\n%s() {%s}\n", typeName, TypeFunc(typeName), body); err != nil {
			return err
		}
	}

	// Emit built-in default body functions for types not overridden by the user.
	for _, typeName := range builtinOrder {
		if userDefBody[typeName] {
			continue
		}
		body := builtinDefBodies[typeName]
		if _, err := fmt.Fprintf(w, "\n# built-in default body: %s\n%s() {%s}\n", typeName, DefaultFunc(typeName), body); err != nil {
			return err
		}
	}

	// Emit built-in verb body functions for (type, verb) pairs not overridden by the user.
	for _, typeName := range builtinOrder {
		verbBodies := BuiltinVerbBodies[typeName]
		for _, verb := range sortedKeys(verbBodies) {
			if userDefVerbBody[typeName][verb] {
				continue
			}
			body := verbBodies[verb]
			if _, err := fmt.Fprintf(w, "\n# built-in defbody %s %s\n%s() {%s}\n", typeName, verb, DefaultVerbFunc(typeName, verb), body); err != nil {
				return err
			}
		}
	}

	// Emit built-in runner functions (setup, run, cleanup) for types not overridden by the user.
	for _, typeName := range builtinRunnerOrder {
		def := builtinRunnerDefs[typeName]
		phases := userRunnerPhase[typeName]
		type phaseEmit struct {
			phase   string
			fn      string
			body    string
			comment string
		}
		candidates := []phaseEmit{
			{"setup", RunnerSetupFunc(typeName), def.Setup, "built-in runner setup: " + typeName},
			{"run", RunnerRunFunc(typeName), def.Run, "built-in runner run: " + typeName},
			{"cleanup", RunnerCleanupFunc(typeName), def.Cleanup, "built-in runner cleanup: " + typeName},
		}
		for _, c := range candidates {
			if c.body == "" || phases[c.phase] {
				continue
			}
			body := normalizeBody(c.body)
			if _, err := fmt.Fprintf(w, "\n# %s\n%s() {%s}\n", c.comment, c.fn, body); err != nil {
				return err
			}
		}
	}

	// Track all types that have a default body (built-in or user-defined).
	hasDefault := make(map[string]bool)
	for typeName := range builtinDefBodies {
		hasDefault[typeName] = true
	}
	for _, d := range f.Directives {
		if db, ok := d.(*parse.DefBody); ok {
			if db.Verb == "" {
				hasDefault[db.Type] = true
			}
		}
	}

	for _, d := range f.Directives {
		switch d := d.(type) {
		case *parse.Passthrough:
			if _, err := fmt.Fprintln(w, d.Line); err != nil {
				return err
			}
			continue
		case *parse.DefBody:
			if err := ValidateName(d.Type); err != nil {
				return fmt.Errorf("defbody: %w", err)
			}
			body := normalizeBody(d.Body)
			if d.Verb != "" {
				if err := ValidateName(d.Verb); err != nil {
					return fmt.Errorf("defbody verb: %w", err)
				}
				if _, err := fmt.Fprintf(w, "\n# defbody %s %s\n%s() {%s}\n", d.Type, d.Verb, DefaultVerbFunc(d.Type, d.Verb), body); err != nil {
					return err
				}
			} else {
				if _, err := fmt.Fprintf(w, "\n# defbody %s\n%s() {%s}\n", d.Type, DefaultFunc(d.Type), body); err != nil {
					return err
				}
			}
			continue
		}

		var name, body, comment string
		switch d := d.(type) {
		case *parse.DefType:
			if err := ValidateName(d.Name); err != nil {
				return fmt.Errorf("deftype: %w", err)
			}
			name = TypeFunc(d.Name)
			body = d.Body
			comment = "deftype " + d.Name
		case *parse.DefRunner:
			if err := ValidateName(d.Name); err != nil {
				return fmt.Errorf("defrunner: %w", err)
			}
			phase := d.Phase
			if phase == "" {
				phase = "run"
			}
			switch phase {
			case "setup":
				name = RunnerSetupFunc(d.Name)
			case "run":
				name = RunnerRunFunc(d.Name)
			case "cleanup":
				name = RunnerCleanupFunc(d.Name)
			}
			body = d.Body
			comment = "defrunner " + d.Name + " " + phase
		case *parse.TargetRule:
			if d.Pattern != "" {
				continue // pattern rules are instantiated on demand by the runtime
			}
			if err := ValidateName(d.Target); err != nil {
				return fmt.Errorf("target: %w", err)
			}
			if d.Verb != "" {
				if err := ValidateName(d.Verb); err != nil {
					return fmt.Errorf("verb rule verb: %w", err)
				}
				name = VerbTargetFunc(d.Verb, d.Target)
				body = d.Body
				comment = "verb " + d.Verb + " for target: " + d.Target
			} else {
				name = TargetFunc(d.Target)
				body = d.Body
				comment = "target: " + d.Target
				if d.Type != "" {
					comment += " (type: " + d.Type + ")"
				}
				// No explicit body: use the type's default if one exists.
				if body == "" && d.Type != "" && hasDefault[d.Type] {
					body = "\n\t" + DefaultFunc(d.Type) + "\n"
				}
			}
		}

		body = normalizeBody(body)

		if _, err := fmt.Fprintf(w, "\n# %s\n%s() {%s}\n", comment, name, body); err != nil {
			return err
		}
	}
	return nil
}

// normalizeBody ensures a function body is suitable for embedding in `f() { body }`:
// empty bodies become a no-op, and bodies that don't end with '\n' get one appended
// (bash requires a newline or ';' before the closing '}').
func normalizeBody(body string) string {
	if body == "" {
		return "\n\t:\n"
	}
	if !strings.HasSuffix(body, "\n") {
		return body + "\n"
	}
	return body
}

// GenerateRule writes a single concrete target function to w.
// Used by the runtime when instantiating pattern-matched targets on demand.
func GenerateRule(w io.Writer, rule *parse.TargetRule) error {
	if err := ValidateName(rule.Target); err != nil {
		return fmt.Errorf("target: %w", err)
	}
	body := normalizeBody(rule.Body)
	comment := "target: " + rule.Target
	if rule.Type != "" {
		comment += " (type: " + rule.Type + ")"
	}
	_, err := fmt.Fprintf(w, "\n# %s\n%s() {%s}\n", comment, TargetFunc(rule.Target), body)
	return err
}

// GenerateVerbRule writes a single concrete verb target function to w.
// Used by the runtime when instantiating pattern-matched verb targets on demand.
func GenerateVerbRule(w io.Writer, rule *parse.TargetRule) error {
	if err := ValidateName(rule.Target); err != nil {
		return fmt.Errorf("target: %w", err)
	}
	if err := ValidateName(rule.Verb); err != nil {
		return fmt.Errorf("verb rule verb: %w", err)
	}
	body := normalizeBody(rule.Body)
	comment := fmt.Sprintf("verb %s for target: %s", rule.Verb, rule.Target)
	_, err := fmt.Fprintf(w, "\n# %s\n%s() {%s}\n", comment, VerbTargetFunc(rule.Verb, rule.Target), body)
	return err
}

// ValidateDuplicates returns an error if any concrete (target, verb) pair appears more than once.
// Pattern rules are skipped; duplicate patterns can only be detected at match time.
func ValidateDuplicates(f *parse.File) error {
	type tvKey struct{ target, verb string }
	seen := make(map[tvKey]bool)
	for _, d := range f.Directives {
		r, ok := d.(*parse.TargetRule)
		if !ok || r.Pattern != "" {
			continue
		}
		key := tvKey{r.Target, r.Verb}
		if seen[key] {
			if r.Verb != "" {
				return fmt.Errorf("duplicate verb rule [%s %s]", r.Verb, r.Target)
			}
			return fmt.Errorf("duplicate target %q", r.Target)
		}
		seen[key] = true
	}
	return nil
}

// PrintBuiltins writes the built-in type and runner definitions as mmk syntax to w.
func PrintBuiltins(w io.Writer) error {
	fmt.Fprintln(w, "# mmk built-in type definitions")
	fmt.Fprintln(w, "# Override any of these in your Mmkfile with deftype / defbody / defrunner.")
	for _, typeName := range builtinOrder {
		if body, ok := BuiltinDefTypes[typeName]; ok {
			if _, err := fmt.Fprintf(w, "\ndeftype %s {%s}\n", typeName, body); err != nil {
				return err
			}
		}
		if body, ok := builtinDefBodies[typeName]; ok {
			if _, err := fmt.Fprintf(w, "\ndefbody %s {%s}\n", typeName, body); err != nil {
				return err
			}
		}
		for _, verb := range sortedKeys(BuiltinVerbBodies[typeName]) {
			body := BuiltinVerbBodies[typeName][verb]
			if _, err := fmt.Fprintf(w, "\ndefbody %s %s {%s}\n", typeName, verb, body); err != nil {
				return err
			}
		}
	}
	for _, typeName := range builtinRunnerOrder {
		def := builtinRunnerDefs[typeName]
		if def.Setup != "" {
			if _, err := fmt.Fprintf(w, "\ndefrunner %s setup {%s}\n", typeName, def.Setup); err != nil {
				return err
			}
		}
		if def.Run != "" {
			if _, err := fmt.Fprintf(w, "\ndefrunner %s {%s}\n", typeName, def.Run); err != nil {
				return err
			}
		}
		if def.Cleanup != "" {
			if _, err := fmt.Fprintf(w, "\ndefrunner %s cleanup {%s}\n", typeName, def.Cleanup); err != nil {
				return err
			}
		}
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
