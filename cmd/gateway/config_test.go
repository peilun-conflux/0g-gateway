package main

import (
	"os"
	"path/filepath"
	"testing"
)

// clearConfigEnv unsets every ZGS_* variable so a test starts from a known
// baseline regardless of the developer's shell or a sourced .env.
func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"ZGS_GW_LISTEN", "ZGS_GW_DATA_DIR", "ZGS_NODES", "ZGS_ETH_RPC",
		"ZGS_PRIVATE_KEY", "ZGS_GW_MAX_SIZE", "ZGS_GW_BATCH_MAX",
		"ZGS_GW_MAX_RETRIES", "ZGS_GW_FLUSH_INTERVAL_MS", "ZGS_EXPECTED_REPLICA",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigFile(t *testing.T) {
	clearConfigEnv(t)
	path := writeConfig(t, `
listen: "127.0.0.1:9000"
data_dir: /var/lib/zgs
nodes:
  - http://node-a:5678
  - http://node-b:5678
eth_rpc: https://rpc.example
private_key: deadbeef
batch_max: 7
expected_replica: 2
`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:9000" || cfg.DataDir != "/var/lib/zgs" {
		t.Errorf("file values not applied: %+v", cfg)
	}
	if len(cfg.Nodes) != 2 || cfg.Nodes[0] != "http://node-a:5678" {
		t.Errorf("nodes = %v", cfg.Nodes)
	}
	if cfg.BatchMax != 7 || cfg.ExpectedReplica != 2 {
		t.Errorf("batch_max=%d replica=%d", cfg.BatchMax, cfg.ExpectedReplica)
	}
	// Keys absent from the file keep their built-in defaults.
	if cfg.MaxSize != 4<<30 || cfg.MaxRetries != 5 || cfg.FlushIntervalMS != 3000 {
		t.Errorf("defaults not preserved for absent keys: %+v", cfg)
	}
}

func TestLoadConfigEnvOverridesFile(t *testing.T) {
	clearConfigEnv(t)
	path := writeConfig(t, `
nodes: [http://file-node:5678]
eth_rpc: https://file-rpc
private_key: fromfile
listen: ":1111"
batch_max: 7
`)
	t.Setenv("ZGS_GW_LISTEN", ":2222")
	t.Setenv("ZGS_PRIVATE_KEY", "fromenv")
	t.Setenv("ZGS_NODES", "http://env-a:5678, http://env-b:5678")
	t.Setenv("ZGS_GW_BATCH_MAX", "9")

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":2222" || cfg.PrivateKey != "fromenv" || cfg.BatchMax != 9 {
		t.Errorf("env did not override file: %+v", cfg)
	}
	if len(cfg.Nodes) != 2 || cfg.Nodes[1] != "http://env-b:5678" {
		t.Errorf("CSV nodes not trimmed/split: %v", cfg.Nodes)
	}
}

func TestLoadConfigEnvOnly(t *testing.T) {
	clearConfigEnv(t)
	t.Chdir(t.TempDir()) // no ./config.yaml here: exercise the env-only path
	t.Setenv("ZGS_NODES", "http://only:5678")
	t.Setenv("ZGS_ETH_RPC", "https://only-rpc")
	t.Setenv("ZGS_PRIVATE_KEY", "abc")

	// An absent default config file is not an error: env-only operation.
	cfg, err := loadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":8080" || len(cfg.Nodes) != 1 {
		t.Errorf("unexpected env-only config: %+v", cfg)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	t.Run("missing required", func(t *testing.T) {
		clearConfigEnv(t)
		if _, err := loadConfig(writeConfig(t, "listen: \":8080\"\n")); err == nil {
			t.Fatal("expected error for missing nodes/eth_rpc/private_key")
		}
	})
	t.Run("explicit path missing", func(t *testing.T) {
		clearConfigEnv(t)
		if _, err := loadConfig(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
			t.Fatal("expected error for an explicitly requested missing file")
		}
	})
	t.Run("bad int env", func(t *testing.T) {
		clearConfigEnv(t)
		t.Chdir(t.TempDir())
		t.Setenv("ZGS_NODES", "http://n:5678")
		t.Setenv("ZGS_ETH_RPC", "https://r")
		t.Setenv("ZGS_PRIVATE_KEY", "k")
		t.Setenv("ZGS_GW_BATCH_MAX", "notanumber")
		if _, err := loadConfig(""); err == nil {
			t.Fatal("expected error for unparseable integer env")
		}
	})
	t.Run("non-positive flush interval", func(t *testing.T) {
		clearConfigEnv(t)
		path := writeConfig(t, `
nodes: [http://n:5678]
eth_rpc: https://r
private_key: k
flush_interval_ms: 0
`)
		if _, err := loadConfig(path); err == nil {
			t.Fatal("expected error for flush_interval_ms <= 0")
		}
	})
}
