// Package secrets manages secure storage of sensitive configuration values
// using the platform's native credential store (SecretService, Pass, Keychain,
// or Windows Credential Manager) with an encrypted file backend as fallback.
package secrets

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/99designs/keyring"
)

// secretNamePatterns — if any of these substrings appear in an uppercased
// variable name, the value is considered sensitive.
var secretNamePatterns = []string{
	"PASSWORD", "PASSWD", "TOKEN", "SECRET", "KEY",
	"AUTH", "APIKEY", "CRED", "CERT", "PRIVATE",
}

// IsSecret reports whether varName looks like it contains a sensitive value.
func IsSecret(name string) bool {
	upper := strings.ToUpper(name)
	for _, p := range secretNamePatterns {
		if strings.Contains(upper, p) {
			return true
		}
	}
	return false
}

// Manager wraps a keyring.Keyring for homelab secret operations.
// Use Open to create one; all methods are safe to call on a nil Manager
// (they become no-ops that return empty strings).
type Manager struct {
	ring keyring.Keyring
}

// Open opens the best available keyring backend for the current platform.
//
// Backend priority (Linux): SecretService → KWallet → Pass → encrypted file.
// Backend priority (macOS): Keychain → encrypted file.
// Backend priority (Windows): Windows Credential Store → encrypted file.
//
// The file backend stores items at ~/.config/homelab/secrets/ using NaCl
// secretbox encryption. The passphrase is derived from /etc/machine-id so
// it is tied to the specific host without requiring user interaction.
func Open() (*Manager, error) {
	pass, _ := machinePassphrase()

	ring, err := keyring.Open(keyring.Config{
		ServiceName: "homelab",
		AllowedBackends: []keyring.BackendType{
			keyring.SecretServiceBackend,
			keyring.KWalletBackend,
			keyring.PassBackend,
			keyring.KeychainBackend,
			keyring.WinCredBackend,
			keyring.FileBackend,
		},
		FileDir:          ExpandHome("~/.config/homelab/secrets"),
		FilePasswordFunc: keyring.FixedStringPrompt(pass),
	})
	if err != nil {
		return nil, fmt.Errorf("open keyring: %w", err)
	}
	return &Manager{ring: ring}, nil
}

// RootKey returns the keyring item key for a root-level secret.
func RootKey(varName string) string { return "root:" + varName }

// ServiceKey returns the keyring item key for a service-level secret.
func ServiceKey(svcName, varName string) string { return "svc:" + svcName + ":" + varName }

// Set stores a secret value.
// namespace="" for root secrets; pass a service name for service secrets.
func (m *Manager) Set(namespace, varName, value string) error {
	if m == nil {
		return fmt.Errorf("keyring not available")
	}
	key := RootKey(varName)
	if namespace != "" {
		key = ServiceKey(namespace, varName)
	}
	return m.ring.Set(keyring.Item{
		Key:   key,
		Data:  []byte(value),
		Label: "homelab: " + varName,
	})
}

// Get retrieves a secret. Returns ("", nil) when the key is absent.
func (m *Manager) Get(namespace, varName string) (string, error) {
	if m == nil {
		return "", nil
	}
	key := RootKey(varName)
	if namespace != "" {
		key = ServiceKey(namespace, varName)
	}
	item, err := m.ring.Get(key)
	if err != nil {
		if errors.Is(err, keyring.ErrKeyNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("keyring get %s: %w", varName, err)
	}
	return string(item.Data), nil
}

// IsSet reports whether the secret is present and non-empty in the keyring.
func (m *Manager) IsSet(namespace, varName string) bool {
	val, err := m.Get(namespace, varName)
	return err == nil && val != ""
}

// Delete removes a secret; no-op if not present.
func (m *Manager) Delete(namespace, varName string) error {
	if m == nil {
		return nil
	}
	key := RootKey(varName)
	if namespace != "" {
		key = ServiceKey(namespace, varName)
	}
	err := m.ring.Remove(key)
	if errors.Is(err, keyring.ErrKeyNotFound) {
		return nil
	}
	return err
}

// Keys returns all variable names stored for the given namespace.
func (m *Manager) Keys(namespace string) ([]string, error) {
	if m == nil {
		return nil, nil
	}
	all, err := m.ring.Keys()
	if err != nil {
		return nil, err
	}
	prefix := "root:"
	if namespace != "" {
		prefix = "svc:" + namespace + ":"
	}
	var names []string
	for _, k := range all {
		if strings.HasPrefix(k, prefix) {
			names = append(names, strings.TrimPrefix(k, prefix))
		}
	}
	return names, nil
}

// machinePassphrase derives a stable passphrase from /etc/machine-id for use
// with the file backend. On systems without machine-id, falls back to hostname.
func machinePassphrase() (string, error) {
	id, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		hostname, _ := os.Hostname()
		h := sha256.Sum256([]byte("homelab-v1:" + hostname))
		return hex.EncodeToString(h[:16]), nil
	}
	h := sha256.Sum256(append(bytes.TrimSpace(id), []byte(":homelab-v1")...))
	return hex.EncodeToString(h[:16]), nil
}

// ExpandHome replaces a leading "~" with the user's home directory.
func ExpandHome(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return home + path[1:]
}
