package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/knusbaum/mmk3/cmd/mmk/runtime"
)

func main() {
	j := flag.Int("j", 0, "parallelism (0 = unlimited)")
	v := flag.Bool("v", false, "verbose: log each target as it runs or is skipped")
	dump := flag.Bool("dump", false, "print generated shell script and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: mmk [-j N] [-v] [-dump] [[verb] target]\n")
		flag.PrintDefaults()
	}
	flag.Parse()

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

	if *dump {
		data, err := os.ReadFile(b.GenPath())
		if err != nil {
			fmt.Fprintf(os.Stderr, "mmk: %v\n", err)
			os.Exit(1)
		}
		os.Stdout.Write(data)
		return
	}

	verb := ""
	target := "all"
	switch flag.NArg() {
	case 0:
		// defaults
	case 1:
		arg := flag.Arg(0)
		if b.HasTarget(arg) {
			target = arg
		} else {
			verb = arg
		}
	case 2:
		verb = flag.Arg(0)
		target = flag.Arg(1)
	default:
		fmt.Fprintf(os.Stderr, "usage: mmk [-j N] [-v] [[verb] target]\n")
		os.Exit(1)
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
