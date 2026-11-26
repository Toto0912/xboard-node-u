package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/machine"
	"github.com/cedar2025/xboard-node/internal/nlog"
	"github.com/cedar2025/xboard-node/internal/porthopping"
	"github.com/cedar2025/xboard-node/internal/service"
	C "github.com/sagernet/sing-box/constant"
	xcore "github.com/xtls/xray-core/core"
)

var (
	version   = "dev"
	buildTime = "unknown"
)

func main() {
	configPath := flag.String("c", "config.yml", "config file path")
	showVersion := flag.Bool("v", false, "show version")
	cleanupRedirects := flag.Bool("cleanup-port-hopping", false, "clean port redirects owned by stopped xboard-node processes")
	requireStopped := flag.Bool("require-stopped", false, "with cleanup-port-hopping, refuse uninstall while an owner is running")
	flag.Parse()
	if *cleanupRedirects {
		cleanup := porthopping.CleanupStale
		if *requireStopped {
			cleanup = porthopping.CleanupStopped
		}
		if err := cleanup(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if *showVersion {
		fmt.Printf("xboard-node %s (built %s)\n", version, buildTime)
		fmt.Printf("sing-box %s (Xboard compatibility patch)\n", C.Version)
		fmt.Printf("Xray %s (Xboard compatibility patch)\n", xcore.Version())
		os.Exit(0)
	}

	rootCfg, err := config.LoadRoot(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	instances, err := rootCfg.NormalizeInstances()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to normalize config: %v\n", err)
		os.Exit(1)
	}
	if len(instances) == 0 {
		fmt.Println("No node bindings configured.")
		return
	}
	config.InitLogger(instances[0].Log)
	if err := porthopping.CleanupStale(); err != nil {
		nlog.Core().Warn("could not recover stale HY2 redirects", "error", err)
	}

	// Apply runtime memory tuning before anything else allocates.
	applyRuntimeConfig(instances[0].Runtime)

	runWithReload(rootCfg, *configPath)
}

// runWithReload restarts all node services when the config file changes.
func runWithReload(initialRoot *config.RootConfig, configPath string) {
	var healthSrv *http.Server
	var healthPort int
	switchHealth := func(port int) error {
		if healthPort == port {
			return nil
		}
		if port <= 0 {
			if healthSrv != nil {
				_ = healthSrv.Close()
			}
			healthSrv = nil
			healthPort = 0
			return nil
		}
		ln, err := listenHealth(fmt.Sprintf(":%d", port))
		if err != nil {
			return err
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok"}`))
		})
		if healthSrv != nil {
			_ = healthSrv.Close()
		}
		healthSrv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		healthPort = port
		go func(server *http.Server) {
			nlog.Core().Debug(fmt.Sprintf("health check listening on :%d", port))
			if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
				nlog.Core().Warn("health check server stopped", "error", err)
			}
		}(healthSrv)
		return nil
	}

	initialInstances, err := initialRoot.NormalizeInstances()
	if err != nil {
		nlog.Core().Error("failed to normalize initial config", "error", err)
		os.Exit(1)
	}
	if len(initialInstances) == 0 {
		return
	}
	if err := switchHealth(initialInstances[0].HealthPort); err != nil {
		nlog.Core().Error("failed to open initial health listener", "error", err)
		os.Exit(1)
	}
	defer func() {
		if healthSrv != nil {
			healthSrv.Close()
		}
	}()

	// Keep one signal subscription for the entire process, including the time
	// an old generation spends draining after a configuration change.
	processCtx, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	finished := make(chan struct{})
	defer close(finished)
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case sig := <-sigCh:
			nlog.Core().Info("shutting down", "signal", sig)
			shutdown()
		case <-finished:
			return
		}
		select {
		case sig := <-sigCh:
			nlog.Core().Warn("received second signal, forcing exit", "signal", sig)
			os.Exit(1)
		case <-time.After(service.FinalReportTimeout + 15*time.Second):
			nlog.Core().Error("shutdown grace period expired, forcing exit")
			os.Exit(2)
		case <-finished:
		}
	}()
	type reloadCandidate struct {
		root       *config.RootConfig
		healthPort int
	}
	for root := initialRoot; processCtx.Err() == nil; {
		ctx, cancel := context.WithCancel(processCtx)
		reloadCh := make(chan reloadCandidate, 1)

		watcher, err := config.WatchConfigRoot(ctx, configPath, func(newCfg *config.RootConfig) {
			candidate, err := newCfg.NormalizeInstances()
			if err == nil {
				err = config.ValidateStartupLayout(candidate)
			}
			if err != nil {
				nlog.Core().Warn("invalid reload layout, keeping running services", "error", err)
				return
			}
			port := 0
			if len(candidate) > 0 {
				port = candidate[0].HealthPort
			}
			select {
			case reloadCh <- reloadCandidate{newCfg, port}:
			default:
			}
		})
		if err != nil {
			nlog.Core().Warn("config watcher unavailable, hot-reload disabled", "error", err)
		}

		instances, err := root.NormalizeInstances()
		if err != nil {
			nlog.Core().Error("failed to normalize config", "error", err)
			os.Exit(1)
		}
		if err := config.ValidateStartupLayout(instances); err != nil {
			nlog.Core().Error("startup layout validation failed", "error", err)
			os.Exit(1)
		}

		var firstErr error
		var failOnce sync.Once
		stopWithError := func(err error) {
			failOnce.Do(func() {
				firstErr = err
				cancel()
			})
		}
		doneCh := make(chan struct{})
		var wg sync.WaitGroup
		for _, instanceCfg := range instances {
			instanceCfg := instanceCfg
			wg.Add(1)
			go func() {
				defer wg.Done()
				if instanceCfg.IsMachineMode() {
					nlog.Core().Info("starting machine instance", "instance", instanceCfg.InstanceID, "machine_id", instanceCfg.Machine.MachineID, "panel_url", instanceCfg.Panel.URL)
					orch := machine.New(instanceCfg)
					if err := orch.Run(ctx); err != nil {
						nlog.Core().Error("machine instance exited with error", "instance", instanceCfg.InstanceID, "error", err)
						stopWithError(err)
					}
					return
				}
				nodes := instanceCfg.ExpandNodes()
				nlog.Core().Info("starting node instance", "instance", instanceCfg.InstanceID, "nodes", len(nodes), "panel_url", instanceCfg.Panel.URL)
				var instanceWG sync.WaitGroup
				for idx, nodeCfg := range nodes {
					nodeCfg := nodeCfg
					instanceWG.Add(1)
					go func(idx int) {
						defer instanceWG.Done()
						if idx > 0 {
							delay := time.Duration(idx) * 250 * time.Millisecond
							if delay > 2*time.Second {
								delay = 2 * time.Second
							}
							select {
							case <-time.After(delay):
							case <-ctx.Done():
								return
							}
						}
						svc := service.New(nodeCfg)
						if err := svc.Run(ctx); err != nil {
							nlog.Core().Error("node service exited with error", "instance", nodeCfg.InstanceID, "node_id", nodeCfg.Panel.NodeID, "error", err)
							stopWithError(err)
						}
					}(idx)
				}
				instanceWG.Wait()
			}()
		}
		go func() { wg.Wait(); close(doneCh) }()

		var newRoot *config.RootConfig
	waitForChange:
		for {
			select {
			case candidate := <-reloadCh:
				if err := switchHealth(candidate.healthPort); err != nil {
					nlog.Core().Warn("invalid reload health listener, keeping running services", "error", err)
					continue
				}
				newRoot = candidate.root
				nlog.Core().Info("config changed, restarting all services...")
				cancel()
				<-doneCh
			case <-doneCh:
			}
			break waitForChange
		}
		cancel()
		if watcher != nil {
			watcher.Stop()
		}

		if processCtx.Err() != nil {
			nlog.Core().Info("stopped")
			return
		}
		if newRoot == nil {
			if firstErr != nil {
				os.Exit(1)
			}
			nlog.Core().Info("stopped")
			return
		}

		newInstances, err := newRoot.NormalizeInstances()
		if err != nil {
			nlog.Core().Error("failed to normalize reloaded config", "error", err)
			os.Exit(1)
		}
		if len(newInstances) == 0 {
			nlog.Core().Info("all node bindings removed")
			return
		}
		config.InitLogger(newInstances[0].Log)
		applyRuntimeConfig(newInstances[0].Runtime)
		root = newRoot
		nlog.Core().Info("reload complete, services restarting with new config")
	}
}

// Keep the health endpoint on ordinary TCP, matching the embedded Xray default.
// Explicitly setting this also overrides Go's GODEBUG MPTCP listener default.
func listenHealth(address string) (net.Listener, error) {
	var lc net.ListenConfig
	lc.SetMultipathTCP(false)
	return lc.Listen(context.Background(), "tcp", address)
}

// applyRuntimeConfig wires up Go runtime memory limits from the config file.
// Both settings can also be overridden by environment variables (GOMEMLIMIT /
// GOGC) — the env vars take precedence because Go's runtime reads them before
// we can call these functions, but we set them here for completeness and so
// the values are logged.
func applyRuntimeConfig(rt config.RuntimeConfig) {
	// GOGC
	if rt.GoGCPercent > 0 {
		prev := debug.SetGCPercent(rt.GoGCPercent)
		nlog.Core().Info("runtime: GOGC set", "gogc", rt.GoGCPercent, "prev", prev)
	}

	// GOMEMLIMIT — parse human-readable size string (e.g. "30MiB")
	if rt.GoMemLimit != "" {
		limit, err := parseMemLimit(rt.GoMemLimit)
		if err != nil {
			nlog.Core().Warn("runtime: invalid gomemlimit, ignoring", "value", rt.GoMemLimit, "error", err)
		} else {
			prev := debug.SetMemoryLimit(limit)
			nlog.Core().Info("runtime: GOMEMLIMIT set",
				"limit", rt.GoMemLimit,
				"bytes", limit,
				"prev_bytes", prev,
			)
		}
	}
}

// parseMemLimit converts a human-readable size string to bytes.
// Accept bytes or decimal/binary size suffixes (case-insensitive).
func parseMemLimit(s string) (int64, error) {
	s = strings.TrimSpace(s)
	suffixes := []struct {
		suffix string
		mult   int64
	}{
		{"TiB", 1 << 40},
		{"GiB", 1 << 30},
		{"MiB", 1 << 20},
		{"KiB", 1 << 10},
		{"TB", 1_000_000_000_000}, {"GB", 1_000_000_000}, {"MB", 1_000_000}, {"KB", 1_000}, {"B", 1},
	}
	upper := strings.ToUpper(s)
	mult := int64(1)
	for _, sf := range suffixes {
		if num, ok := strings.CutSuffix(upper, strings.ToUpper(sf.suffix)); ok {
			upper, mult = strings.TrimSpace(num), sf.mult
			break
		}
	}
	n, err := strconv.ParseInt(upper, 10, 64)
	if err != nil || n < 0 || n > math.MaxInt64/mult {
		return 0, fmt.Errorf("invalid memory limit %q", s)
	}
	return n * mult, nil
}
