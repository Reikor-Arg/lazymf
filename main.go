// Command lazymf reminds the main AI session of its live background jobs.
//
// It hooks into the end-of-turn hook of AI coding tools. See README.md.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is injected at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `lazymf - reminds your AI that it has background jobs running

Usage:
  lazymf hook <tool> [--interval N] [--max-age MIN] [--stuck MIN] [--mode auto|rewake|block] [--message S] [--event E]   (reads the hook JSON on stdin)
  lazymf install   [--tool X] [--interval N] [--max-age MIN] [--stuck MIN] [--mode auto|rewake|block] [--home DIR]
  lazymf doctor    [--home DIR]
  lazymf uninstall [--tool X] [--home DIR]
  lazymf version

Tools: claude, codex, copilot (or "all" for install/uninstall)
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, defaultWatcher()))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, w watcher) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "hook":
		return runHook(args[1:], stdin, stdout, stderr, w)
	case "install":
		return runInstall(args[1:], stdout, stderr, true)
	case "uninstall":
		return runInstall(args[1:], stdout, stderr, false)
	case "doctor":
		return runDoctor(args[1:], stdout, stderr)
	case "version", "--version":
		fmt.Fprintln(stdout, "lazymf", version)
		return 0
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprint(stderr, usage)
	return 2
}
