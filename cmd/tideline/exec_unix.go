//go:build !windows

package main

import "syscall"

// execReplace replaces this process with another program.
func execReplace(bin string, args, env []string) error {
	return syscall.Exec(bin, args, env)
}
