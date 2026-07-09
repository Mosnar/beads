package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storage/dbproxy/util"
)

func migrateModeWorkspace(t *testing.T, mode string) string {
	t.Helper()
	t.Setenv("BEADS_PROXIED_SERVER_ROOT_PATH", "")
	t.Setenv("BEADS_PROXIED_SERVER_CONFIG", "")
	t.Setenv("BEADS_PROXIED_SERVER_LOG", "")
	t.Setenv("BEADS_DOLT_DATA_DIR", "")

	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	require.NoError(t, os.MkdirAll(beadsDir, 0o755))
	writeMetadataConfig(t, beadsDir, mode, "myproj")
	t.Chdir(dir)
	return beadsDir
}

func touchFile(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
}

func serverAssetNames() []string {
	return []string{"dolt-server.pid", "dolt-server.port", "dolt-server.lock", "dolt-server.log", "dolt-server.log.1"}
}

func proxiedAssetNames() []string {
	return []string{"proxy.pid", "proxy.lock", "proxy.log", "proxy-child.pid", "proxy-child.lock", "server_config.yaml", "server.log"}
}

func TestMigrateToProxiedServer_FlipsMode(t *testing.T) {
	beadsDir := migrateModeWorkspace(t, configfile.DoltModeServer)

	for _, n := range serverAssetNames() {
		touchFile(t, filepath.Join(beadsDir, n))
	}
	touchFile(t, filepath.Join(beadsDir, "dolt-pprof", "cpu.pprof"))

	require.NoError(t, runMigrateToProxiedServer(false, 0))

	cfg, err := configfile.Load(beadsDir)
	require.NoError(t, err)
	assert.True(t, cfg.IsDoltProxiedServerMode())
	assert.Equal(t, "myproj", cfg.GetDoltDatabase())

	_, statErr := os.Stat(configfile.ProxiedServerClientInfoPath(beadsDir))
	require.NoError(t, statErr, "sidecar must be written")

	for _, n := range serverAssetNames() {
		_, err := os.Stat(filepath.Join(beadsDir, n))
		assert.True(t, os.IsNotExist(err), "server asset %s must be removed", n)
	}
	_, err = os.Stat(filepath.Join(beadsDir, "dolt-pprof"))
	assert.True(t, os.IsNotExist(err), "dolt-pprof/ must be removed")
}

func TestMigrateToProxiedServer_RejectsNonServerMode(t *testing.T) {
	migrateModeWorkspace(t, configfile.DoltModeEmbedded)
	err := runMigrateToProxiedServer(false, 0)
	require.Error(t, err)
}

func TestMigrateToProxiedServer_DryRunWritesNothing(t *testing.T) {
	beadsDir := migrateModeWorkspace(t, configfile.DoltModeServer)
	touchFile(t, filepath.Join(beadsDir, "dolt-server.log"))

	require.NoError(t, runMigrateToProxiedServer(true, 0))

	cfg, err := configfile.Load(beadsDir)
	require.NoError(t, err)
	assert.True(t, cfg.IsDoltServerMode(), "mode must be unchanged in dry-run")

	_, statErr := os.Stat(configfile.ProxiedServerClientInfoPath(beadsDir))
	assert.True(t, os.IsNotExist(statErr), "sidecar must not be written in dry-run")

	_, assetErr := os.Stat(filepath.Join(beadsDir, "dolt-server.log"))
	require.NoError(t, assetErr, "dry-run must not delete assets")
}

func TestMigrateToServer_FlipsModeAndRemovesSidecar(t *testing.T) {
	beadsDir := migrateModeWorkspace(t, configfile.DoltModeProxiedServer)
	require.NoError(t, configfile.SaveProxiedServerClientInfo(beadsDir, &configfile.ProxiedServerClientInfo{}))

	rootDir := filepath.Join(beadsDir, "dolt")
	require.NoError(t, os.MkdirAll(filepath.Join(rootDir, ".dolt"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(rootDir, "myproj", ".dolt"), 0o755))
	for _, n := range proxiedAssetNames() {
		touchFile(t, filepath.Join(rootDir, n))
	}

	require.NoError(t, runMigrateToServer(false))

	cfg, err := configfile.Load(beadsDir)
	require.NoError(t, err)
	assert.True(t, cfg.IsDoltServerMode())

	_, statErr := os.Stat(configfile.ProxiedServerClientInfoPath(beadsDir))
	assert.True(t, os.IsNotExist(statErr), "sidecar must be removed")

	_, markerErr := os.Stat(filepath.Join(rootDir, ".bd-dolt-ok"))
	require.NoError(t, markerErr, "compatibility marker must be written")

	for _, n := range proxiedAssetNames() {
		_, err := os.Stat(filepath.Join(rootDir, n))
		assert.True(t, os.IsNotExist(err), "proxied asset %s must be removed", n)
	}

	_, dotDoltErr := os.Stat(filepath.Join(rootDir, ".dolt"))
	require.NoError(t, dotDoltErr, "shared .dolt must be preserved")
	_, dbErr := os.Stat(filepath.Join(rootDir, "myproj", ".dolt"))
	require.NoError(t, dbErr, "database subdir must be preserved")
}

func TestMigrateToServer_KeepsCustomConfig(t *testing.T) {
	beadsDir := migrateModeWorkspace(t, configfile.DoltModeProxiedServer)
	customConfig := filepath.Join(t.TempDir(), "custom.yaml")
	touchFile(t, customConfig)
	require.NoError(t, configfile.SaveProxiedServerClientInfo(beadsDir, &configfile.ProxiedServerClientInfo{
		ConfigPath: customConfig,
	}))
	require.NoError(t, os.MkdirAll(filepath.Join(beadsDir, "dolt", ".dolt"), 0o755))

	require.NoError(t, runMigrateToServer(false))

	_, err := os.Stat(customConfig)
	require.NoError(t, err, "user-supplied config path must not be deleted")
}

func TestMigrateToServer_RejectsNonProxiedMode(t *testing.T) {
	migrateModeWorkspace(t, configfile.DoltModeEmbedded)
	err := runMigrateToServer(false)
	require.Error(t, err)
}

func TestMigrateMode_RefusesWhenLockHeld(t *testing.T) {
	beadsDir := migrateModeWorkspace(t, configfile.DoltModeServer)

	held, err := util.TryLock(filepath.Join(beadsDir, migrateLockFileName))
	require.NoError(t, err)

	require.Error(t, runMigrateToProxiedServer(false, 0), "must refuse while the lock is held")
	require.Error(t, runMigrateToServer(false), "must refuse while the lock is held")

	cfg, err := configfile.Load(beadsDir)
	require.NoError(t, err)
	assert.True(t, cfg.IsDoltServerMode(), "mode must be unchanged while blocked")

	held.Unlock()
	require.NoError(t, runMigrateToProxiedServer(false, 0), "must succeed once the lock is released")
}

func TestMigrateToServer_RefusesWhenLifecycleLockHeld(t *testing.T) {
	cases := []struct {
		name string
		rel  func(beadsDir string) string
	}{
		{"proxy.lock", func(b string) string { return filepath.Join(b, "dolt", "proxy.lock") }},
		{"proxy-child.lock", func(b string) string { return filepath.Join(b, "dolt", "proxy-child.lock") }},
		{"dolt-server.lock", func(b string) string { return filepath.Join(b, "dolt-server.lock") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			beadsDir := migrateModeWorkspace(t, configfile.DoltModeProxiedServer)
			require.NoError(t, configfile.SaveProxiedServerClientInfo(beadsDir, &configfile.ProxiedServerClientInfo{}))
			require.NoError(t, os.MkdirAll(filepath.Join(beadsDir, "dolt", ".dolt"), 0o755))

			held, err := util.TryLock(tc.rel(beadsDir))
			require.NoError(t, err)

			require.Error(t, runMigrateToServer(false), "must refuse while %s is held", tc.name)

			cfg, err := configfile.Load(beadsDir)
			require.NoError(t, err)
			assert.True(t, cfg.IsDoltProxiedServerMode(), "mode must be unchanged when blocked")

			held.Unlock()
			require.NoError(t, runMigrateToServer(false), "must succeed once %s is released", tc.name)

			free, err := util.TryLock(tc.rel(beadsDir))
			require.NoError(t, err, "%s must be released after a successful migration", tc.name)
			free.Unlock()
		})
	}
}

func TestMigrateMode_DryRunIgnoresLock(t *testing.T) {
	beadsDir := migrateModeWorkspace(t, configfile.DoltModeServer)
	held, err := util.TryLock(filepath.Join(beadsDir, migrateLockFileName))
	require.NoError(t, err)
	defer held.Unlock()

	require.NoError(t, runMigrateToProxiedServer(true, 0), "dry-run must not require the lock")
}

func TestMigrateMode_ReleasesLockAfterSuccess(t *testing.T) {
	beadsDir := migrateModeWorkspace(t, configfile.DoltModeServer)
	require.NoError(t, runMigrateToProxiedServer(false, 0))

	lock, err := util.TryLock(filepath.Join(beadsDir, migrateLockFileName))
	require.NoError(t, err, "lock must be released after the command completes")
	lock.Unlock()
}

func TestMigrateToProxiedServer_AlreadyProxiedIsNoop(t *testing.T) {
	beadsDir := migrateModeWorkspace(t, configfile.DoltModeProxiedServer)
	require.NoError(t, runMigrateToProxiedServer(false, 0))

	cfg, err := configfile.Load(beadsDir)
	require.NoError(t, err)
	assert.True(t, cfg.IsDoltProxiedServerMode())
}
