// Command nano-harness is the composition root for the nano-harness runtime.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/jinyule/nano-harness/internal/version"
)

// exitProcess is replaceable only so package tests can execute the real main path.
var exitProcess = os.Exit

func main() {
	exitProcess(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		if err := printUsage(stderr); err != nil {
			return 1
		}
		return 2
	}

	switch args[0] {
	case "version", "--version", "-version":
		if _, err := fmt.Fprintln(stdout, version.Current()); err != nil {
			return 1
		}
		return 0
	case "help", "--help", "-h":
		if err := printUsage(stdout); err != nil {
			return 1
		}
		return 0
	default:
		if _, err := fmt.Fprintf(stderr, "unknown command %q\n", args[0]); err != nil {
			return 1
		}
		if err := printUsage(stderr); err != nil {
			return 1
		}
		return 2
	}
}

func printUsage(w io.Writer) error {
	_, err := fmt.Fprintln(w, "usage: nano-harness <version|help>")
	return err
}
