package secrets_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/groot/homelab/internal/secrets"
)

// ── IsSecret ─────────────────────────────────────────────────────────────────────

func TestIsSecret_DetectsPassword(t *testing.T) {
	cases := []struct {
		name     string
		expected bool
	}{
		{"PASSWORD", true},
		{"PASSWD", true},
		{"MY_PASSWORD", true},
		{"DB_PASSWORD", true},
		{"admin_password", true},
		// ROOTPASS doesn't match "PASSWD" pattern (requires full word match)
	}
	for _, c := range cases {
		result := secrets.IsSecret(c.name)
		assert.Equal(t, c.expected, result, "IsSecret(%q)", c.name)
	}
}

func TestIsSecret_DetectsToken(t *testing.T) {
	cases := []string{"TOKEN", "AUTH_TOKEN", "API_TOKEN", "access_token", "refresh_token"}
	for _, name := range cases {
		assert.True(t, secrets.IsSecret(name), "IsSecret(%q)", name)
	}
}

func TestIsSecret_DetectsSecret(t *testing.T) {
	cases := []string{"SECRET", "CLIENT_SECRET", "jwt_secret", "app_secret"}
	for _, name := range cases {
		assert.True(t, secrets.IsSecret(name), "IsSecret(%q)", name)
	}
}

func TestIsSecret_DetectsKey(t *testing.T) {
	cases := []string{"KEY", "APIKEY", "RSA_KEY", "private_key", "ENCRYPTION_KEY"}
	for _, name := range cases {
		assert.True(t, secrets.IsSecret(name), "IsSecret(%q)", name)
	}
}

func TestIsSecret_DetectsAuth(t *testing.T) {
	cases := []string{"AUTH", "AUTH_KEY", "basic_auth", "BEARER_AUTH"}
	for _, name := range cases {
		assert.True(t, secrets.IsSecret(name), "IsSecret(%q)", name)
	}
}

func TestIsSecret_DetectsCred(t *testing.T) {
	cases := []string{"CRED", "CREDENTIALS", "mysql_cred", "user_creds"}
	for _, name := range cases {
		assert.True(t, secrets.IsSecret(name), "IsSecret(%q)", name)
	}
}

func TestIsSecret_DetectsCert(t *testing.T) {
	cases := []string{"CERT", "CLIENT_CERT", "ssl_cert", "tls_cert"}
	for _, name := range cases {
		assert.True(t, secrets.IsSecret(name), "IsSecret(%q)", name)
	}
}

func TestIsSecret_DetectsPrivate(t *testing.T) {
	cases := []string{"PRIVATE_KEY", "private_token", "PRIVATE_TOKEN"}
	for _, name := range cases {
		assert.True(t, secrets.IsSecret(name), "IsSecret(%q)", name)
	}
}

func TestIsSecret_NonSensitiveNames(t *testing.T) {
	cases := []string{
		"DOMAIN", "HOME_SUBDOMAIN", "ACME_EMAIL",
		"PORT", "HOST", "SERVER_URL",
		"DEBUG", "LOG_LEVEL",
		"USERNAME", "EMAIL", "FULL_NAME",
	}
	for _, name := range cases {
		assert.False(t, secrets.IsSecret(name), "IsSecret(%q) should be false", name)
	}
}

func TestIsSecret_CaseInsensitive(t *testing.T) {
	assert.True(t, secrets.IsSecret("password"))
	assert.True(t, secrets.IsSecret("PASSWORD"))
	assert.True(t, secrets.IsSecret("Password"))
	assert.True(t, secrets.IsSecret("PaSsWoRd"))
}

// ── RootKey / ServiceKey ───────────────────────────────────────────────────────

func TestRootKey_Format(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"TS_AUTHKEY", "root:TS_AUTHKEY"},
		{"CLOUDFLARE_API_TOKEN", "root:CLOUDFLARE_API_TOKEN"},
		{"DOMAIN", "root:DOMAIN"},
	}
	for _, tt := range tests {
		result := secrets.RootKey(tt.input)
		assert.Equal(t, tt.expected, result, "RootKey(%q)", tt.input)
	}
}

func TestServiceKey_Format(t *testing.T) {
	tests := []struct {
		svcName  string
		varName  string
		expected string
	}{
		{"immich", "DB_PASSWORD", "svc:immich:DB_PASSWORD"},
		{"jellyfin", "API_KEY", "svc:jellyfin:API_KEY"},
		{"uptime-kuma", "SMTP_PASS", "svc:uptime-kuma:SMTP_PASS"},
	}
	for _, tt := range tests {
		result := secrets.ServiceKey(tt.svcName, tt.varName)
		assert.Equal(t, tt.expected, result, "ServiceKey(%q, %q)", tt.svcName, tt.varName)
	}
}

// ── expandHome ─────────────────────────────────────────────────────────────────

func TestExpandHome_ReplacesTilde(t *testing.T) {
	t.Setenv("HOME", "/home/testuser")

	result := secrets.ExpandHome("~/.config/homelab")
	assert.Equal(t, "/home/testuser/.config/homelab", result)
}

func TestExpandHome_NoTilde(t *testing.T) {
	result := secrets.ExpandHome("/absolute/path")
	assert.Equal(t, "/absolute/path", result)
}

func TestExpandHome_Empty(t *testing.T) {
	result := secrets.ExpandHome("")
	assert.Equal(t, "", result)
}