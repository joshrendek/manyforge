package config

import (
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
