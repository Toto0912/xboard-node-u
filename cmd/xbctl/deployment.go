package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cedar2025/xboard-node/internal/config"
)

// deploymentPort is negative only for an explicit empty binding list.
// Read the installer's literal KEY=value credential format without invoking a shell.
func deploymentPort(path, credentials, workingDir, installedPath string) (int, error) {
	if _, err := os.Stat(path); err != nil {
		return 0, err
	}
	data, err := os.ReadFile(credentials)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || !envKeyPattern.MatchString(key) {
			return 0, fmt.Errorf("invalid credentials entry on line %d", i+1)
		}
		old, existed := os.LookupEnv(key)
		if err := os.Setenv(key, value); err != nil {
			return 0, fmt.Errorf("invalid credentials entry on line %d", i+1)
		}
		defer func() {
			if existed {
				_ = os.Setenv(key, old)
			} else {
				_ = os.Unsetenv(key)
			}
		}()
	}
	root, err := config.LoadRootFrom(path, installedPath)
	if err != nil {
		return 0, err
	}
	instances, err := root.NormalizeInstances()
	if err != nil {
		return 0, err
	}
	if len(instances) == 0 {
		return -1, nil
	}
	if err := config.ValidateStartupLayoutAt(instances, workingDir); err != nil {
		return 0, err
	}
	port := instances[0].HealthPort
	if port < 0 || port > 65535 {
		return 0, fmt.Errorf("invalid health_port %d", port)
	}
	return port, nil
}

func runConfigHealthPort(args []string) error {
	fs := flag.NewFlagSet("health-port", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("config", defaultConfigPath, "configuration file")
	env := fs.String("credentials", "", "service credentials file")
	workDir := fs.String("working-dir", "", "service working directory")
	installed := fs.String("installed-config", "", "final configuration pathname")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected health-port arguments")
	}
	if *env == "" {
		*env = filepath.Join(filepath.Dir(*path), "credentials.env")
	}
	if *installed == "" {
		*installed = *path
	}
	if *workDir == "" {
		*workDir = filepath.Dir(*installed)
	}
	port, err := deploymentPort(*path, *env, *workDir, *installed)
	if err != nil {
		return err
	}
	if port < 0 {
		fmt.Println("empty")
	} else {
		fmt.Println(port)
	}
	return nil
}
