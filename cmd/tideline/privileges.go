//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// dropPrivileges lets the container image start as root, fix ownership of
// the data volume, then run as an unprivileged user.
//
// Hosts such as Fly.io and Railway mount volumes owned by root, which an
// image that starts as non-root can't write to. When TIDELINE_RUN_AS is set
// (the Docker image sets it to 65532:65532) and the process is root, this
// hands the database directory and its files to that user and switches to
// it before anything else runs. Outside that case it does nothing.
func dropPrivileges(dbPath string) error {
	spec := env("TIDELINE_RUN_AS")
	if spec == "" || os.Getuid() != 0 {
		return nil
	}
	uidStr, gidStr, ok := strings.Cut(spec, ":")
	if !ok {
		gidStr = uidStr
	}
	uid, err1 := strconv.Atoi(uidStr)
	gid, err2 := strconv.Atoi(gidStr)
	if err1 != nil || err2 != nil || uid <= 0 || gid <= 0 {
		return fmt.Errorf("TIDELINE_RUN_AS must be uid[:gid] with non-zero ids, got %q", spec)
	}

	dir, err := filepath.Abs(filepath.Dir(dbPath))
	if err != nil {
		return err
	}
	if dir == "/" {
		return errors.New("refusing to change ownership of /: set TIDELINE_DB to a file inside a data directory")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	// Only the directory and the database's own files, never anything else
	// that might share the mount.
	for _, p := range []string{dir, dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Lchown(p, uid, gid); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("chown %s: %w", p, err)
		}
	}
	// Order matters: supplementary groups and gid while still root, uid last.
	if err := syscall.Setgroups([]int{}); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("setgid: %w", err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("setuid: %w", err)
	}
	if os.Getuid() == 0 || os.Geteuid() == 0 {
		return errors.New("still running as root after dropping privileges")
	}
	return nil
}
