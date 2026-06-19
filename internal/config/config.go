// Package config loads and persists the VSAT Cluster v2 web app configuration.
//
// The config holds the admin password hash, the session-signing secret and a few
// operational settings. It is written to disk encrypted at rest with AES-256-GCM;
// the AES key lives in a sibling key file with 0600 permissions. This is modest,
// lab-grade protection: copying the encrypted blob alone does not reveal secrets.
package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	configFileName = "config.enc"
	keyFileName    = "config.key"

	// DefaultMaxContainers is the hard cap on simultaneous VSAT containers.
	DefaultMaxContainers = 4
	// DefaultInstancePrefix is prepended to / enforced on container names.
	DefaultInstancePrefix = "vsat"
)

// ErrNotConfigured is returned by Load when no config file exists yet, signalling
// that the first-run /setup flow should be shown.
var ErrNotConfigured = errors.New("config: not configured")

// Config is the persisted application configuration.
type Config struct {
	// PasswordHash is the bcrypt hash of the static admin password.
	PasswordHash string `json:"passwordHash"`
	// SessionSecret is the random HMAC key used to sign session cookies.
	SessionSecret []byte `json:"sessionSecret"`
	// InstancePrefix is enforced as the prefix of every container name.
	InstancePrefix string `json:"instancePrefix"`
	// MaxContainers caps how many VSAT containers may exist at once.
	MaxContainers int `json:"maxContainers"`
	// ContainerMetadata stores per-container display metadata and encrypted CCM
	// cleanup credentials by container name.
	ContainerMetadata map[string]ContainerMetadata `json:"containerMetadata,omitempty"`
}

// ContainerMetadata is captured after successful installs. The config file is
// encrypted at rest; APIKey is stored only so remove can clean up the tenant.
type ContainerMetadata struct {
	TenantURL      string `json:"tenantUrl,omitempty"`
	CompanyID      string `json:"companyId,omitempty"`
	OrganizationID string `json:"organizationId,omitempty"`
	APIBaseURL     string `json:"apiBaseUrl,omitempty"`
	APIKey         string `json:"apiKey,omitempty"`
	EdgeInstanceID string `json:"edgeInstanceId,omitempty"`
	PairingCodeID  string `json:"pairingCodeId,omitempty"`
}

// Store reads and writes the encrypted config within a directory.
type Store struct {
	Dir string
}

// NewStore returns a Store rooted at dir. If dir is empty, DefaultDir is used.
func NewStore(dir string) *Store {
	if strings.TrimSpace(dir) == "" {
		dir = DefaultDir()
	}
	return &Store{Dir: dir}
}

// DefaultDir resolves the config directory, honouring VSAT_CONFIG_DIR, then
// falling back to a per-user config location.
func DefaultDir() string {
	if dir := strings.TrimSpace(os.Getenv("VSAT_CONFIG_DIR")); dir != "" {
		return dir
	}
	if base, err := os.UserConfigDir(); err == nil {
		return filepath.Join(base, "vsat-cluster")
	}
	return filepath.Join(".", ".vsat-cluster")
}

func (s *Store) configPath() string { return filepath.Join(s.Dir, configFileName) }
func (s *Store) keyPath() string    { return filepath.Join(s.Dir, keyFileName) }

// Exists reports whether a config file is already present.
func (s *Store) Exists() bool {
	_, err := os.Stat(s.configPath())
	return err == nil
}

// Load decrypts and returns the stored config, or ErrNotConfigured if absent.
func (s *Store) Load() (*Config, error) {
	blob, err := os.ReadFile(s.configPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotConfigured
	}
	if err != nil {
		return nil, fmt.Errorf("config: read: %w", err)
	}
	key, err := s.loadKey()
	if err != nil {
		return nil, err
	}
	plain, err := decrypt(key, blob)
	if err != nil {
		return nil, fmt.Errorf("config: decrypt: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(plain, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse: %w", err)
	}
	cfg.applyDefaults()
	return &cfg, nil
}

// Save encrypts and writes the config, creating the directory and key as needed.
func (s *Store) Save(cfg *Config) error {
	if cfg == nil {
		return errors.New("config: nil config")
	}
	cfg.applyDefaults()
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("config: mkdir: %w", err)
	}
	key, err := s.loadOrCreateKey()
	if err != nil {
		return err
	}
	plain, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	blob, err := encrypt(key, plain)
	if err != nil {
		return fmt.Errorf("config: encrypt: %w", err)
	}
	if err := writeFileAtomic(s.configPath(), blob, 0o600); err != nil {
		return fmt.Errorf("config: write: %w", err)
	}
	return nil
}

func (c *Config) applyDefaults() {
	if strings.TrimSpace(c.InstancePrefix) == "" {
		c.InstancePrefix = DefaultInstancePrefix
	}
	if c.MaxContainers <= 0 {
		c.MaxContainers = DefaultMaxContainers
	}
}

// NewSessionSecret returns a fresh 32-byte random secret for cookie signing.
func NewSessionSecret() ([]byte, error) {
	secret := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, secret); err != nil {
		return nil, fmt.Errorf("config: session secret: %w", err)
	}
	return secret, nil
}

// --- key handling ---------------------------------------------------------

func (s *Store) loadKey() ([]byte, error) {
	key, err := os.ReadFile(s.keyPath())
	if err != nil {
		return nil, fmt.Errorf("config: read key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("config: key must be 32 bytes, got %d", len(key))
	}
	return key, nil
}

func (s *Store) loadOrCreateKey() ([]byte, error) {
	if key, err := s.loadKey(); err == nil {
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(errors.Unwrap(err), os.ErrNotExist) {
		// A genuine read error (not "missing") should surface, except length
		// mismatch which we treat as corrupt and regenerate below only on absence.
		if _, statErr := os.Stat(s.keyPath()); statErr == nil {
			return nil, err
		}
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("config: generate key: %w", err)
	}
	if err := writeFileAtomic(s.keyPath(), key, 0o600); err != nil {
		return nil, fmt.Errorf("config: write key: %w", err)
	}
	return key, nil
}

// --- AES-GCM --------------------------------------------------------------

func encrypt(key, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func decrypt(key, blob []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(blob) < ns {
		return nil, errors.New("ciphertext too short")
	}
	nonce, ciphertext := blob[:ns], blob[ns:]
	return gcm.Open(nil, nonce, ciphertext, nil)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
