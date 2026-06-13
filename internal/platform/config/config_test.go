package config

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func TestLoadDKIMMasterKey(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}

	t.Run("base64", func(t *testing.T) {
		t.Setenv("MANYFORGE_DKIM_MASTER_KEY", base64.StdEncoding.EncodeToString(key))
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(cfg.DKIMMasterKey) != 32 {
			t.Fatalf("DKIMMasterKey len = %d, want 32", len(cfg.DKIMMasterKey))
		}
	})

	t.Run("hex", func(t *testing.T) {
		t.Setenv("MANYFORGE_DKIM_MASTER_KEY", hex.EncodeToString(key))
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(cfg.DKIMMasterKey) != 32 {
			t.Fatalf("DKIMMasterKey len = %d, want 32", len(cfg.DKIMMasterKey))
		}
	})

	t.Run("wrong-length-is-config-error", func(t *testing.T) {
		short := make([]byte, 16)
		t.Setenv("MANYFORGE_DKIM_MASTER_KEY", base64.StdEncoding.EncodeToString(short))
		if _, err := Load(); err == nil {
			t.Fatal("expected error for 16-byte key, got nil")
		}
	})

	t.Run("garbage-is-config-error", func(t *testing.T) {
		t.Setenv("MANYFORGE_DKIM_MASTER_KEY", "not-base64-or-hex-!!!")
		if _, err := Load(); err == nil {
			t.Fatal("expected error for undecodable key, got nil")
		}
	})

	t.Run("unset-is-no-key-no-error", func(t *testing.T) {
		t.Setenv("MANYFORGE_DKIM_MASTER_KEY", "")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.DKIMMasterKey != nil {
			t.Fatalf("DKIMMasterKey = %x, want nil when unset", cfg.DKIMMasterKey)
		}
	})
}

func TestLoadMCPAllowLoopback(t *testing.T) {
	t.Run("true-when-set", func(t *testing.T) {
		t.Setenv("MANYFORGE_MCP_ALLOW_LOOPBACK", "true")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.MCPAllowLoopback {
			t.Fatal("MCPAllowLoopback = false, want true")
		}
	})

	t.Run("false-when-unset", func(t *testing.T) {
		t.Setenv("MANYFORGE_MCP_ALLOW_LOOPBACK", "")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.MCPAllowLoopback {
			t.Fatal("MCPAllowLoopback = true, want false")
		}
	})

	t.Run("invalid-value-is-config-error", func(t *testing.T) {
		t.Setenv("MANYFORGE_MCP_ALLOW_LOOPBACK", "notabool")
		if _, err := Load(); err == nil {
			t.Fatal("expected error for invalid bool, got nil")
		}
	})
}

// TestEnvKey32Disambiguation (manyforge-no9) pins the explicit-prefix and anchored
// auto-detect parsing so a 32-byte key is loaded deterministically rather than via
// "first decoder that yields 32 bytes": "hex:"/"base64:" prefixes are authoritative,
// a bare 64-char [0-9a-fA-F] value is hex, everything else is base64 (padded or raw).
func TestEnvKey32Disambiguation(t *testing.T) {
	const env = "MANYFORGE_TEST_KEY32"
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	ok := func(t *testing.T, val string) {
		t.Helper()
		t.Setenv(env, val)
		b, err := envKey32(env)
		if err != nil {
			t.Fatalf("envKey32(%q): unexpected error %v", val, err)
		}
		if !bytes.Equal(b, key) {
			t.Fatalf("envKey32(%q) = %x, want the 32-byte key", val, b)
		}
	}
	bad := func(t *testing.T, val string) {
		t.Helper()
		t.Setenv(env, val)
		if _, err := envKey32(env); err == nil {
			t.Fatalf("envKey32(%q): want error, got nil", val)
		}
	}

	t.Run("explicit hex: prefix", func(t *testing.T) { ok(t, "hex:"+hex.EncodeToString(key)) })
	t.Run("explicit base64: prefix (std)", func(t *testing.T) { ok(t, "base64:"+base64.StdEncoding.EncodeToString(key)) })
	t.Run("explicit base64: prefix (url)", func(t *testing.T) { ok(t, "base64:"+base64.URLEncoding.EncodeToString(key)) })
	t.Run("bare 64-char hex is hex", func(t *testing.T) { ok(t, hex.EncodeToString(key)) })
	t.Run("bare 44-char std base64", func(t *testing.T) { ok(t, base64.StdEncoding.EncodeToString(key)) })
	t.Run("bare raw unpadded base64", func(t *testing.T) { ok(t, base64.RawStdEncoding.EncodeToString(key)) })
	t.Run("explicit hex wrong length errors", func(t *testing.T) { bad(t, "hex:"+hex.EncodeToString(key[:16])) })
	t.Run("explicit hex non-hex errors", func(t *testing.T) { bad(t, "hex:zzzz") })
	t.Run("explicit base64 garbage errors", func(t *testing.T) { bad(t, "base64:not valid!!") })
}

// TestLoadAgentRunLimits pins manyforge-ji7: the agent run-loop bounds + temperature load from
// MANYFORGE_AGENT_* env keys, defaulting to the code defaults when unset.
func TestLoadAgentRunLimits(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.AgentMaxIterations != 8 {
			t.Errorf("AgentMaxIterations = %d, want 8", cfg.AgentMaxIterations)
		}
		if cfg.AgentMaxTokensPerRun != 100_000 {
			t.Errorf("AgentMaxTokensPerRun = %d, want 100000", cfg.AgentMaxTokensPerRun)
		}
		if cfg.AgentMaxOutputTokens != 4096 {
			t.Errorf("AgentMaxOutputTokens = %d, want 4096", cfg.AgentMaxOutputTokens)
		}
		if cfg.AgentWallClock.String() != "2m0s" {
			t.Errorf("AgentWallClock = %s, want 2m0s", cfg.AgentWallClock)
		}
		if cfg.AgentTemperature != 0.0 {
			t.Errorf("AgentTemperature = %v, want 0", cfg.AgentTemperature)
		}
		if cfg.AgentRetriageCapPerHour != 5 {
			t.Errorf("AgentRetriageCapPerHour = %d, want 5", cfg.AgentRetriageCapPerHour)
		}
	})

	t.Run("overrides", func(t *testing.T) {
		t.Setenv("MANYFORGE_AGENT_MAX_ITERATIONS", "12")
		t.Setenv("MANYFORGE_AGENT_MAX_TOKENS_PER_RUN", "250000")
		t.Setenv("MANYFORGE_AGENT_MAX_OUTPUT_TOKENS", "8192")
		t.Setenv("MANYFORGE_AGENT_WALL_CLOCK", "90s")
		t.Setenv("MANYFORGE_AGENT_TEMPERATURE", "0.7")
		t.Setenv("MANYFORGE_AGENT_RETRIAGE_CAP_PER_HOUR", "9")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.AgentMaxIterations != 12 || cfg.AgentMaxTokensPerRun != 250_000 ||
			cfg.AgentMaxOutputTokens != 8192 || cfg.AgentWallClock.String() != "1m30s" || cfg.AgentTemperature != 0.7 ||
			cfg.AgentRetriageCapPerHour != 9 {
			t.Errorf("overrides not applied: %+v", cfg)
		}
	})

	t.Run("malformed-is-config-error", func(t *testing.T) {
		t.Setenv("MANYFORGE_AGENT_MAX_ITERATIONS", "not-a-number")
		if _, err := Load(); err == nil {
			t.Fatal("Load with malformed MANYFORGE_AGENT_MAX_ITERATIONS: want error, got nil")
		}
	})
}
