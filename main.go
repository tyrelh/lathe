// Command lathe orchestrates bounded agent work in whatever repo you invoke it from.
package main

import (
	"fmt"
	"os"

	"github.com/tyrelh/lathe/internal/install"
)

const usage = `lathe — a software factory you run from any repo

usage: lathe <command> [args]

commands:
  install    link this repo into each agent's skills directory
  help       show this message

Run install from a checkout of the lathe repo; it links the checkout itself,
so SKILL.md stays discoverable.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "install":
		cwd, err := os.Getwd()
		if err == nil {
			err = install.Run(cwd, os.Stdout)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "lathe install:", err)
			os.Exit(1)
		}
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "lathe: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}
