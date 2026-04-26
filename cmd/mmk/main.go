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
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: mmk [-j N] [-v] [target]\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	target := "all"
	if flag.NArg() > 0 {
		target = flag.Arg(0)
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

	if err := b.Execute(target, *j); err != nil {
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
