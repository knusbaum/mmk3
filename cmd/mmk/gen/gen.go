// Package gen generates a bash function file from a parsed mmk AST.
// The output is a plain bash script of function definitions that can be
// sourced by per-task invocation scripts.
package gen

import (
	"fmt"
	"io"
	"runtime"
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

// RunnerDepsFunc returns the bash function name for the deps clause helper of
// a runner type. Built-in image uses this to express "no consumer-dep when
// skip_if matches." User defrunner deps clauses that need a helper function
// can adopt the same naming if they want it auto-exposed.
func RunnerDepsFunc(name string) string { return "__mmk_runner_deps_" + name }

// DefaultFunc returns the bash function name for a type's default body.
func DefaultFunc(typeName string) string { return "__mmk_default_" + typeName }

// VerbTargetFunc returns the bash function name for a verb rule on a target.
func VerbTargetFunc(verb, target string) string { return "__mmk_verb_" + verb + "_target_" + target }

// DefaultVerbFunc returns the bash function name for a type's default verb body.
func DefaultVerbFunc(typeName, verb string) string { return "__mmk_default_" + verb + "_" + typeName }

// statMtime is the bash body that prints the target's mtime to stdout.
// stat's flags differ across platforms: -c %Y (GNU coreutils) vs -f %m (BSD).
// Pick the right one based on the host OS where mmk runs.
var statMtime = func() string {
	if runtime.GOOS == "darwin" || runtime.GOOS == "freebsd" || runtime.GOOS == "openbsd" || runtime.GOOS == "netbsd" {
		return "\n\tstat -f %m \"$target\" 2>/dev/null || return 1\n"
	}
	return "\n\tstat -c %Y \"$target\" 2>/dev/null || return 1\n"
}()

// BuiltinDefTypes contains the built-in deftype body (bash printing a timestamp)
// for each built-in type. A user deftype with the same name overrides these.
//
// directory's deftype reports a fixed-low date (1) when the directory exists
// so that consumers ("file foo : src.c bar_dir") don't see the directory's
// own mtime — which would otherwise rise every time a file is added or
// removed from it, churning every consumer. Absence still returns non-zero
// (the defbody then runs `mkdir -p`).
var BuiltinDefTypes = map[string]string{
	"file":      statMtime,
	"image":     "\n\tdocker inspect --format '{{.Created}}' \"$target\" 2>/dev/null || return 1\n",
	"source":    statMtime,
	"directory": "\n\t[ -d \"$target\" ] && echo 1 || return 1\n",
}

// BuiltinDefBodies contains the built-in default body for each built-in type.
// A user defbody for the same type name overrides these.
var BuiltinDefBodies = map[string]string{
	"file":      "\n\t[[ -e \"$target\" ]] && return 0\n\tprintf 'mmk: %s does not exist and has no rule to create it\\n' \"$target\" >&2; return 1\n",
	"image":     "\n\tif [[ -n \"$deps\" ]]; then\n\t\tdocker build ${platform:+--platform \"$platform\"} -t \"$target\" -f \"${deps%% *}\" .\n\telse\n\t\tdocker pull ${platform:+--platform \"$platform\"} \"$target\"\n\tfi\n",
	"source":    "\n\t[[ -e \"$target\" ]] && return 0\n\tprintf 'mmk: %s does not exist and has no rule to create it\\n' \"$target\" >&2; return 1\n",
	"directory": "\n\tmkdir -p \"$target\"\n",
}

// BuiltinVerbBodies contains the built-in verb body for (type, verb) pairs.
// A user defbody for the same type+verb overrides these.
//
// directory clean uses rm -rf so it works even when files in the directory
// haven't been cleaned yet (matching what a user typically wants from
// "clean the directory"). The file type's `rm -f` is the analog for one
// artifact; this is the analog for a tree.
var BuiltinVerbBodies = map[string]map[string]string{
	"file":      {"clean": "\n\trm -f \"$target\"\n"},
	"image":     {"clean": "\n\tdocker image inspect \"$target\" >/dev/null 2>&1 || return 0\n\tdocker image rm -f \"$target\"\n"},
	"directory": {"clean": "\n\trm -rf \"$target\"\n"},
}

// DefBodyOptionsKey identifies a (type, verb) pair for built-in defbody options.
// Verb is "" for the default-build defbody.
type DefBodyOptionsKey struct {
	Type string
	Verb string
}

// BuiltinDefBodyOptions ships options for built-in (type, verb) defbody pairs.
// `image clean` declares order=after-consumers so that cleaning a runner image
// is automatically sequenced after every target that uses the image, without
// the user needing to spell the ordering out per-mmkfile.
var BuiltinDefBodyOptions = map[DefBodyOptionsKey][]parse.Option{
	{Type: "image", Verb: "clean"}: {{Key: "order", Value: "after-consumers"}},
}

// RunnerDefInfo describes which optional phases are defined for a built-in runner type.
// The run phase is always present for any valid runner type.
//
// Deps is the dep clause tokens from `defrunner T : depexpr ... { run body }`.
// Each entry is a raw bash expression evaluated at graph construction time per
// runner instance; the output is word-split and appended to the dep list of
// every target that says `on T`. Default (nil/empty Deps): the runner target
// itself is auto-added — the historical behavior.
type RunnerDefInfo struct {
	HasSetup   bool
	HasCleanup bool
	Deps       []string
}

// BuiltinRunnerDefs maps type names that have built-in runner definitions to
// info about which phases exist. The runtime uses this to know which types are
// valid runners, whether to call setup/cleanup, and what deps `on T` injects.
var BuiltinRunnerDefs = map[string]RunnerDefInfo{
	"image": {
		HasSetup:   true,
		HasCleanup: true,
		// Empty output ⇒ no implicit dep on the image target (skip_if matched);
		// otherwise depend on $target so the image is built before consumers run.
		Deps: []string{"$(" + RunnerDepsFunc("image") + ")"},
	},
}

type runnerDefBodies struct {
	Setup   string
	Run     string
	Cleanup string
	// DepsHelper is the bash body of __mmk_runner_deps_<type>, emitted as a
	// top-level helper alongside the runner phase functions. The exported
	// RunnerDefInfo.Deps references it via $(...) so the dep clause stays a
	// one-liner in the mmk-syntax form printed by -builtins.
	DepsHelper string
}

// builtinRunnerDefs holds the bash bodies for each built-in runner type.
// These are emitted as __mmk_runner_{setup,run,cleanup}_<type> functions unless
// the user overrides them with their own defrunner directives.
// __mmk_skip is the sentinel runner state used when skip_if matched at setup
// time. The run and cleanup phases short-circuit on it.
const skipSentinel = "__mmk_skip__"

// skipIfCheck is bash that evaluates the user-supplied skip_if option:
//
//	empty   -> don't skip (returns 1)
//	auto    -> skip if any common in-container signal matches
//	<bash>  -> evaluate the snippet; skip if it returns 0
//
// Emitted at the top of each phase body. Each phase has its own copy because
// the built-in's bodies are emitted as separate bash functions.
const skipIfCheck = `	__mmk_skip_check() {
		case "$skip_if" in
			"")    return 1 ;;
			auto)  [ -f /.dockerenv ] || [ -f /run/.containerenv ] \
					|| [ -n "${KUBERNETES_SERVICE_HOST:-}" ] \
					|| grep -qE 'docker|containerd' /proc/1/cgroup 2>/dev/null ;;
			*)     eval "$skip_if" ;;
		esac
	}
`

// userFlag is bash that populates the array __mmk_user with --user flags
// based on the user= option:
//
//	empty   -> no --user flag (image's USER directive applies)
//	host    -> --user $(id -u):$(id -g) on Linux; nothing on macOS/BSD where
//	           the host UID typically doesn't exist inside the container
//	<value> -> --user <value> verbatim
//
// Bind-mounted files written from inside the container will be owned by the
// resulting UID, so `host` is the right choice when the build artifacts are
// expected to be readable by the developer who started the container.
const userFlag = `	__mmk_user=()
	case "$user" in
		"")    : ;;
		host)  if [ "$(uname -s)" = "Linux" ]; then __mmk_user=(--user "$(id -u):$(id -g)"); fi ;;
		*)     __mmk_user=(--user "$user") ;;
	esac
`

// imageDepsHelper is the bash body of the image runner's deps clause.
// $target is the runner target; $skip_if is the image's skip_if option.
// Output: empty when skipping (no consumer-dep on the image); otherwise the
// image target's name so consumers depend on it the same way they always have.
const imageDepsHelper = `
` + skipIfCheck + `	if __mmk_skip_check; then
		return 0
	fi
	printf '%s' "$target"
`

var builtinRunnerDefs = map[string]runnerDefBodies{
	"image": {
		DepsHelper: imageDepsHelper,
		Setup: `
` + skipIfCheck + `	if __mmk_skip_check; then
		printf '` + skipSentinel + `'
		return 0
	fi
` + userFlag + `	name="mmk-$(printf '%s' "$target" | tr -cs 'a-zA-Z0-9' '-')-$$"
	docker rm -f "$name" 2>/dev/null || true
	id=$(docker run -d --rm \
		${platform:+--platform "$platform"} \
		"${__mmk_user[@]}" \
		--name "$name" \
		-v "$(pwd):/work" \
		-v "$MMK_GENFILE:/mmk-generated.sh:ro" \
		-w /work \
		"$target" \
		sleep infinity)
	printf '%s' "$id"
`,
		Run: `
	if [ "$MMK_RUNNER_STATE" = "` + skipSentinel + `" ]; then
		target="$MMK_TARGET"; deps="$MMK_DEPS"; read -ra dep <<< "$deps"; eval "$MMK_EXECUTE"
		return $?
	fi
	# tty=true on the rule (or runner) opts the body into an in-container PTY
	# and host stdin forwarding. Used for interactive shells (bash -i) and
	# anything that needs line editing or TTY-detection. Default is no -t:
	# build/clean tasks don't need a PTY, and parallel -t calls would fight
	# over host terminal mode.
	__mmk_tty_flag=
	case "$tty" in
		""|0|false|no) ;;
		*) __mmk_tty_flag=-t ;;
	esac
	if [ -n "$__mmk_tty_flag" ]; then
		# Snapshot/restore the local terminal state. Allocating a remote PTY
		# puts the local tty in raw mode; if docker exec exits abnormally the
		# host shell would be left dirty (requiring 'stty sane'). The EXIT
		# trap also covers signal-driven exits.
		__mmk_tty_save=$(stty -g 2>/dev/null) || __mmk_tty_save=
		[ -n "$__mmk_tty_save" ] && trap 'stty "$__mmk_tty_save" 2>/dev/null' EXIT
	fi
	__mmk_extra_env=()
	for __mmk_v in $forward_env; do __mmk_extra_env+=(-e "$__mmk_v"); done
	# Forward the consumer rule's option keys too. Without this, options
	# (e.g. ` + "`source=./src`" + ` on a typed target) are visible in this runner
	# script's environment but never reach the body's bash inside the container.
	for __mmk_v in $MMK_RULE_OPT_KEYS; do __mmk_extra_env+=(-e "$__mmk_v"); done
` + userFlag + `	docker exec -i $__mmk_tty_flag \
		"${__mmk_user[@]}" \
		-e "MMK_TARGET=$MMK_TARGET" \
		-e "MMK_DEPS=$MMK_DEPS" \
		-e MMK_EXECUTE \
		-e MMK_VERBOSE \
		"${__mmk_extra_env[@]}" \
		"$MMK_RUNNER_STATE" \
		bash -c ". /mmk-generated.sh; target=\"\$MMK_TARGET\"; deps=\"\$MMK_DEPS\"; read -ra dep <<< \"\$deps\"; eval \"\$MMK_EXECUTE\""
`,
		Cleanup: `
	[ "$MMK_RUNNER_STATE" = "` + skipSentinel + `" ] && return 0
	docker rm -f "$MMK_RUNNER_STATE" 2>/dev/null || true
`,
	},
}

// builtinRunnerOrder lists built-in runner types in a deterministic emit order.
var builtinRunnerOrder = []string{"image"}

// builtinOrder lists built-in types in a deterministic emit order.
var builtinOrder = []string{"source", "file", "directory", "image"}

// Generate writes bash function definitions for all directives in f to w.
// frozen is pre-evaluated content that replaces all passthrough directives;
// nil means emit passthrough lines verbatim (original behaviour).
func Generate(w io.Writer, f *parse.File, frozen []string) error {
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
		body := BuiltinDefBodies[typeName]
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
			body := NormalizeBody(c.body)
			if _, err := fmt.Fprintf(w, "\n# %s\n%s() {%s}\n", c.comment, c.fn, body); err != nil {
				return err
			}
		}
		// Emit the deps-clause helper alongside the runner. Suppressed when
		// the user provides their own run-stage defrunner (they own the deps
		// clause too in that case).
		if def.DepsHelper != "" && !phases["run"] {
			body := NormalizeBody(def.DepsHelper)
			if _, err := fmt.Fprintf(w, "\n# built-in runner deps helper: %s\n%s() {%s}\n", typeName, RunnerDepsFunc(typeName), body); err != nil {
				return err
			}
		}
	}

	frozenEmitted := false
	for _, d := range f.Directives {
		switch d := d.(type) {
		case *parse.Passthrough:
			if frozen != nil {
				if !frozenEmitted {
					for _, line := range frozen {
						if _, err := fmt.Fprintln(w, line); err != nil {
							return err
						}
					}
					frozenEmitted = true
				}
			} else {
				if _, err := fmt.Fprintln(w, d.Line); err != nil {
					return err
				}
			}
			continue
		case *parse.DefBody:
			if err := ValidateName(d.Type); err != nil {
				return fmt.Errorf("defbody: %w", err)
			}
			body := NormalizeBody(d.Body)
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
			continue // target bodies are passed via MMK_EXECUTE at execution time
		case *parse.Subproject:
			continue // subproject directives are expanded at runtime into TargetRules
		case *parse.Group:
			continue // group declarations have no bash representation
		}

		body = NormalizeBody(body)

		if _, err := fmt.Fprintf(w, "\n# %s\n%s() {%s}\n", comment, name, body); err != nil {
			return err
		}
	}
	return nil
}

// NormalizeBody ensures a function body is suitable for embedding in `f() { body }`:
// empty bodies become a no-op, and bodies that don't end with '\n' get one appended
// (bash requires a newline or ';' before the closing '}').
func NormalizeBody(body string) string {
	if body == "" {
		return "\n\t:\n"
	}
	if !strings.HasSuffix(body, "\n") {
		return body + "\n"
	}
	return body
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
		if body, ok := BuiltinDefBodies[typeName]; ok {
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
		info := BuiltinRunnerDefs[typeName]
		if def.DepsHelper != "" {
			if _, err := fmt.Fprintf(w, "\n# Helper invoked from `defrunner %s : $(...)` below.\n%s() {%s}\n", typeName, RunnerDepsFunc(typeName), def.DepsHelper); err != nil {
				return err
			}
		}
		if def.Setup != "" {
			if _, err := fmt.Fprintf(w, "\ndefrunner %s setup {%s}\n", typeName, def.Setup); err != nil {
				return err
			}
		}
		if def.Run != "" {
			depsClause := ""
			if len(info.Deps) > 0 {
				depsClause = " : " + strings.Join(info.Deps, " ")
			}
			if _, err := fmt.Fprintf(w, "\ndefrunner %s%s {%s}\n", typeName, depsClause, def.Run); err != nil {
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
