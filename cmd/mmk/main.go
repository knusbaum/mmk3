package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/knusbaum/mmk3/cmd/mmk/gen"
	"github.com/knusbaum/mmk3/cmd/mmk/runtime"
)

func main() {
	j := flag.Int("j", 0, "parallelism (0 = unlimited)")
	v := flag.Bool("v", false, "verbose: log each target as it runs or is skipped")
	dump := flag.Bool("dump", false, "print generated shell script and exit")
	builtins := flag.Bool("builtins", false, "print built-in type definitions as mmk syntax and exit")
	list := flag.Bool("list", false, "list available targets and verbs, then exit")
	graph := flag.Bool("graph", false, "print dependency tree for target and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: mmk [-j N] [-v] [-dump] [-builtins] [-list] [-graph] [[verb] target]\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *builtins {
		if err := gen.PrintBuiltins(os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "mmk: %v\n", err)
			os.Exit(1)
		}
		return
	}

	src, err := readMmkfile()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mmk: %v\n", err)
		os.Exit(1)
	}

	b, err := runtime.NewBuild(src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mmk: %v\n", err)
		os.Exit(1)
	}
	defer b.Close()
	b.Verbose = *v

	if *list {
		b.PrintList(os.Stdout)
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
		if err := b.Graph(target, verb); err != nil {
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

// readMmkfile reads the mmkfile from the current directory.
// It looks for Mmkfile first, then mmkfile.
func readMmkfile() ([]byte, error) {
	for _, name := range []string{"Mmkfile", "mmkfile"} {
		data, err := os.ReadFile(name)
		if err == nil {
			return data, nil
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("no Mmkfile or mmkfile found in current directory")
}
