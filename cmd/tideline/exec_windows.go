package main

import (
	"errors"
	"os"
	"os/exec"
)

// execReplace runs another program in this process's place: Windows can't
// replace a running process, so it runs as a child and its exit code is
// passed on.
func execReplace(bin string, args, env []string) error {
	cmd := exec.Command(bin, args[1:]...)
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = env, os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		os.Exit(ee.ExitCode())
	}
	if err != nil {
		return err
	}
	os.Exit(0)
	return nil
}
