// Package vault is an encrypted file keyring (keyring.Keyring) for the agent,
// which has no desktop keyring. The whole file is one AES-256-GCM blob under a
// 32-byte key kept in a separate 0400 key file.
package vault

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
	"sync"
	"syscall"
	"time"

	"github.com/99designs/keyring"
)

const (
	keySize   = 32
	nonceSize = 12
)

type entry struct {
	Item     keyring.Item `json:"item"`
	Modified time.Time    `json:"modified"`
}

// Vault implements keyring.Keyring over an encrypted file. Safe for use by
// several goroutines and several processes (flock on <path>.lock).
type Vault struct {
	mu   sync.Mutex
	path string
	aead cipher.AEAD
}

var _ keyring.Keyring = (*Vault)(nil)

// Open opens the vault at path with the key in keyFile, creating either file
// when absent. A key file of the wrong size, or a key that cannot decrypt an
// existing vault, is an error.
func Open(path, keyFile string) (*Vault, error) {
	key, err := loadKey(keyFile)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	v := &Vault{path: path, aead: aead}
	unlock, err := v.lock(true)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := v.save(map[string]entry{}); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else if _, err := v.load(); err != nil {
		return nil, err
	}
	return v, nil
}

// lock takes the in-process mutex plus an advisory flock on <path>.lock, so
// separate Vault instances (the agent and the processes it spawns) serialize.
func (v *Vault) lock(exclusive bool) (func(), error) {
	v.mu.Lock()
	if err := os.MkdirAll(filepath.Dir(v.path), 0o700); err != nil {
		v.mu.Unlock()
		return nil, err
	}
	f, err := os.OpenFile(v.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		v.mu.Unlock()
		return nil, err
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		v.mu.Unlock()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		v.mu.Unlock()
	}, nil
}

func loadKey(keyFile string) ([]byte, error) {
	dir := filepath.Dir(keyFile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if _, err := os.Stat(keyFile); errors.Is(err, os.ErrNotExist) {
		if err := createKey(dir, keyFile); err != nil {
			return nil, err
		}
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("read vault key: %w", err)
	}
	if len(key) != keySize {
		return nil, fmt.Errorf("vault key %s: size %d, want %d", keyFile, len(key), keySize)
	}
	return key, nil
}

// createKey writes a complete 0400 key to a temp file and links it into place,
// so the key file is never visible empty; losing the race (EEXIST) is fine,
// the winner's key is then read.
func createKey(dir, keyFile string) error {
	tmp, err := os.CreateTemp(dir, ".vault-key-*")
	if err != nil {
		return fmt.Errorf("create vault key: %w", err)
	}
	defer os.Remove(tmp.Name())
	key := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(key); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o400); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmp.Name(), keyFile); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create vault key: %w", err)
	}
	return nil
}

func (v *Vault) load() (map[string]entry, error) {
	raw, err := os.ReadFile(v.path)
	if err != nil {
		return nil, err
	}
	if len(raw) < nonceSize+v.aead.Overhead() {
		return nil, fmt.Errorf("vault %s: file too short", v.path)
	}
	plain, err := v.aead.Open(nil, raw[:nonceSize], raw[nonceSize:], nil)
	if err != nil {
		return nil, fmt.Errorf("vault %s: decrypt failed (wrong key or corrupt file): %w", v.path, err)
	}
	m := map[string]entry{}
	if err := json.Unmarshal(plain, &m); err != nil {
		return nil, fmt.Errorf("vault %s: %w", v.path, err)
	}
	return m, nil
}

func (v *Vault) save(m map[string]entry) error {
	plain, err := json.Marshal(m)
	if err != nil {
		return err
	}
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	out := v.aead.Seal(nonce, nonce, plain, nil)

	tmp, err := os.CreateTemp(filepath.Dir(v.path), ".vault-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), v.path)
}

// Get returns the item for key or keyring.ErrKeyNotFound.
func (v *Vault) Get(key string) (keyring.Item, error) {
	unlock, err := v.lock(false)
	if err != nil {
		return keyring.Item{}, err
	}
	defer unlock()
	m, err := v.load()
	if err != nil {
		return keyring.Item{}, err
	}
	e, ok := m[key]
	if !ok {
		return keyring.Item{}, keyring.ErrKeyNotFound
	}
	return e.Item, nil
}

// GetMetadata returns the non-secret parts of an item.
func (v *Vault) GetMetadata(key string) (keyring.Metadata, error) {
	unlock, err := v.lock(false)
	if err != nil {
		return keyring.Metadata{}, err
	}
	defer unlock()
	m, err := v.load()
	if err != nil {
		return keyring.Metadata{}, err
	}
	e, ok := m[key]
	if !ok {
		return keyring.Metadata{}, keyring.ErrKeyNotFound
	}
	it := e.Item
	it.Data = nil
	return keyring.Metadata{Item: &it, ModificationTime: e.Modified}, nil
}

// Set stores an item.
func (v *Vault) Set(item keyring.Item) error {
	unlock, err := v.lock(true)
	if err != nil {
		return err
	}
	defer unlock()
	m, err := v.load()
	if err != nil {
		return err
	}
	m[item.Key] = entry{Item: item, Modified: time.Now().UTC()}
	return v.save(m)
}

// Remove deletes an item; keyring.ErrKeyNotFound when absent.
func (v *Vault) Remove(key string) error {
	unlock, err := v.lock(true)
	if err != nil {
		return err
	}
	defer unlock()
	m, err := v.load()
	if err != nil {
		return err
	}
	if _, ok := m[key]; !ok {
		return keyring.ErrKeyNotFound
	}
	delete(m, key)
	return v.save(m)
}

// Keys lists every stored key.
func (v *Vault) Keys() ([]string, error) {
	unlock, err := v.lock(false)
	if err != nil {
		return nil, err
	}
	defer unlock()
	m, err := v.load()
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys, nil
}
