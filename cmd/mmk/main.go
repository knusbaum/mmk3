package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"syscall"

	"github.com/knusbaum/mmk3/cmd/mmk/gen"
	"github.com/knusbaum/mmk3/cmd/mmk/runtime"
	"github.com/knusbaum/mmk3/cmd/mmk/tui"
	"github.com/knusbaum/mmk3/cmd/mmk/tui/dagview"
)

func main() {
	raiseOpenFileLimit()
	jDefault := 100
	if s := os.Getenv("MMK_J"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			jDefault = n
		}
	}
	j := flag.Int("j", jDefault, "parallelism (0 = unlimited; default overridden by $MMK_J)")
	v := flag.Bool("v", false, "verbose: log each target as it runs or is skipped")
	why := flag.Bool("why", false, "print the dep chain from root → target as each target starts")
	dump := flag.Bool("dump", false, "print generated shell script and exit")
	builtins := flag.Bool("builtins", false, "print built-in type definitions as mmk syntax and exit")
	list := flag.Bool("list", false, "list available targets and verbs, then exit")
	all := flag.Bool("all", false, "with -list, also include internal targets (no docstring); without -all only docstringed targets and 'all' are shown")
	graph := flag.Bool("graph", false, "print dependency tree for target and exit")
	dagGraph := flag.Bool("dag", false, "print dependency DAG (boxes + arrows) for target and exit")
	dagMGroup := flag.Bool("mgroup", false, "with -dag, collapse matrix combos sharing a base into one box")
	full := flag.Bool("full", false, "with -graph, recurse into subprojects (one mmk subprocess per subproject)")
	useTUI := flag.Bool("tui", false, "render the build as a live TUI tree with status updates")
	installSkill := flag.Bool("install-skill", false, "install the mmk Claude Code skill via 'claude plugin' commands (Y/n prompt before running)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: mmk [-j N] [-v] [-dump] [-builtins] [-list [-all]] [-graph [-full]] [[verb] target]\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	flag.Visit(func(f *flag.Flag) {
		if f.Name == "j" {
			os.Setenv("MMK_J", strconv.Itoa(*j))
		}
	})

	if *installSkill {
		if err := runInstallSkill(); err != nil {
			fmt.Fprintf(os.Stderr, "mmk: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *builtins {
		if err := gen.PrintBuiltins(os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "mmk: %v\n", err)
			os.Exit(1)
		}
		return
	}

	mmkfilePath, err := findMmkfile()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mmk: %v\n", err)
		os.Exit(1)
	}

	b, err := runtime.NewBuildFromFile(mmkfilePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mmk: %v\n", err)
		os.Exit(1)
	}
	defer b.Close()
	// MMK_VERBOSE=1 (forwarded from a parent mmk that was started with -v)
	// makes a sub-mmk inherit verbose mode without the parent having to
	// rewrite synthetic subproject bodies to pass -v explicitly.
	b.Verbose = *v || os.Getenv("MMK_VERBOSE") == "1"
	b.Why = *why

	if *list {
		b.PrintList(os.Stdout, *all)
		return
	}

	verb := ""
	target := "all"
	switch flag.NArg() {
	case 0:
		// defaults
	case 1:
		arg := flag.Arg(0)
		if b.HasTarget(arg) || b.ResolveSubpath(arg, "") {
			target = arg
		} else {
			verb = arg
		}
	case 2:
		verb = flag.Arg(0)
		target = flag.Arg(1)
		// If target has the form `subproject/rest`, register a delegating
		// rule so the rest of the pipeline can resolve it normally.
		b.ResolveSubpath(target, verb)
	default:
		fmt.Fprintf(os.Stderr, "usage: mmk [-j N] [-v] [[verb] target]\n")
		os.Exit(1)
	}

	if verb != "" && !b.HasVerb(verb) {
		fmt.Fprintf(os.Stderr, "mmk: unknown verb %q\n", verb)
		os.Exit(1)
	}

	if *dump {
		if err := b.Prepare(target, verb); err != nil {
			fmt.Fprintf(os.Stderr, "mmk: %v\n", err)
			os.Exit(1)
		}
		data, err := os.ReadFile(b.GenPath())
		if err != nil {
			fmt.Fprintf(os.Stderr, "mmk: %v\n", err)
			os.Exit(1)
		}
		os.Stdout.Write(data)
		return
	}

	if *graph {
		if err := b.Graph(target, verb, *full); err != nil {
			fmt.Fprintf(os.Stderr, "mmk: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *dagGraph {
		var root *runtime.TargetNode
		var err error
		if verb == "" {
			root, err = b.Resolve(target)
		} else {
			root, err = b.ResolveVerb(target, verb)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "mmk: %v\n", err)
			os.Exit(1)
		}
		res := dagview.View(root, nil, dagview.Options{
			UseUnicode:  true,
			GroupMatrix: *dagMGroup,
		})
		// Render entire drawing without ANSI; mmk -dag is intended for
		// piping/scrollback. Add color back when we have a -color flag.
		fmt.Print(res.Render(0, 0, res.W(), res.H(), false))
		return
	}

	if *useTUI {
		if err := tui.Run(b, target, verb, *j); err != nil {
			fmt.Fprintf(os.Stderr, "mmk: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := b.Execute(target, verb, *j); err != nil {
		fmt.Fprintf(os.Stderr, "mmk: %v\n", err)
		os.Exit(1)
	}
}

// raiseOpenFileLimit raises the soft open-file limit to the hard limit.
// Best-effort: errors are silently ignored.
func raiseOpenFileLimit() {
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err != nil {
		return
	}
	if rl.Cur < rl.Max {
		rl.Cur = rl.Max
		syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rl) //nolint — best-effort
	}
}

// findMmkfile returns the path to the mmkfile in the current directory.
// It looks for Mmkfile first, then mmkfile.
func findMmkfile() (string, error) {
	for _, name := range []string{"Mmkfile", "mmkfile"} {
		if _, err := os.Stat(name); err == nil {
			return name, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
	}
	return "", fmt.Errorf("no Mmkfile or mmkfile found in current directory")
}
