//go:build !linux

package main

import (
	"errors"
	"os"
)

func lockInstallDir(string) (*os.File, error) {
	return nil, errors.New("automatic binary upgrades require Linux")
}
