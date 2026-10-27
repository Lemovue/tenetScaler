// Command tenantscaler is the single TenantScaler binary. Each component is a
// subcommand (gateway, controller, pool, replica, demo, experiment, ...).
// Subcommands register themselves from their own files via register().
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"

	"tenantscaler/internal/envfile"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "0.1.0-dev"

type command struct {
	summary string
	run     func(args []string) error
}

var commands = map[string]command{}

func register(name, summary string, run func(args []string) error) {
	if _, dup := commands[name]; dup {
		panic("duplicate subcommand " + name)
	}
	commands[name] = command{summary: summary, run: run}
}

func init() {
	register("version", "print the version", func([]string) error {
		fmt.Println("tenantscaler", version)
		return nil
	})
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: tenantscaler <command> [flags]")
	fmt.Fprintln(os.Stderr, "\ncommands:")
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(os.Stderr, "  %-16s %s\n", n, commands[n].summary)
	}
	fmt.Fprintln(os.Stderr, "\nrun 'tenantscaler <command> -h' for command flags")
}

func main() {
	if err := envfile.Load(".env"); err != nil {
		fmt.Fprintln(os.Stderr, "warning:", err)
	}
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		usage()
		os.Exit(2)
	}
	cmd, ok := commands[os.Args[1]]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err := cmd.run(os.Args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return // -h printed the flag usage; that is success
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
