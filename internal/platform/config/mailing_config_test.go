package config

import (
	"encoding/base64"
	"testing"
)

func TestMailingMasterKey(t *testing.T) {
	t.Setenv("MANYFORGE_MAILING_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.MailingMasterKey) != 32 {
		t.Fatalf("MailingMasterKey len = %d", len(cfg.MailingMasterKey))
	}
}

func TestMailingMasterKeyRejectsWrongLength(t *testing.T) {
	t.Setenv("MANYFORGE_MAILING_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 31)))
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid key error")
	}
}
