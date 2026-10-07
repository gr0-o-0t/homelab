#!/bin/sh
# Yggdrasil entrypoint: generate the node key on first run, start the daemon,
# then keep one socat forwarder running per enabled service.
#
# Each forwarder listens on the service's allocated mesh port and relays to
# Caddy (tailscale:<same port>), which has a matching `:<port>` site block.
# Caddy can't demultiplex these by Host — the mesh has no naming, so clients
# connect to [<node address>]:<port> — hence a port per service.
#
# Forwarders are reconciled on SIGHUP (`docker kill -s HUP yggdrasil`, which is
# what `homelab enable/disable --ygg` sends): socat is started for new
# .forward files, killed for removed ones, restarted for changed ones, and
# left alone otherwise. The yggdrasil daemon is never touched, so a service
# change does not drop the node's mesh peerings — restarting the container
# used to, and peers take minutes to come back.

set -e

KEY=/var/lib/yggdrasil/private.key
if [ ! -f "$KEY" ]; then
	echo "ygg: no node key yet — generating $KEY"
	mkdir -p "$(dirname "$KEY")"
	openssl genpkey -algorithm Ed25519 -out "$KEY"
	chmod 600 "$KEY"
fi

FWD_DIR=/etc/socat.d
# One <name>.pid + <name>.spec (copy of the .forward it was started from) per
# running forwarder. tmpfs-like scratch: a fresh container starts empty.
STATE=/run/socat
rm -rf "$STATE"
mkdir -p "$STATE"

# reconcile brings the running socats in line with $FWD_DIR. Kills first, so
# a forwarder whose port moved frees the old port before anything binds.
reconcile() {
	for pidf in "$STATE"/*.pid; do
		[ -f "$pidf" ] || continue
		name=$(basename "$pidf" .pid)
		pid=$(cat "$pidf")
		if [ -f "$FWD_DIR/$name.forward" ] &&
			cmp -s "$FWD_DIR/$name.forward" "$STATE/$name.spec" &&
			kill -0 "$pid" 2>/dev/null; then
			continue # unchanged and alive
		fi
		echo "ygg: stopping forwarder $name"
		kill "$pid" 2>/dev/null || true
		rm -f "$pidf" "$STATE/$name.spec"
	done

	for f in "$FWD_DIR"/*.forward; do
		[ -f "$f" ] || continue
		name=$(basename "$f" .forward)
		[ -f "$STATE/$name.pid" ] && continue
		PORT=""
		TARGET=""
		. "$f"
		[ -n "$PORT" ] && [ -n "$TARGET" ] || continue
		echo "ygg: forwarding TCP6:${PORT} -> TCP4:${TARGET} ($name)"
		socat TCP6-LISTEN:${PORT},fork,reuseaddr TCP4:${TARGET} &
		echo $! >"$STATE/$name.pid"
		cp "$f" "$STATE/$name.spec"
	done
}

shutdown() {
	for pidf in "$STATE"/*.pid; do
		[ -f "$pidf" ] && kill "$(cat "$pidf")" 2>/dev/null
	done
	kill "$YGG_PID" 2>/dev/null
	wait "$YGG_PID" 2>/dev/null
	exit 0
}

# Start yggdrasil daemon in background
echo "ygg: starting yggdrasil daemon..."
/usr/bin/yggdrasil -useconffile /etc/yggdrasil.conf &
YGG_PID=$!

# Wait for TUN interface to come up
sleep 2

set +e
RELOAD=""
trap 'RELOAD=1' HUP
trap shutdown TERM INT
reconcile

# Exit when yggdrasil exits, so the restart policy takes over. Waiting on all
# children instead would leave a container reporting "up" with a dead mesh node
# and a handful of live socats forwarding nothing. A trapped SIGHUP interrupts
# `wait`, so loop back to it after reconciling.
rc=0
while kill -0 "$YGG_PID" 2>/dev/null; do
	wait "$YGG_PID"
	rc=$?
	if [ -n "$RELOAD" ]; then
		RELOAD=""
		echo "ygg: SIGHUP — reconciling forwarders"
		reconcile
	fi
done
echo "ygg: yggdrasil exited ($rc)"
exit "$rc"
