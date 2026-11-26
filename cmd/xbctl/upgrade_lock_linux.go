package main

import (
	"fmt"
	"os"
	"syscall"
)

// A directory lock has no stale marker after an interrupted process exits.
func lockInstallDir(path string) (*os.File, error) {
	dir, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(dir.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		dir.Close()
		return nil, fmt.Errorf("cannot lock installation directory for upgrade: %w", err)
	}
	return dir, nil
}
