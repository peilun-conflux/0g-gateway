package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config holds every gateway setting. Values are resolved in increasing order of
// precedence:
//
//  1. built-in defaults (defaultConfig)
//  2. the YAML config file, if present
//  3. environment variables
//
// so an env var always overrides the file. That ordering lets an operator keep
// most settings in a mounted config file while injecting secrets such as
// private_key (ZGS_PRIVATE_KEY) through the environment.
type Config struct {
	Listen  string `yaml:"listen"`   // S3-compatible endpoint listen address
	DataDir string `yaml:"data_dir"` // local cache + bbolt metadata directory

	// 0G backend — all three are required.
	Nodes      []string `yaml:"nodes"`       // storage-node JSON-RPC endpoints
	EthRPC     string   `yaml:"eth_rpc"`     // host-chain (EVM) RPC, where the Flow contract lives
	PrivateKey string   `yaml:"private_key"` // signer key (hex, no 0x); needs gas on the host chain

	MaxSize         int64 `yaml:"max_size"`          // max object size in bytes
	CacheMaxBytes   int64 `yaml:"cache_max_bytes"`   // cache size that triggers finalized-LRU eviction; 0 = unbounded
	BatchMax        int   `yaml:"batch_max"`         // max objects per on-chain batch
	MaxRetries      int   `yaml:"max_retries"`       // upload retry ceiling
	FlushIntervalMS int   `yaml:"flush_interval_ms"` // worker flush interval (ms), must be > 0
	ExpectedReplica uint  `yaml:"expected_replica"`  // 0 → default to len(Nodes)
}

func defaultConfig() Config {
	return Config{
		Listen:          ":8080",
		DataDir:         "./data",
		MaxSize:         4 << 30,  // one object = one root: cap at the SDK fragment size
		CacheMaxBytes:   10 << 30, // bound local disk; finalized objects evict (cold-readable from 0G)
		BatchMax:        20,
		MaxRetries:      5,
		FlushIntervalMS: 3000,
	}
}

// loadConfig resolves the configuration from defaults, the YAML file at path,
// and the environment (see Config). An empty path falls back to ./config.yaml
// when that file exists; a path that is explicitly set but unreadable is a fatal
// error, while a missing default file just means env-only operation. The
// returned Config is validated.
func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()

	explicit := path != ""
	if path == "" {
		path = "config.yaml"
	}
	switch data, err := os.ReadFile(path); {
	case err == nil:
		// Unmarshalling onto the defaults overwrites only the keys present in
		// the file, leaving the rest at their default values.
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse config file %s: %w", path, err)
		}
	case explicit || !os.IsNotExist(err):
		// A file the operator asked for, or a real I/O error on the default
		// path — surface it rather than silently falling back to env-only.
		return Config{}, fmt.Errorf("read config file %s: %w", path, err)
	}

	if err := applyEnvOverrides(&cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyEnvOverrides(cfg *Config) error {
	envStr("ZGS_GW_LISTEN", &cfg.Listen)
	envStr("ZGS_GW_DATA_DIR", &cfg.DataDir)
	envStr("ZGS_ETH_RPC", &cfg.EthRPC)
	envStr("ZGS_PRIVATE_KEY", &cfg.PrivateKey)
	if v := os.Getenv("ZGS_NODES"); v != "" {
		cfg.Nodes = splitCSV(v)
	}
	if err := envInt("ZGS_GW_MAX_SIZE", &cfg.MaxSize); err != nil {
		return err
	}
	if err := envInt("ZGS_GW_CACHE_MAX_BYTES", &cfg.CacheMaxBytes); err != nil {
		return err
	}
	if err := envInt("ZGS_GW_BATCH_MAX", &cfg.BatchMax); err != nil {
		return err
	}
	if err := envInt("ZGS_GW_MAX_RETRIES", &cfg.MaxRetries); err != nil {
		return err
	}
	if err := envInt("ZGS_GW_FLUSH_INTERVAL_MS", &cfg.FlushIntervalMS); err != nil {
		return err
	}
	return envInt("ZGS_EXPECTED_REPLICA", &cfg.ExpectedReplica)
}

func (c Config) validate() error {
	var missing []string
	if len(c.Nodes) == 0 {
		missing = append(missing, "nodes (ZGS_NODES)")
	}
	if c.EthRPC == "" {
		missing = append(missing, "eth_rpc (ZGS_ETH_RPC)")
	}
	if c.PrivateKey == "" {
		missing = append(missing, "private_key (ZGS_PRIVATE_KEY)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
	}
	if c.FlushIntervalMS <= 0 {
		return fmt.Errorf("flush_interval_ms must be a positive integer, got %d", c.FlushIntervalMS)
	}
	if c.CacheMaxBytes < 0 {
		return fmt.Errorf("cache_max_bytes must be >= 0 (0 = unbounded), got %d", c.CacheMaxBytes)
	}
	return nil
}

// splitCSV parses a comma-separated env value into a trimmed, empty-free slice.
func splitCSV(v string) []string {
	parts := strings.Split(v, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// integer constrains envInt to the numeric config field types.
type integer interface {
	~int | ~int64 | ~uint
}

// envStr overwrites *dst with the env var k when it is set and non-empty.
func envStr(k string, dst *string) {
	if v := os.Getenv(k); v != "" {
		*dst = v
	}
}

// envInt overwrites *dst with the integer env var k when set; an unparseable
// value is a fatal configuration error rather than a silent fallback.
func envInt[T integer](k string, dst *T) error {
	v := os.Getenv(k)
	if v == "" {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid integer for %s: %q", k, v)
	}
	*dst = T(n)
	return nil
}
