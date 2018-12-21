// Copyright 2015 Keybase, Inc. All rights reserved. Use of
// this source code is governed by the included BSD license.

// +build !windows

package install

import (
	"os"

	"github.com/keybase/client/go/lsof"
)

// IsInUse returns true if the mount is in use. This may be used by the updater
// to determine if it's safe to apply an update and restart.
func IsInUse(mountDir string, log Log) bool {
	return len(LsofMount(mountDir, log)) > 0
}

func LsofMount(mountDir string, log Log) []lsof.Process {
	log.Debug("Mount dir: %s", mountDir)
	if mountDir == "" {
		return nil
	}
	if _, serr := os.Stat(mountDir); os.IsNotExist(serr) {
		log.Debug("%s doesn't exist", mountDir)
		return nil
	}

	log.Debug("Checking mount (lsof)")
	processes, err := lsof.MountPoint(mountDir)
	if err != nil {
		// If there is an error in lsof it's ok to continue
		// An exit status of 1 means that the mount is not in use, and is
		// not really an error.
		log.Debug("Continuing despite error in lsof: %s", err)
		return nil
	}
	return processes
}
