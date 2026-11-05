package vault

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/99designs/keyring"
)

func paths(t *testing.T) (string, string) {
	d := t.TempDir()
	return filepath.Join(d, "vault.enc"), filepath.Join(d, "vault.key")
}

func TestVault_RoundTrip(t *testing.T) {
	data, key := paths(t)
	v, err := Open(data, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Set(keyring.Item{Key: "a", Data: []byte("one")}); err != nil {
		t.Fatal(err)
	}
	if err := v.Set(keyring.Item{Key: "b", Data: []byte("two"), Label: "B"}); err != nil {
		t.Fatal(err)
	}
	v2, err := Open(data, key)
	if err != nil {
		t.Fatal(err)
	}
	it, err := v2.Get("b")
	if err != nil || string(it.Data) != "two" || it.Label != "B" {
		t.Fatalf("Get b = %+v, %v", it, err)
	}
	if _, err := v2.GetMetadata("a"); err != nil {
		t.Fatal(err)
	}
	keys, _ := v2.Keys()
	sort.Strings(keys)
	if len(keys) != 2 || keys[0] != "a" || keys[1] != "b" {
		t.Fatalf("Keys = %v", keys)
	}
	if err := v2.Remove("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := v2.Get("a"); !errors.Is(err, keyring.ErrKeyNotFound) {
		t.Fatalf("Get removed = %v", err)
	}
	v3, _ := Open(data, key)
	if keys, _ := v3.Keys(); len(keys) != 1 {
		t.Fatalf("Keys after reopen = %v", keys)
	}
}

func TestVault_CiphertextHasNoPlaintext(t *testing.T) {
	data, key := paths(t)
	v, _ := Open(data, key)
	if err := v.Set(keyring.Item{Key: "k", Data: []byte("s3cr3t-value")}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(data)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("s3cr3t-value")) {
		t.Fatal("plaintext found in vault file")
	}
}

func TestVault_KeyFileMode0400(t *testing.T) {
	data, key := paths(t)
	if _, err := Open(data, key); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(key)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o400 || st.Size() != 32 {
		t.Fatalf("key mode %v size %d", st.Mode().Perm(), st.Size())
	}
	st, _ = os.Stat(data)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("data mode %v", st.Mode().Perm())
	}
}

func TestVault_WrongKeyFails(t *testing.T) {
	data, key := paths(t)
	v, _ := Open(data, key)
	_ = v.Set(keyring.Item{Key: "k", Data: []byte("x")})
	_, otherKey := paths(t)
	if _, err := Open(data, otherKey); err == nil {
		t.Fatal("expected error opening with a different key")
	}
}

func TestVault_BadKeySizeFails(t *testing.T) {
	data, key := paths(t)
	if err := os.WriteFile(key, []byte("short"), 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(data, key); err == nil {
		t.Fatal("expected error for wrong-size key file")
	}
}
