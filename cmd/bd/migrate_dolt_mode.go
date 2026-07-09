package main

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/beads"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/doltserver"
	"github.com/steveyegge/beads/internal/metrics"
	"github.com/steveyegge/beads/internal/storage/dbproxy/proxy"
	"github.com/steveyegge/beads/internal/ui"
)

var migrateToProxiedServerCmd = &cobra.Command{
	Use:           "from-server-to-proxied-server",
	Short:         "[EXPERIMENTAL] Switch a server-mode repo to proxied-server mode",
	SilenceUsage:  true,
	SilenceErrors: true,
	Long: `Switch a repo from server mode (bd init --server) to proxied-server mode.

Both modes root their dolt sql-server at the same .beads/dolt directory, so this
only rewrites .beads/metadata.json (dolt_mode) and writes the proxied-server
sidecar — no Dolt data is copied or moved. Stop the running server first with
'bd dolt stop'.

Note: dolt_mode lives in the committed metadata.json, so this change propagates
to clones on the next push.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		evt := metrics.NewCommandEvent("migrate-to-proxied-server")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		dryRun, _ := cmd.Flags().GetBool("dry-run")
		if !dryRun {
			CheckReadonly("migrate from-server-to-proxied-server")
		}

		idleTimeout, err := resolveMigrateIdleTimeout(cmd)
		if err != nil {
			return err
		}
		return runMigrateToProxiedServer(dryRun, idleTimeout)
	},
}

var migrateToServerCmd = &cobra.Command{
	Use:           "from-proxied-server-to-server",
	Short:         "[EXPERIMENTAL] Switch a proxied-server repo to server mode",
	SilenceUsage:  true,
	SilenceErrors: true,
	Long: `Switch a repo from proxied-server mode to server mode (bd init --server).

Both modes root their dolt sql-server at the same .beads/dolt directory, so this
only rewrites .beads/metadata.json (dolt_mode) and removes the proxied-server
sidecar — no Dolt data is copied or moved. Stop the running proxy first with
'bd dolt stop'.

Note: dolt_mode lives in the committed metadata.json, so this change propagates
to clones on the next push.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		evt := metrics.NewCommandEvent("migrate-to-server")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		dryRun, _ := cmd.Flags().GetBool("dry-run")
		if !dryRun {
			CheckReadonly("migrate from-proxied-server-to-server")
		}
		return runMigrateToServer(dryRun)
	},
}

func resolveMigrateIdleTimeout(cmd *cobra.Command) (time.Duration, error) {
	if !cmd.Flags().Changed("idle-timeout") {
		return 0, nil
	}
	v, _ := cmd.Flags().GetDuration("idle-timeout")
	if v < 0 {
		return 0, HandleError("--idle-timeout must be 0 (never) or a positive duration, got %s", v)
	}
	if v == 0 {
		return proxy.IdleTimeoutNever, nil
	}
	return v, nil
}

func loadMigrateModeConfig() (string, *configfile.Config, error) {
	beadsDir := beads.FindBeadsDir()
	if beadsDir == "" {
		return "", nil, HandleErrorWithHint(activeWorkspaceNotFoundError(), diagHint())
	}
	cfg, err := configfile.Load(beadsDir)
	if err != nil {
		return "", nil, HandleError("failed to load config: %v", err)
	}
	if cfg == nil {
		return "", nil, HandleError("no beads database found in %s — run 'bd init' first", beadsDir)
	}
	return beadsDir, cfg, nil
}

func runMigrateToProxiedServer(dryRun bool, idleTimeout time.Duration) error {
	beadsDir, cfg, err := loadMigrateModeConfig()
	if err != nil {
		return err
	}
	if cfg.IsDoltProxiedServerMode() {
		fmt.Printf("%s\n", ui.RenderPass("✓ Already in proxied-server mode"))
		return nil
	}
	if !cfg.IsDoltServerMode() {
		return HandleError("repo is not in server mode (dolt_mode=%q); this command only migrates server-mode repos", cfg.GetDoltMode())
	}

	serverDir := doltserver.ResolveServerDir(beadsDir)
	if state, _ := doltserver.IsRunning(serverDir); state != nil && state.Running {
		return HandleErrorWithHint("dolt server is still running", "stop it first: bd dolt stop")
	}

	if dryRun {
		fmt.Println("Dry run mode - no changes will be made")
		fmt.Printf("Would set dolt_mode: %s → %s\n", configfile.DoltModeServer, configfile.DoltModeProxiedServer)
		fmt.Printf("Would write %s\n", configfile.ProxiedServerClientInfoFileName)
		return nil
	}

	cfg.DoltMode = configfile.DoltModeProxiedServer
	if err := cfg.Save(beadsDir); err != nil {
		return HandleError("failed to save metadata.json: %v", err)
	}

	info := &configfile.ProxiedServerClientInfo{IdleTimeout: idleTimeout}
	if err := configfile.SaveProxiedServerClientInfo(beadsDir, info); err != nil {
		return HandleError("failed to write %s: %v", configfile.ProxiedServerClientInfoFileName, err)
	}

	commandDidWrite.Store(true)
	fmt.Printf("%s\n\n", ui.RenderPass("✓ Switched to proxied-server mode"))
	fmt.Printf("  Data directory unchanged: %s\n", proxiedServerRoot(beadsDir))
	fmt.Println("  The proxy starts automatically on the next bd command.")
	return nil
}

func runMigrateToServer(dryRun bool) error {
	beadsDir, cfg, err := loadMigrateModeConfig()
	if err != nil {
		return err
	}
	if cfg.IsDoltServerMode() {
		fmt.Printf("%s\n", ui.RenderPass("✓ Already in server mode"))
		return nil
	}
	if !cfg.IsDoltProxiedServerMode() {
		return HandleError("repo is not in proxied-server mode (dolt_mode=%q); this command only migrates proxied-server repos", cfg.GetDoltMode())
	}

	rootDir, err := resolveProxiedServerRootPath(beadsDir)
	if err != nil {
		return HandleError("%v", err)
	}
	if running, _ := proxy.IsRunning(rootDir); running {
		return HandleErrorWithHint("proxied-server is still running", "stop it first: bd dolt stop")
	}

	if dryRun {
		fmt.Println("Dry run mode - no changes will be made")
		fmt.Printf("Would set dolt_mode: %s → %s\n", configfile.DoltModeProxiedServer, configfile.DoltModeServer)
		fmt.Printf("Would remove %s\n", configfile.ProxiedServerClientInfoFileName)
		return nil
	}

	if err := doltserver.MarkDoltDirCompatible(rootDir); err != nil {
		return HandleError("failed to mark dolt directory compatible: %v", err)
	}

	cfg.DoltMode = configfile.DoltModeServer
	if err := cfg.Save(beadsDir); err != nil {
		return HandleError("failed to save metadata.json: %v", err)
	}

	if err := os.Remove(configfile.ProxiedServerClientInfoPath(beadsDir)); err != nil && !os.IsNotExist(err) {
		return HandleError("failed to remove %s: %v", configfile.ProxiedServerClientInfoFileName, err)
	}

	commandDidWrite.Store(true)
	fmt.Printf("%s\n\n", ui.RenderPass("✓ Switched to server mode"))
	fmt.Printf("  Data directory unchanged: %s\n", rootDir)
	fmt.Println("  The dolt sql-server starts automatically on the next bd command.")
	return nil
}
