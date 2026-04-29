package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// installCmds are the Claude Code shell invocations that wire up this repo
// as a plugin marketplace and install the mmk skill plugin from it.
var installCmds = [][]string{
	{"claude", "plugin", "marketplace", "add", "knusbaum/mmk3"},
	{"claude", "plugin", "install", "mmk@mmk3"},
}

// runInstallSkill prints the exact commands it intends to run, prompts the
// user for confirmation, and shells out to the `claude` CLI. Refuses to run
// if `claude` is not on PATH.
func runInstallSkill() error {
	if _, err := exec.LookPath("claude"); err != nil {
		return errors.New("'claude' not found on PATH — install Claude Code first (https://claude.com/code) and re-run")
	}

	fmt.Println("To install the mmk Claude Code skill, the following commands will be run:")
	for _, c := range installCmds {
		fmt.Printf("    %s\n", strings.Join(c, " "))
	}
	fmt.Println()
	fmt.Print("Proceed? [Y/n] ")

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return fmt.Errorf("read confirmation: %w", err)
	}
	switch strings.TrimSpace(strings.ToLower(line)) {
	case "", "y", "yes":
	default:
		fmt.Println("Aborted.")
		return nil
	}

	for _, c := range installCmds {
		fmt.Printf("\n$ %s\n", strings.Join(c, " "))
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(c, " "), err)
		}
	}
	fmt.Println("\nDone. The mmk skill is now installed in Claude Code.")
	return nil
}
