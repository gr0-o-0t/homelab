// Package envshim carries a container's environment through a Swarm secret
// instead of the (readable) service spec.
package envshim

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"maps"
	"slices"
)

// Encode serialises env as sorted, length-prefixed key and value records, so
// no value can be confused with a separator and equal maps encode equally.
func Encode(env map[string]string) []byte {
	var out []byte
	for _, k := range slices.Sorted(maps.Keys(env)) {
		out = binary.AppendUvarint(out, uint64(len(k)))
		out = append(out, k...)
		out = binary.AppendUvarint(out, uint64(len(env[k])))
		out = append(out, env[k]...)
	}
	return out
}

var errCorrupt = errors.New("envshim: corrupt environment data")

// Decode is the inverse of Encode.
func Decode(b []byte) (map[string]string, error) {
	env := map[string]string{}
	next := func() (string, bool) {
		n, w := binary.Uvarint(b)
		if w <= 0 || uint64(len(b)-w) < n {
			return "", false
		}
		s := string(b[w : w+int(n)])
		b = b[w+int(n):]
		return s, true
	}
	for len(b) > 0 {
		k, ok := next()
		if !ok {
			return nil, errCorrupt
		}
		v, ok := next()
		if !ok {
			return nil, errCorrupt
		}
		env[k] = v
	}
	return env, nil
}

// SecretName is content-addressed, so a changed environment is a new secret
// and the service update rolls its tasks.
func SecretName(svc string, content []byte) string {
	sum := sha256.Sum256(content)
	return "hl_" + svc + "_env_" + hex.EncodeToString(sum[:])[:12]
}
