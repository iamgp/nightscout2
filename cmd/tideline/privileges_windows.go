package main

import "errors"

// dropPrivileges is a Unix concept; on Windows it only refuses a setting
// that can't be honoured.
func dropPrivileges(string) error {
	if env("TIDELINE_RUN_AS") != "" {
		return errors.New("TIDELINE_RUN_AS is not supported on Windows")
	}
	return nil
}
