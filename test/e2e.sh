#!/usr/bin/env bash
# ──────────────────────────────────────────────────────────────────────────────
# E2E test for homelab CLI
#
# Tests add, new, status, doctor, enable, disable, up, down, logs, restart,
# setup, start, stop commands against a temporary config directory.
#
# Usage:
#   ./test/e2e.sh            # full test (skips Docker-dependent tests if Docker unavailable)
#   ./test/e2e.sh --docker   # force Docker-dependent tests (fails if Docker not running)
#
# Requires: go, docker (optional)
# ──────────────────────────────────────────────────────────────────────────────
set -euo pipefail

HOMELAB="./homelab"
PASS=0
FAIL=0
DOCKER_ONLY=false
[[ "${1:-}" == "--docker" ]] && DOCKER_ONLY=true

# ── helpers ───────────────────────────────────────────────────────────────────

TMPDIR=$(mktemp -d)
CONFIG_DIR="$TMPDIR/homelab-config"
mkdir -p "$CONFIG_DIR/services"

cleanup() { rm -rf "$TMPDIR"; }
trap cleanup EXIT

pass() {
	PASS=$((PASS + 1))
	echo "  ✓ $1"
}
fail() {
	FAIL=$((FAIL + 1))
	echo "  ✗ $1"
}
skip() { echo "  – $1 (skipped)"; }

check() {
	local desc="$1" expected="$2"
	shift 2
	if "$@" 2>/dev/null; then
		if [ "$expected" = "pass" ]; then pass "$desc"; else fail "$desc"; fi
	else
		if [ "$expected" = "fail" ]; then pass "$desc"; else fail "$desc"; fi
	fi
}

check_output() {
	local desc="$1" want="$2"
	shift 2
	local out
	out=$("$@" 2>&1 || true)
	if echo "$out" | grep -qF "$want"; then
		pass "$desc"
	else
		fail "$desc"
		echo "    wanted: $want"
		echo "    got:    $(echo "$out" | head -3)"
	fi
}

docker_running() {
	docker info &>/dev/null
}

# Build homelab binary
echo "Building homelab…"
go build -o "$TMPDIR/homelab" .
echo ""

# Ensure homelab is in PATH for test
PATH="$TMPDIR:$PATH"

cd "$TMPDIR"

# Write minimal root config.yaml
cat >"$CONFIG_DIR/config.yaml" <<'YAML'
vars:
  DOMAIN:
    value: "example.com"
    required: true
  HOME_SUBDOMAIN:
    value: "home"
    required: true
  ACME_EMAIL:
    value: "admin@example.com"
    required: true
  TS_HOSTNAME:
    value: "caddy-home"
    required: true
  I2P_EXT_PORT:
    value: "45678"
    required: false
secrets: {}
YAML

# Create Caddy routing directories (needed for enable symlinks)
mkdir -p "$CONFIG_DIR/caddy/conf.d" "$CONFIG_DIR/caddy/conf.d-cf" \
	"$CONFIG_DIR/caddy/conf.d-i2p" "$CONFIG_DIR/caddy/conf.d-tor" \
	"$CONFIG_DIR/caddy/conf.d-ygg"

# ── 1. add command ───────────────────────────────────────────────────────────
echo "─── 1. add ───"

check_output "add: list catalog" "uptime-kuma" \
	"$HOMELAB" --config-dir "$CONFIG_DIR" add

check "add: install uptime-kuma" pass \
	"$HOMELAB" --config-dir "$CONFIG_DIR" add uptime-kuma

check "add: verify files exist" pass \
	test -f "$CONFIG_DIR/services/uptime-kuma/config.yaml"
check "add: verify caddy.conf exists" pass \
	test -f "$CONFIG_DIR/services/uptime-kuma/caddy.conf"

check "add: reinstall fails" fail \
	"$HOMELAB" --config-dir "$CONFIG_DIR" add uptime-kuma

check "add: nonexistent service fails" fail \
	"$HOMELAB" --config-dir "$CONFIG_DIR" add definitely-not-real

# ── 2. new (scaffold) command ────────────────────────────────────────────────
echo ""
echo "─── 2. new ───"

check "new: scaffold test-svc" pass \
	"$HOMELAB" --config-dir "$CONFIG_DIR" new test-svc \
	--container test-app --port 9999

check "new: verify docker-compose.yml" pass \
	test -f "$CONFIG_DIR/services/test-svc/docker-compose.yml"
check "new: verify caddy.conf" pass \
	test -f "$CONFIG_DIR/services/test-svc/caddy.conf"
check "new: verify config.yaml with ports" pass \
	grep -q "9999" "$CONFIG_DIR/services/test-svc/config.yaml"
check "new: verify ports section" pass \
	grep -q "ports:" "$CONFIG_DIR/services/test-svc/config.yaml"

check "new: dry-run prints output" pass \
	"$HOMELAB" --config-dir "$CONFIG_DIR" new dry-svc \
	--container dry-app --port 3000 --dry-run

# ── 3. status command ────────────────────────────────────────────────────────
echo ""
echo "─── 3. status ───"

check_output "status: shows header" "Homelab Status" \
	"$HOMELAB" --config-dir "$CONFIG_DIR" status
check_output "status: uptime-kuma appears" "uptime-kuma" \
	"$HOMELAB" --config-dir "$CONFIG_DIR" status

check_output "status: service-specific shows name" "test-svc" \
	"$HOMELAB" --config-dir "$CONFIG_DIR" status test-svc

# ── 4. doctor command ────────────────────────────────────────────────────────
echo ""
echo "─── 4. doctor ───"

check_output "doctor: runs health checks" "Homelab Health" \
	"$HOMELAB" --config-dir "$CONFIG_DIR" doctor
check_output "doctor: service" "test-svc" \
	"$HOMELAB" --config-dir "$CONFIG_DIR" doctor test-svc

# ── 5. setup command (non-interactive: just verify it runs with TTY check) ───
echo ""
echo "─── 5. setup ───"

check_output "setup: root help mentions setup" "Configure homelab or service" \
	"$HOMELAB" --help
check_output "setup: service help" "Service Setup" \
	"$HOMELAB" --config-dir "$CONFIG_DIR" setup test-svc

# ── 6. enable/disable (Docker-dependent: skip if Docker not available) ───────
echo ""
echo "─── 6. enable/disable ───"

if docker_running || $DOCKER_ONLY; then
	# enable writes Caddy config; caddy reload may fail if container not running
	# but the config file is written regardless — check for it.
	ENABLE_OUT=$("$HOMELAB" --config-dir "$CONFIG_DIR" enable test-svc 2>&1 || true)
	echo "$ENABLE_OUT" | grep -qF "Enable:" && pass "enable: command runs" ||
		fail "enable: command runs (got: $(echo "$ENABLE_OUT" | head -2))"

	# Legacy caddy.conf path: creates caddy/conf.d/<svc>.conf symlink
	check "enable: symlink created" pass \
		test -L "$CONFIG_DIR/caddy/conf.d/test-svc.conf"

	DISABLE_OUT=$("$HOMELAB" --config-dir "$CONFIG_DIR" disable test-svc 2>&1 || true)
	echo "$DISABLE_OUT" | grep -qF "test-svc" && pass "disable: removes routes" ||
		fail "disable: removes routes (got: $(echo "$DISABLE_OUT" | head -2))"
	check "disable: symlink removed" fail \
		test -L "$CONFIG_DIR/caddy/conf.d/test-svc.conf"
else
	skip "enable/disable tests (Docker not available)"
fi

# ── 7. up/down/logs/restart ──────────────────────────────────────────────────
echo ""
echo "─── 7. up/down/logs/restart ───"

if docker_running || $DOCKER_ONLY; then
	check_output "up: help mentions service" "service" \
		"$HOMELAB" up --help
	check_output "start: help mentions service" "service" \
		"$HOMELAB" start --help
	check_output "restart: help mentions service" "service" \
		"$HOMELAB" restart --help
	check_output "stop: help mentions service" "service" \
		"$HOMELAB" stop --help

	# These will fail because the test-svc compose file has no real image,
	# but the command should be recognized and attempted.
	check "up: nonexistent service fails" fail \
		"$HOMELAB" --config-dir "$CONFIG_DIR" up definitely-not-real
else
	skip "up/down/logs/restart tests (Docker not available)"
fi

# ── 8. root-level command presence ───────────────────────────────────────────
echo ""
echo "─── 8. CLI completeness ───"

for cmd in add status setup enable disable start stop restart reload \
	validate logs doctor new delete pull exec config images port; do
	check_output "CLI has command: $cmd" "help for $cmd" \
		"$HOMELAB" "$cmd" --help
done

# ── 9. service subcommand backward compat ────────────────────────────────────
echo ""
echo "─── 9. backward compat ───"

check_output "service: list available" "service" \
	"$HOMELAB" --help
check_output "service: list subcommands works" "uptime-kuma" \
	"$HOMELAB" --config-dir "$CONFIG_DIR" service list
check_output "service: ps works" "service" \
	"$HOMELAB" service ps --help 2>&1 || true

# up/down aliases
check_output "up is primary command" "Create and start" \
	"$HOMELAB" up --help 2>&1
check_output "down is primary command" "Stop and remove" \
	"$HOMELAB" down --help 2>&1
check_output "alias rm → delete" "delete" \
	"$HOMELAB" rm --help 2>&1

# ── 10. version info ─────────────────────────────────────────────────────────
echo ""
echo "─── 10. misc ───"

check "unknown flag errors" fail \
	"$HOMELAB" --nonexistent-flag

# ── results ──────────────────────────────────────────────────────────────────
echo ""
echo "═══════════════════════════════════════════════════════════════"
echo "  PASS: $PASS   FAIL: $FAIL"
echo "═══════════════════════════════════════════════════════════════"
echo ""
if [ "$FAIL" -gt 0 ]; then
	exit 1
fi
