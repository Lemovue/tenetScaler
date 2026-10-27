package main

import (
	"errors"
	"flag"
	"fmt"

	"tenantscaler/internal/llama"
)

func init() {
	register("cleanup", "kill leftover llama-server replicas listed in the PID file (after a crash)", runCleanup)
}

func runCleanup(args []string) error {
	fs := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	pidFile := fs.String("pid-file", llama.DefaultPIDFile, "PID file written by the llama backend")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // -h printed the flags; asking for help is not a failure
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("cleanup takes no arguments, got %q", fs.Args())
	}
	res, err := llama.Cleanup(*pidFile)
	fmt.Printf("cleanup: %d PIDs listed in %s; killed %d running llama-server process(es) %v\n",
		res.Listed, *pidFile, len(res.Killed), res.Killed)
	if len(res.Skipped) > 0 {
		fmt.Printf("cleanup: left %d llama-server process(es) %v alone: started at another time than recorded (PID reused)\n",
			len(res.Skipped), res.Skipped)
	}
	return err
}
