package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/porthopping"
	"gopkg.in/yaml.v3"
)

const (
	defaultConfigPath      = "/etc/xboard-node/config.yml"
	defaultMetaPath        = "/etc/xboard-node/install-meta.json"
	defaultCredentialsPath = "/etc/xboard-node/credentials.env"
	defaultBinaryPath      = "/usr/local/bin/xboard-node"
	defaultCLIPath         = "/usr/local/bin/xbctl"
	serviceName            = "xboard-node.service"
	serviceFilePath        = "/etc/systemd/system/xboard-node.service"
	defaultInstallRoot     = "/etc/xboard-node"
)

var (
	version   = "dev"
	buildTime = "unknown"
	// Set by release builds. Local builds require XBOARD_NODE_REPOSITORY for upgrades.
	releaseRepository = ""
)

type instanceRow struct {
	ID      string `json:"id"`
	Mode    string `json:"mode"`
	Panel   string `json:"panel"`
	Target  string `json:"target"`
	Service string `json:"service"`
	Health  string `json:"health"`
}

type instanceSummary struct {
	ID         string `json:"id"`
	PanelURL   string `json:"panel_url"`
	Mode       string `json:"mode"`
	NodeID     *int   `json:"node_id"`
	MachineID  *int   `json:"machine_id"`
	HealthPort int    `json:"health_port"`
}

type installMeta struct {
	ConfigMode       string            `json:"config_mode"`
	Version          string            `json:"version"`
	LatestInstanceID string            `json:"latest_instance_id"`
	InstanceCount    int               `json:"instance_count"`
	Instances        []instanceSummary `json:"instances"`
	UpdatedAt        string            `json:"updated_at"`
}

func loadInstallMeta(path string) (*installMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	meta := &installMeta{}
	if err := json.Unmarshal(data, meta); err != nil {
		return nil, fmt.Errorf("parse install meta: %w", err)
	}
	return meta, nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}
	switch args[0] {
	case "status":
		return runStatus()
	case "list":
		return runList(args[1:])
	case "instance":
		return runInstance(args[1:])
	case "service":
		return runService(args[1:])
	case "logs", "log":
		return runService(append([]string{"logs"}, args[1:]...))
	case "health":
		return runHealth()
	case "bind":
		return runBind(args[1:])
	case "bind-node":
		return runBind(append([]string{"add-node"}, args[1:]...))
	case "bind-machine":
		return runBind(append([]string{"add-machine"}, args[1:]...))
	case "unbind-node":
		return runBind(append([]string{"remove-node"}, args[1:]...))
	case "unbind-machine":
		return runBind(append([]string{"remove-machine"}, args[1:]...))
	case "start", "stop", "restart", "enable", "disable":
		return runService(args)
	case "upgrade":
		return runUpgrade(args[1:])
	case "uninstall":
		return runUninstall(args[1:])
	case "version", "-v", "--version":
		fmt.Printf("xbctl %s (built %s)\n", version, buildTime)
		return nil
	case "config":
		return runConfig(args[1:])
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command: %s", args[0])
	}
}

func printUsage() {
	fmt.Println(`xbctl commands:
  xbctl help
  xbctl status
  xbctl list [--output text|json]
  xbctl instance list [--output text|json]
  xbctl instance get <id> [--output text|json]
  xbctl config init --mode node|machine --panel-url URL --token TOKEN [flags]
  xbctl config health-port [--config PATH]
  xbctl service status|start|stop|restart|enable|disable|logs|backend
  xbctl health
  xbctl bind add-node --panel-url URL --token TOKEN --node-id ID [--node-type TYPE] [--kernel singbox|xray]
  xbctl bind add-machine --panel-url URL --token TOKEN --machine-id ID [--kernel singbox|xray]
  xbctl bind remove <instance-id>
  xbctl bind remove-node --panel URL --node-id ID
  xbctl bind remove-machine --panel URL --machine-id ID
  xbctl upgrade [--version VERSION]
  xbctl uninstall [--purge] [--yes]
  xbctl version

shortcuts:
  xbctl start|stop|restart        = xbctl service start|stop|restart
  xbctl log|logs                  = xbctl service logs
  xbctl bind-node ...             = xbctl bind add-node ...
  xbctl bind-machine ...          = xbctl bind add-machine ...
  xbctl unbind-node ...           = xbctl bind remove-node ...
  xbctl unbind-machine ...        = xbctl bind remove-machine ...`)
}

func runStatus() error {
	fmt.Println("xboard-node status")
	fmt.Println()

	// Version from install-meta.json
	ver := "unknown"
	if meta, err := loadInstallMeta(defaultMetaPath); err == nil {
		ver = meta.Version
	}
	fmt.Printf("  version:  %s\n", ver)

	// Service status
	svc := serviceState()
	fmt.Printf("  service:  %s\n", svc)

	// Health
	health := instanceAwareHealth()
	fmt.Printf("  health:   %s\n", health)
	fmt.Println()

	// Instance list
	rows, err := collectInstanceRows()
	if err != nil {
		fmt.Printf("  (no instances found: %v)\n", err)
		return nil
	}
	return printRows(rows, "text")
}

func runList(args []string) error {
	output := parseOutput(args)
	rows, err := collectInstanceRows()
	if err != nil {
		return err
	}
	return printRows(rows, output)
}

func runInstance(args []string) error {
	if len(args) == 0 {
		return runList(nil)
	}
	if args[0] == "list" {
		return runList(args[1:])
	}
	if args[0] != "get" {
		return fmt.Errorf("unknown instance command: %s", args[0])
	}
	if len(args) < 2 {
		return errors.New("usage: xbctl instance get <id> [--output text|json]")
	}
	id := args[1]
	output := parseOutput(args[2:])
	rows, err := collectInstanceRows()
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.ID == id {
			return printRows([]instanceRow{row}, output)
		}
	}
	return fmt.Errorf("instance not found: %s", id)
}

func runService(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: xbctl service <status|start|stop|restart|enable|disable|logs>")
	}
	switch args[0] {
	case "backend":
		manager, err := detectServiceManager()
		if err != nil {
			return err
		}
		fmt.Println(manager)
		return nil
	case "status", "start", "stop", "restart", "enable", "disable", "logs":
		return controlService(args[0], args[1:]...)
	default:
		return fmt.Errorf("unknown service command: %s", args[0])
	}
}

func runHealth() error {
	h := instanceAwareHealth()
	fmt.Println(h)
	if h == "down" {
		return errors.New("health check failed")
	}
	return nil
}

func runBind(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: xbctl bind <add-node|add-machine|remove-node|remove-machine> ...")
	}
	if err := ensureRoot("bind"); err != nil {
		return err
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "add-node":
		return runBindAdd("node", rest)
	case "add-machine":
		return runBindAdd("machine", rest)
	case "remove-node":
		panel, nodeID, err := parseRemoveNodeArgs(rest)
		if err != nil {
			return err
		}
		return removeBinding(panel, nodeID, 0, "")
	case "remove-machine":
		panel, machineID, err := parseRemoveMachineArgs(rest)
		if err != nil {
			return err
		}
		return removeBinding(panel, 0, machineID, "")
	case "remove":
		if len(rest) == 0 {
			return errors.New("usage: xbctl bind remove <instance-id>")
		}
		return removeBinding("", 0, 0, rest[0])
	default:
		return fmt.Errorf("unknown bind command: %s", sub)
	}
}

func runBindAdd(mode string, args []string) error {
	// Build configInit args from bind args
	initArgs := []string{
		"--mode", mode,
		"--config", defaultConfigPath,
		"--output", defaultConfigPath,
		"--credentials-in", defaultCredentialsPath,
		"--credentials-out", defaultCredentialsPath,
		"--meta", defaultMetaPath,
		"--install-root", defaultInstallRoot,
	}
	// Pass through remaining args (--panel-url, --token, --node-id, --machine-id, --kernel, etc.)
	initArgs = append(initArgs, args...)

	if err := runConfigInit(initArgs); err != nil {
		return fmt.Errorf("bind failed: %w", err)
	}
	// Restart service to pick up new config
	fmt.Println("Restarting service...")
	if err := controlService("restart"); err != nil {
		return fmt.Errorf("service restart failed: %w", err)
	}
	fmt.Println("Binding added successfully")
	return nil
}

func runUpgrade(args []string) error {
	if err := ensureRoot("upgrade"); err != nil {
		return err
	}
	manager, err := detectServiceManager()
	if err != nil {
		return err
	}
	if manager == openRC {
		return runAlpineInstaller("upgrade", args)
	}

	requested := "latest"
	if len(args) == 2 && args[0] == "--version" {
		requested = args[1]
	} else if len(args) != 0 {
		return errors.New("usage: xbctl upgrade [--version VERSION]")
	}
	base, err := releaseBaseURL()
	if err != nil {
		return err
	}
	var healthPort int
	var previousState string
	prepare := func() error {
		out, err := exec.Command("systemctl", "show", "--property=WorkingDirectory", "--value", serviceName).Output()
		if err != nil {
			return fmt.Errorf("read service working directory: %w", err)
		}
		serviceDirectory := strings.TrimSpace(string(out))
		if serviceDirectory == "" {
			serviceDirectory = "/"
		}
		port, err := deploymentPort(defaultConfigPath, defaultCredentialsPath, serviceDirectory, defaultConfigPath)
		if err != nil {
			return err
		}
		healthPort = port
		previousState, err = serviceActiveState()
		if err != nil {
			return err
		}
		switch previousState {
		case "active", "inactive", "failed":
		default:
			return fmt.Errorf("service state is changing (%s), retry when settled", previousState)
		}
		return nil
	}
	installedVersion := ""
	installerPath := filepath.Join(defaultInstallRoot, "install.sh")
	record := func(tag string, installer []byte) error {
		root, err := loadWritableRootConfig(defaultConfigPath)
		if err != nil {
			return err
		}
		meta, err := installMetaBytes(root, tag, "")
		if err != nil {
			return err
		}
		files := []bindingFile{{path: defaultMetaPath, data: meta, mode: 0644}}
		if installer != nil {
			files = append(files, bindingFile{path: installerPath, data: installer, mode: 0755})
		}
		if err := commitBindingFiles(files); err != nil {
			return fmt.Errorf("record installed release: %w", err)
		}
		installedVersion = tag
		if _, err := os.Lstat("/usr/bin/xbctl"); os.IsNotExist(err) {
			if err := os.Symlink(defaultCLIPath, "/usr/bin/xbctl"); err != nil {
				fmt.Printf("Warning: could not create xbctl symlink: %v\n", err)
			}
		}
		return nil
	}
	if err := upgradeRelease(nil, base, requested, runtime.GOARCH, filepath.Dir(defaultBinaryPath), installerPath, prepare, record, func(rollback bool) error {
		if rollback && previousState != "active" {
			return restoreStoppedService(previousState)
		}
		if !rollback && healthPort < 0 {
			if previousState == "active" {
				return runCommand("systemctl", "stop", serviceName)
			}
			return nil
		}
		if err := runCommand("systemctl", "daemon-reload"); err != nil {
			return err
		}
		if err := runCommand("systemctl", "restart", serviceName); err != nil {
			return err
		}
		return observeInstalledStart(healthPort)
	}); err != nil {
		return err
	}
	fmt.Printf("Upgrade complete (version: %s)\n", installedVersion)
	return nil
}

func runUninstall(args []string) error {
	if err := ensureRoot("uninstall"); err != nil {
		return err
	}

	purge := false
	yes := false
	for _, a := range args {
		switch a {
		case "--purge":
			purge = true
		case "--yes", "-y":
			yes = true
		}
	}

	if !yes {
		fmt.Print("Proceed with uninstall? [y/N]: ")
		var answer string
		fmt.Scanln(&answer)
		if answer != "y" && answer != "Y" {
			fmt.Println("Uninstall cancelled")
			return nil
		}
	}
	manager, err := detectServiceManager()
	if err != nil {
		return err
	}
	if manager == openRC {
		args := []string{"--yes"}
		if purge {
			args = append(args, "--purge")
		}
		return runAlpineInstaller("uninstall", args)
	}

	var warnings []string

	// Stop and disable service
	if fileExists(serviceFilePath) {
		if err := runCommand("systemctl", "stop", serviceName); err != nil {
			warnings = append(warnings, fmt.Sprintf("stop service: %v", err))
		}
		if err := runCommand("systemctl", "disable", serviceName); err != nil {
			warnings = append(warnings, fmt.Sprintf("disable service: %v", err))
		}
	}

	if err := porthopping.CleanupStopped(); err != nil {
		return fmt.Errorf("clean HY2 redirects before uninstall (binaries preserved): %w", err)
	}
	if fileExists(serviceFilePath) {
		if err := os.Remove(serviceFilePath); err != nil {
			warnings = append(warnings, fmt.Sprintf("remove service file: %v", err))
		}
		runCommand("systemctl", "daemon-reload")
	}
	// Remove binaries
	// Remove binaries and symlinks
	for _, p := range []string{defaultBinaryPath, defaultCLIPath, "/usr/bin/xbctl"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			warnings = append(warnings, fmt.Sprintf("remove %s: %v", p, err))
		}
	}

	if purge {
		if err := os.RemoveAll(defaultInstallRoot); err != nil {
			warnings = append(warnings, fmt.Sprintf("remove %s: %v", defaultInstallRoot, err))
		} else {
			fmt.Printf("Removed %s\n", defaultInstallRoot)
		}
	} else {
		os.Remove(defaultMetaPath)
		fmt.Printf("Config preserved under %s\n", defaultInstallRoot)
	}

	if len(warnings) > 0 {
		fmt.Println("Uninstall completed with warnings:")
		for _, w := range warnings {
			fmt.Printf("  - %s\n", w)
		}
		return nil
	}

	fmt.Println("Uninstall complete")
	return nil
}

func ensureRoot(cmd string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("%s requires root privileges; run with sudo", cmd)
	}
	return nil
}

func releaseBaseURL() (string, error) {
	repository := strings.TrimSpace(os.Getenv("XBOARD_NODE_REPOSITORY"))
	if repository == "" {
		repository = releaseRepository
	}
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9_.-]+$`).MatchString(repository) ||
		strings.HasSuffix(repository, "/.") || strings.HasSuffix(repository, "/..") {
		return "", errors.New("set XBOARD_NODE_REPOSITORY to your GitHub owner/repository, or use a release build with an embedded repository")
	}
	return "https://github.com/" + repository + "/releases", nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func binaryVersion(output string) string {
	for _, line := range strings.Split(output, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "xboard-node" {
			return f[1]
		}
	}
	return "unknown"
}

func copyFile(src, dst string) error {

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(dst), ".xbctl-backup-*")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Chmod(0755)
	}
	if err == nil {
		err = out.Sync()
	}
	if err = errors.Join(err, out.Close()); err != nil {
		return err
	}
	return os.Rename(out.Name(), dst)
}

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func parseOutput(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "--output" && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(args[i], "--output=") {
			return strings.TrimPrefix(args[i], "--output=")
		}
	}
	return "text"
}

func parseRemoveNodeArgs(args []string) (string, int, error) {
	var panel string
	var nodeID int
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--panel", "-a", "--api":
			if i+1 >= len(args) {
				return "", 0, errors.New("missing value for --panel")
			}
			panel = args[i+1]
			i++
		case "--node-id", "-n":
			if i+1 >= len(args) {
				return "", 0, errors.New("missing value for --node-id")
			}
			var err error
			nodeID, err = parsePositiveInt(args[i+1], "node-id")
			if err != nil {
				return "", 0, err
			}
			i++
		default:
			return "", 0, fmt.Errorf("unknown remove-node arg: %s", args[i])
		}
	}
	if strings.TrimSpace(panel) == "" || nodeID <= 0 {
		return "", 0, errors.New("usage: xbctl bind remove-node --panel URL --node-id ID")
	}
	return strings.TrimSpace(panel), nodeID, nil
}

func parseRemoveMachineArgs(args []string) (string, int, error) {
	var panel string
	var machineID int
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--panel", "-a", "--api":
			if i+1 >= len(args) {
				return "", 0, errors.New("missing value for --panel")
			}
			panel = args[i+1]
			i++
		case "--machine-id":
			if i+1 >= len(args) {
				return "", 0, errors.New("missing value for --machine-id")
			}
			var err error
			machineID, err = parsePositiveInt(args[i+1], "machine-id")
			if err != nil {
				return "", 0, err
			}
			i++
		default:
			return "", 0, fmt.Errorf("unknown remove-machine arg: %s", args[i])
		}
	}
	if strings.TrimSpace(panel) == "" || machineID <= 0 {
		return "", 0, errors.New("usage: xbctl bind remove-machine --panel URL --machine-id ID")
	}
	return strings.TrimSpace(panel), machineID, nil
}

func parsePositiveInt(raw string, field string) (int, error) {
	var value int
	_, err := fmt.Sscanf(raw, "%d", &value)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("invalid %s: %s", field, raw)
	}
	return value, nil
}

func removeBinding(panelURL string, nodeID int, machineID int, instanceID string) error {
	root, err := loadWritableRootConfig(defaultConfigPath)
	if err != nil {
		return err
	}
	instances := normalizeRootInstances(root)
	if len(instances) == 0 {
		return errors.New("no instances configured")
	}

	kept := make([]config.Config, 0, len(instances))
	removed := make([]config.Config, 0, 1)
	for _, inst := range instances {
		var matched bool
		if instanceID != "" {
			// Match by instance ID
			id, _ := inst.AutoInstanceID()
			matched = id == instanceID || inst.InstanceID == instanceID
		} else {
			matched = strings.TrimSpace(inst.Panel.URL) == strings.TrimSpace(panelURL)
			if nodeID > 0 {
				matched = matched && !inst.IsMachineMode() && inst.Panel.NodeID == nodeID
			}
			if machineID > 0 {
				matched = matched && inst.IsMachineMode() && inst.Machine != nil && inst.Machine.MachineID == machineID
			}
		}
		if matched {
			removed = append(removed, inst)
			continue
		}
		kept = append(kept, inst)
	}
	if len(removed) == 0 {
		if instanceID != "" {
			return fmt.Errorf("binding not found: id=%s", instanceID)
		}
		if nodeID > 0 {
			return fmt.Errorf("binding not found: panel=%s node_id=%d", panelURL, nodeID)
		}
		return fmt.Errorf("binding not found: panel=%s machine_id=%d", panelURL, machineID)
	}

	root.Instances = kept
	if err := saveBindingRemoval(defaultConfigPath, defaultCredentialsPath, defaultMetaPath, root, removed); err != nil {
		return err
	}
	if len(kept) == 0 {
		if err := controlService("stop"); err != nil {
			return err
		}
		fmt.Println("All bindings removed. Service stopped.")
		return nil
	}
	if err := controlService("restart"); err != nil {
		return err
	}
	fmt.Printf("removed %d binding(s)\n", len(removed))
	return nil
}

func loadWritableRootConfig(path string) (*config.RootConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}
	rc := &config.RootConfig{}
	if err := yaml.Unmarshal(data, rc); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if len(rc.Instances) == 0 {
		legacy := &config.Config{}
		if err := yaml.Unmarshal(data, legacy); err != nil {
			return nil, fmt.Errorf("parse legacy config: %w", err)
		}
		rc.Config = *legacy
	}
	return rc, nil
}

func normalizeRootInstances(root *config.RootConfig) []config.Config {
	if root.Instances != nil {
		return append([]config.Config(nil), root.Instances...)
	}
	// Only treat legacy single-config mode if the embedded Config is valid
	if root.Config.Panel.URL != "" || root.Config.IsStandalone() {
		return []config.Config{root.Config}
	}
	return nil
}

func instanceMode(inst config.Config) string {
	if inst.IsStandalone() {
		return "standalone"
	}
	if inst.IsMachineMode() {
		return "machine"
	}
	return "node"
}

func collectInstanceRows() ([]instanceRow, error) {
	rows, err := collectRowsFromMeta()
	if err == nil && len(rows) > 0 {
		return rows, nil
	}
	return collectRowsFromConfig()
}

func collectRowsFromMeta() ([]instanceRow, error) {
	meta, err := loadInstallMeta(defaultMetaPath)
	if err != nil {
		return nil, err
	}
	serviceStatus := serviceState()
	healthStatus := healthStatus()
	rows := make([]instanceRow, 0, len(meta.Instances))
	for _, inst := range meta.Instances {
		rows = append(rows, instanceRow{
			ID:      inst.ID,
			Mode:    inst.Mode,
			Panel:   formatPanel(inst.Mode, inst.PanelURL),
			Target:  formatTargetForMode(inst.Mode, inst.NodeID, inst.MachineID),
			Service: serviceStatus,
			Health:  healthStatus,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows, nil
}

func collectRowsFromConfig() ([]instanceRow, error) {
	root, err := config.LoadRoot(defaultConfigPath)
	if err != nil {
		return nil, err
	}
	instances, err := root.NormalizeInstances()
	if err != nil {
		return nil, err
	}
	serviceStatus := serviceState()
	healthStatus := healthStatus()
	rows := make([]instanceRow, 0, len(instances))
	for _, inst := range instances {
		mode := instanceMode(*inst)
		rows = append(rows, instanceRow{
			ID:      inst.InstanceID,
			Mode:    mode,
			Panel:   formatPanel(mode, inst.Panel.URL),
			Target:  formatTargetForMode(mode, intPtr(inst.Panel.NodeID), machineIDPtr(inst)),
			Service: serviceStatus,
			Health:  healthStatus,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows, nil
}

func printRows(rows []instanceRow, output string) error {
	if output == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tMODE\tPANEL\tTARGET\tSERVICE\tHEALTH")
	for _, row := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", row.ID, row.Mode, row.Panel, row.Target, row.Service, row.Health)
	}
	tw.Flush()
	_, err := fmt.Print(buf.String())
	return err
}

func healthStatus() string {
	return instanceAwareHealth()
}

func instanceAwareHealth() string {
	port, err := deploymentPort(defaultConfigPath, defaultCredentialsPath, filepath.Dir(defaultConfigPath), defaultConfigPath)
	if err != nil {
		return "unknown"
	}
	if port <= 0 {
		return "disabled"
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "down"
	}
	resp.Body.Close()
	if resp.StatusCode == 200 {
		return "ok"
	}
	return "down"
}

func formatPanel(mode, panelURL string) string {
	if mode == "standalone" && panelURL == "" {
		return "-"
	}
	return panelURL
}

func formatTargetForMode(mode string, nodeID *int, machineID *int) string {
	if mode == "standalone" {
		return "local"
	}
	return formatTarget(nodeID, machineID)
}

func formatTarget(nodeID *int, machineID *int) string {
	if nodeID != nil && *nodeID > 0 {
		return fmt.Sprintf("node_id=%d", *nodeID)
	}
	if machineID != nil && *machineID > 0 {
		return fmt.Sprintf("machine_id=%d", *machineID)
	}
	return ""
}

func intPtr(v int) *int {
	if v <= 0 {
		return nil
	}
	vv := v
	return &vv
}

func latestInstanceID(instances []*config.Config) string {
	if len(instances) > 0 {
		id, _ := instances[len(instances)-1].AutoInstanceID()
		return id
	}
	return ""
}

func regenerateServiceFile() error {
	unit := fmt.Sprintf(`[Unit]
Description=Xboard Node Backend
Documentation=https://github.com/cedar2025/xboard-node
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=%s
EnvironmentFile=-%s
ExecStart=%s -c %s
ExecStopPost=%s --cleanup-port-hopping
Restart=always
RestartSec=5
LimitNOFILE=1048576
NoNewPrivileges=true
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
`, defaultInstallRoot, defaultCredentialsPath, defaultBinaryPath, defaultConfigPath, defaultBinaryPath)
	return os.WriteFile(serviceFilePath, []byte(unit), 0o644)
}

func machineIDPtr(cfg *config.Config) *int {
	if cfg.Machine == nil || cfg.Machine.MachineID <= 0 {
		return nil
	}
	vv := cfg.Machine.MachineID
	return &vv
}

// ── config subcommand ─────────────────────────────────────────────────

func runConfig(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: xbctl config <init|health-port>")
	}
	switch args[0] {
	case "init":
		return runConfigInit(args[1:])
	case "health-port", "deployment-port":
		return runConfigHealthPort(args[1:])
	case "metadata":
		return runConfigMetadata(args[1:])
	case "migrate-empty-v241":
		return runMigrateEmptyV241(args[1:])
	default:
		return fmt.Errorf("unknown config command: %s", args[0])
	}
}

// runConfigInit generates/merges an instance into config.yml, writes
// credentials.env and install-meta.json. It replaces the Python PY_INST,
// PY_CFG, PY_ENV, and PY_META heredoc blocks in install.sh.
//
// Output (stdout, one per line):
//
//	INSTANCE_ID=<generated-id>
//	ENV_KEY=<credential-env-var-name>
func runConfigInit(args []string) error {
	var (
		configIn       string
		configOut      string
		credentialsIn  string
		credentialsOut string
		metaPath       string
		mode           string
		panelURL       string
		nodeID         int
		nodeType       string
		machineID      int
		kernelType     string
		healthPort     int
		gomemlimit     string
		gogc           int
		installRoot    string
		token          string
		releaseVersion string
	)

	for i := 0; i < len(args); i++ {
		if i+1 >= len(args) {
			break
		}
		switch args[i] {
		case "--config":
			i++
			configIn = args[i]
		case "--output":
			i++
			configOut = args[i]
		case "--credentials-in":
			i++
			credentialsIn = args[i]
		case "--credentials-out":
			i++
			credentialsOut = args[i]
		case "--meta":
			i++
			metaPath = args[i]
		case "--mode":
			i++
			mode = args[i]
		case "--panel-url":
			i++
			panelURL = args[i]
		case "--node-id":
			i++
			v, err := strconv.Atoi(args[i])
			if err != nil {
				return fmt.Errorf("invalid --node-id: %w", err)
			}
			nodeID = v
		case "--node-type":
			i++
			nodeType = args[i]
		case "--machine-id":
			i++
			v, err := strconv.Atoi(args[i])
			if err != nil {
				return fmt.Errorf("invalid --machine-id: %w", err)
			}
			machineID = v
		case "--kernel":
			i++
			kernelType = args[i]
		case "--health-port":
			i++
			v, err := strconv.Atoi(args[i])
			if err != nil {
				return fmt.Errorf("invalid --health-port: %w", err)
			}
			healthPort = v
		case "--gomemlimit":
			i++
			gomemlimit = args[i]
		case "--gogc":
			i++
			v, err := strconv.Atoi(args[i])
			if err != nil {
				return fmt.Errorf("invalid --gogc: %w", err)
			}
			gogc = v
		case "--install-root":
			i++
			installRoot = args[i]
		case "--token":
			i++
			token = args[i]
		case "--version":
			i++
			releaseVersion = args[i]
		}
	}

	if mode == "" {
		return errors.New("--mode is required (node or machine)")
	}
	if panelURL == "" {
		return errors.New("--panel-url is required")
	}
	if mode == "node" && nodeID <= 0 {
		return errors.New("--node-id is required for node mode")
	}
	if mode == "machine" && machineID <= 0 {
		return errors.New("--machine-id is required for machine mode")
	}

	// Build the new instance.
	inst := config.Config{
		Panel: config.PanelConfig{URL: panelURL},
		Kernel: config.KernelConfig{
			Type:     kernelType,
			LogLevel: "warn",
		},
		Log:        config.LogConfig{Level: "info", Output: "stdout"},
		HealthPort: healthPort,
	}

	if mode == "machine" {
		inst.Machine = &config.MachineConfig{MachineID: machineID}
	} else {
		inst.Panel.NodeID = nodeID
		if nodeType != "" {
			inst.Panel.NodeType = nodeType
		}
	}

	// Generate deterministic instance ID.
	instanceID, err := inst.AutoInstanceID()
	if err != nil {
		return fmt.Errorf("generate instance ID: %w", err)
	}
	inst.InstanceID = instanceID

	if installRoot == "" {
		installRoot = "/etc/xboard-node"
	}
	inst.Kernel.ConfigDir = filepath.Join(installRoot, "instances", instanceID)

	// Build credential env key.
	envKey := "INSTANCE_" + strings.ToUpper(strings.ReplaceAll(instanceID, "-", "_"))
	if mode == "machine" {
		envKey += "_MACHINE_TOKEN"
		inst.Machine.TokenEnv = envKey
	} else {
		envKey += "_API_KEY"
		inst.Panel.TokenEnv = envKey
	}

	if gomemlimit != "" {
		inst.Runtime.GoMemLimit = gomemlimit
	}
	if gogc > 0 {
		inst.Runtime.GoGCPercent = gogc
	}

	// Load existing config (if any).
	if configIn == "" {
		configIn = configOut
	}
	root := &config.RootConfig{}
	hasExisting := false
	if configIn != "" {
		if loaded, loadErr := loadWritableRootConfig(configIn); loadErr == nil {
			root = loaded
			hasExisting = len(root.Instances) > 0 || root.Config.Panel.URL != "" || root.Config.Kernel.Type != "" || root.Config.IsStandalone()
		} else if !errors.Is(loadErr, os.ErrNotExist) {
			return loadErr
		}
	}

	// Normalise existing instance IDs so dedup works correctly.
	var instances []config.Config
	if hasExisting {
		instances = normalizeRootInstances(root)
		seen := make(map[string]bool)
		deduped := make([]config.Config, 0, len(instances))
		for _, existing := range instances {
			autoID, idErr := existing.AutoInstanceID()
			if idErr == nil && autoID != "" {
				existing.InstanceID = autoID
			}
			id := existing.InstanceID
			if id != "" && seen[id] {
				continue
			}
			if id != "" {
				seen[id] = true
			}
			deduped = append(deduped, existing)
		}
		instances = deduped
	}

	// Merge: replace if same ID exists, otherwise append.
	replaced := false
	for i, existing := range instances {
		if existing.InstanceID == instanceID {
			instances[i] = inst
			replaced = true
			break
		}
	}
	if !replaced {
		instances = append(instances, inst)
	}
	root.Instances = instances

	// Write config.
	if configOut == "" {
		configOut = configIn
	}
	if configOut == "" {
		return errors.New("--output (or --config) is required")
	}

	if credentialsIn == "" {
		credentialsIn = credentialsOut
	}
	if configIn == "" {
		configIn = configOut
	}
	contents, err := bindingConfig(configIn, root, &inst)
	if err != nil {
		return err
	}
	files := []bindingFile{{path: configOut, data: contents, mode: 0600}}
	if token != "" && credentialsOut != "" {
		contents, err := editCredentials(credentialsIn, map[string]string{envKey: token}, nil)
		if err != nil {
			return err
		}
		files = append(files, bindingFile{path: credentialsOut, data: contents, mode: 0600})
	}
	if metaPath != "" {
		data, err := installMetaBytes(root, releaseVersion, instanceID)
		if err != nil {
			return err
		}
		files = append(files, bindingFile{path: metaPath, data: data, mode: 0644})
	}
	if err := commitBindingFiles(files); err != nil {
		return err
	}
	// Output for bash capture.
	fmt.Printf("INSTANCE_ID=%s\n", instanceID)
	fmt.Printf("ENV_KEY=%s\n", envKey)
	return nil
}

// writeInstallMetaVersioned writes install-meta.json with an explicit
// version and latest-instance-ID. When latestID is empty, the last
// instance in the config is used (backward compat with writeInstallMeta).
func writeInstallMetaVersioned(path string, root *config.RootConfig, ver, latestID string) error {
	data, err := installMetaBytes(root, ver, latestID)
	if err != nil {
		return err
	}
	return commitBindingFiles([]bindingFile{{path: path, data: data, mode: 0644}})
}

func installMetaBytes(root *config.RootConfig, ver, latestID string) ([]byte, error) {
	if ver == "" {
		ver = "unknown"
	}
	instances := normalizeRootInstances(root)
	items := make([]instanceSummary, 0, len(instances))
	chooseLast := latestID == ""
	for _, inst := range instances {
		id, err := inst.AutoInstanceID()
		if err != nil {
			return nil, err
		}
		if chooseLast {
			latestID = id
		}
		item := instanceSummary{
			ID:         id,
			PanelURL:   inst.Panel.URL,
			Mode:       instanceMode(inst),
			NodeID:     intPtr(inst.Panel.NodeID),
			MachineID:  machineIDPtr(&inst),
			HealthPort: inst.HealthPort,
		}
		if item.Mode == "machine" || item.Mode == "standalone" {
			item.NodeID = nil
		}
		if item.Mode == "standalone" {
			item.PanelURL = ""
			item.MachineID = nil
		}
		items = append(items, item)
	}
	meta := installMeta{
		ConfigMode:       "instances",
		Version:          ver,
		LatestInstanceID: latestID,
		InstanceCount:    len(items),
		Instances:        items,
		UpdatedAt:        time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal install meta: %w", err)
	}
	data = append(data, '\n')
	return data, nil
}
