package main

import (
	"flag"
	"io"
)

// Generate installation metadata without rewriting the user's configuration.
func runConfigMetadata(args []string) error {
	flags := flag.NewFlagSet("metadata", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", defaultConfigPath, "config path")
	metaPath := flags.String("meta", defaultMetaPath, "metadata path")
	ver := flags.String("version", "unknown", "installed release")
	if err := flags.Parse(args); err != nil {
		return err
	}
	root, err := loadWritableRootConfig(*configPath)
	if err != nil {
		return err
	}
	return writeInstallMetaVersioned(*metaPath, root, *ver, "")
}
