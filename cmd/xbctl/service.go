package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	openRCService       = "xboard-node"
	alpineInstallerPath = defaultInstallRoot + "/install-alpine.sh"
)

type serviceManager string

const (
	systemd serviceManager = "systemd"
	openRC  serviceManager = "openrc"
)

func selectServiceManager(systemdRunning, openRCRunning bool) (serviceManager, error) {
	if systemdRunning {
		return systemd, nil
	}
	if openRCRunning {
		return openRC, nil
	}
	return "", errors.New("no running systemd or OpenRC service manager detected")
}

func detectServiceManager() (serviceManager, error) {
	_, systemctlErr := exec.LookPath("systemctl")
	_, openRCErr := exec.LookPath("rc-service")
	return selectServiceManager(isDir("/run/systemd/system") && systemctlErr == nil,
		isDir("/run/openrc") && openRCErr == nil)
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func privilegedCommand(name string, args ...string) error {
	if os.Geteuid() == 0 {
		return runCommand(name, args...)
	}
	return runCommand("sudo", append([]string{name}, args...)...)
}

func controlService(action string, rest ...string) error {
	manager, err := detectServiceManager()
	if err != nil {
		return err
	}
	return dispatchService(manager, action, rest, privilegedCommand)
}

func dispatchService(manager serviceManager, action string, rest []string, run func(string, ...string) error) error {
	if manager == systemd {
		if action == "logs" {
			if len(rest) == 0 {
				rest = []string{"-f"}
			}
			return run("journalctl", append([]string{"-u", serviceName}, rest...)...)
		}
		args := []string{action, serviceName}
		if action == "status" {
			args = append(args, "--no-pager")
		}
		return run("systemctl", append(args, rest...)...)
	}
	if action == "logs" {
		args, err := openRCLogArgs(rest)
		if err != nil {
			return err
		}
		return run("tail", args...)
	}
	if len(rest) != 0 {
		return errors.New("extra service options are not supported by OpenRC")
	}
	switch action {
	case "enable":
		return run("rc-update", "add", openRCService, "default")
	case "disable":
		return run("rc-update", "del", openRCService, "default")
	case "status", "start", "stop", "restart":
		if err := run("rc-service", openRCService, action); err != nil {
			return err
		}
		if action == "status" && !openRCNodeRunning() {
			return errors.New("OpenRC supervisor exists but node process is not running")
		}
		return nil
	default:
		return fmt.Errorf("unknown service action: %s", action)
	}
}

func openRCLogArgs(rest []string) ([]string, error) {
	lines, follow := "80", len(rest) == 0
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "-f", "--follow":
			follow = true
		case "--no-pager": // tail does not invoke a pager.
		case "-n", "--lines":
			i++
			if i >= len(rest) {
				return nil, errors.New("log line count is missing")
			}
			n, err := strconv.Atoi(rest[i])
			if err != nil || n < 0 {
				return nil, errors.New("invalid log line count")
			}
			lines = rest[i]
		default:
			return nil, fmt.Errorf("unsupported OpenRC log option: %s", rest[i])
		}
	}
	args := []string{"-n", lines}
	if follow {
		args = append(args, "-F")
	}
	return append(args, "/var/log/xboard-node/node.log", "/var/log/xboard-node/error.log"), nil
}

func supervisedNodeRunning(procRoot, pidPath, binary string) bool {
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return false
	}
	pid := strings.TrimSpace(string(data))
	if n, err := strconv.Atoi(pid); err != nil || n <= 0 {
		return false
	}
	children, err := os.ReadFile(filepath.Join(procRoot, pid, "task", pid, "children"))
	if err != nil {
		return false
	}
	for _, child := range strings.Fields(string(children)) {
		if n, err := strconv.Atoi(child); err != nil || n <= 0 {
			continue
		}
		path, err := os.Readlink(filepath.Join(procRoot, child, "exe"))
		if err == nil && path == binary {
			return true
		}
	}
	return false
}

func openRCNodeRunning() bool {
	return supervisedNodeRunning("/proc", "/run/xboard-node.pid", defaultBinaryPath)
}

func serviceState() string {
	manager, err := detectServiceManager()
	if err != nil {
		return "unknown"
	}
	if manager == systemd {
		out, _ := exec.Command("systemctl", "is-active", serviceName).Output()
		if state := strings.TrimSpace(string(out)); state != "" {
			return state
		}
		return "unknown"
	}
	err = exec.Command("rc-service", openRCService, "status").Run()
	if err == nil {
		if openRCNodeRunning() {
			return "active"
		}
		return "activating"
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 3 {
		return "inactive"
	}
	return "failed"
}

func runAlpineInstaller(action string, args []string) error {
	if !fileExists(alpineInstallerPath) {
		return errors.New("Alpine installer missing; run the project's install-alpine.sh once to install unified xbctl management")
	}
	cmd := exec.Command("sh", append([]string{alpineInstallerPath, action}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = os.Environ()
	if os.Getenv("XBOARD_NODE_REPOSITORY") == "" && releaseRepository != "" {
		cmd.Env = append(cmd.Env, "XBOARD_NODE_REPOSITORY="+releaseRepository)
	}
	return cmd.Run()
}
