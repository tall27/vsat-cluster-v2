package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	if store.Exists() {
		t.Fatal("expected no config initially")
	}
	if _, err := store.Load(); err != ErrNotConfigured {
		t.Fatalf("expected ErrNotConfigured, got %v", err)
	}

	cfg := &Config{
		PasswordHash:  "$2a$10$abcdefghijklmnopqrstuv",
		SessionSecret: []byte("0123456789abcdef0123456789abcdef"),
	}
	if err := store.Save(cfg); err != nil {
		t.Fatalf("save: %v", err)
	}
	if !store.Exists() {
		t.Fatal("expected config to exist after save")
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.PasswordHash != cfg.PasswordHash {
		t.Errorf("password hash mismatch: %q != %q", got.PasswordHash, cfg.PasswordHash)
	}
	if string(got.SessionSecret) != string(cfg.SessionSecret) {
		t.Error("session secret mismatch")
	}
	if got.InstancePrefix != DefaultInstancePrefix {
		t.Errorf("expected default prefix, got %q", got.InstancePrefix)
	}
	if got.MaxContainers != DefaultMaxContainers {
		t.Errorf("expected default max, got %d", got.MaxContainers)
	}
}

func TestSaveLoadContainerMetadata(t *testing.T) {
	store := NewStore(t.TempDir())
	cfg := &Config{
		PasswordHash:  "h",
		SessionSecret: []byte("0123456789abcdef0123456789abcdef"),
		ContainerMetadata: map[string]ContainerMetadata{
			"vsat-a": {
				Backend:        "ngts",
				TenantURL:      "https://demo.venafi.cloud",
				CompanyID:      "demo",
				OrganizationID: "org-1",
				APIBaseURL:     "https://api.venafi.cloud/",
				APIKey:         "secret-key",
				ClientID:       "client-id",
				ClientSecret:   "client-secret",
				TSGID:          "1926383011",
				EdgeInstanceID: "edge-1",
				PairingCodeID:  "pair-1",
			},
		},
	}
	if err := store.Save(cfg); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	meta := got.ContainerMetadata["vsat-a"]
	if meta.Backend != "ngts" || meta.TenantURL != "https://demo.venafi.cloud" || meta.CompanyID != "demo" || meta.OrganizationID != "org-1" ||
		meta.APIBaseURL != "https://api.venafi.cloud/" || meta.APIKey != "secret-key" || meta.ClientID != "client-id" ||
		meta.ClientSecret != "client-secret" || meta.TSGID != "1926383011" || meta.EdgeInstanceID != "edge-1" ||
		meta.PairingCodeID != "pair-1" {
		t.Fatalf("metadata did not round trip: %+v", meta)
	}
}

func TestConfigEncryptedAtRest(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	cfg := &Config{PasswordHash: "SECRET-HASH-VALUE", SessionSecret: []byte("k")}
	if err := store.Save(cfg); err != nil {
		t.Fatalf("save: %v", err)
	}
	blob, err := os.ReadFile(filepath.Join(dir, configFileName))
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if contains(blob, []byte("SECRET-HASH-VALUE")) {
		t.Error("plaintext secret found in on-disk config; not encrypted")
	}
	// Key file must exist with restrictive intent (32 bytes).
	key, err := os.ReadFile(filepath.Join(dir, keyFileName))
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	if len(key) != 32 {
		t.Errorf("expected 32-byte key, got %d", len(key))
	}
}

func TestLoadFailsWithWrongKey(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := store.Save(&Config{PasswordHash: "h", SessionSecret: []byte("s")}); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Corrupt the key.
	if err := os.WriteFile(filepath.Join(dir, keyFileName), make([]byte, 32), 0o600); err != nil {
		t.Fatalf("overwrite key: %v", err)
	}
	if _, err := store.Load(); err == nil {
		t.Error("expected decrypt failure with wrong key")
	}
}

func TestNewSessionSecretUnique(t *testing.T) {
	a, err := NewSessionSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewSessionSecret()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 32 || len(b) != 32 {
		t.Fatalf("expected 32-byte secrets, got %d and %d", len(a), len(b))
	}
	if string(a) == string(b) {
		t.Error("expected unique secrets")
	}
}

func contains(haystack, needle []byte) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == string(needle) {
			return true
		}
	}
	return false
}
