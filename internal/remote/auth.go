// Package remote runs homelab commands on the leader agent from any PC.
//
// The CLI forwards its argv over a websocket (GET /v1/run on the agent's
// tsnet listener); the agent runs this same binary as a child against its
// state dir, under a pty when the client has a terminal, and streams the
// output and exit code back. It is the authentication boundary of the whole
// homelab: whoever passes it can run any homelab command.
package remote

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

// authContext is the HMAC message the bearer token is derived from, so the
// cluster secret itself never crosses the wire.
const authContext = "homelab-remote-v1"

// Token is the bearer token for a cluster secret:
// hex(HMAC-SHA256(secret, "homelab-remote-v1")).
func Token(secret string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(authContext))
	return hex.EncodeToString(m.Sum(nil))
}

// authorized reports whether r carries the bearer token for secret, in
// constant time. An empty secret authorizes nothing.
func authorized(r *http.Request, secret string) bool {
	if secret == "" {
		return false
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	return hmac.Equal([]byte(got), []byte(Token(secret)))
}
