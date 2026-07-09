package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/beads/internal/configfile"
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

func TestMigrateToProxiedServer_FlipsMode(t *testing.T) {
	beadsDir := migrateModeWorkspace(t, configfile.DoltModeServer)

	require.NoError(t, runMigrateToProxiedServer(false, 0))

	cfg, err := configfile.Load(beadsDir)
	require.NoError(t, err)
	assert.True(t, cfg.IsDoltProxiedServerMode())
	assert.Equal(t, "myproj", cfg.GetDoltDatabase())

	_, statErr := os.Stat(configfile.ProxiedServerClientInfoPath(beadsDir))
	require.NoError(t, statErr, "sidecar must be written")
}

func TestMigrateToProxiedServer_RejectsNonServerMode(t *testing.T) {
	migrateModeWorkspace(t, configfile.DoltModeEmbedded)
	err := runMigrateToProxiedServer(false, 0)
	require.Error(t, err)
}

func TestMigrateToProxiedServer_DryRunWritesNothing(t *testing.T) {
	beadsDir := migrateModeWorkspace(t, configfile.DoltModeServer)

	require.NoError(t, runMigrateToProxiedServer(true, 0))

	cfg, err := configfile.Load(beadsDir)
	require.NoError(t, err)
	assert.True(t, cfg.IsDoltServerMode(), "mode must be unchanged in dry-run")

	_, statErr := os.Stat(configfile.ProxiedServerClientInfoPath(beadsDir))
	assert.True(t, os.IsNotExist(statErr), "sidecar must not be written in dry-run")
}

func TestMigrateToServer_FlipsModeAndRemovesSidecar(t *testing.T) {
	beadsDir := migrateModeWorkspace(t, configfile.DoltModeProxiedServer)
	require.NoError(t, configfile.SaveProxiedServerClientInfo(beadsDir, &configfile.ProxiedServerClientInfo{}))

	doltDir := filepath.Join(beadsDir, "dolt", ".dolt")
	require.NoError(t, os.MkdirAll(doltDir, 0o755))

	require.NoError(t, runMigrateToServer(false))

	cfg, err := configfile.Load(beadsDir)
	require.NoError(t, err)
	assert.True(t, cfg.IsDoltServerMode())

	_, statErr := os.Stat(configfile.ProxiedServerClientInfoPath(beadsDir))
	assert.True(t, os.IsNotExist(statErr), "sidecar must be removed")

	_, markerErr := os.Stat(filepath.Join(beadsDir, "dolt", ".bd-dolt-ok"))
	require.NoError(t, markerErr, "compatibility marker must be written")
}

func TestMigrateToServer_RejectsNonProxiedMode(t *testing.T) {
	migrateModeWorkspace(t, configfile.DoltModeEmbedded)
	err := runMigrateToServer(false)
	require.Error(t, err)
}

func TestMigrateToProxiedServer_AlreadyProxiedIsNoop(t *testing.T) {
	beadsDir := migrateModeWorkspace(t, configfile.DoltModeProxiedServer)
	require.NoError(t, runMigrateToProxiedServer(false, 0))

	cfg, err := configfile.Load(beadsDir)
	require.NoError(t, err)
	assert.True(t, cfg.IsDoltProxiedServerMode())
}
