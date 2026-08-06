#!/bin/sh
# Yggdrasil entrypoint: generate the node key on first run, start the daemon,
# then start one socat forwarder per enabled service.
#
# Each forwarder listens on the service's allocated mesh port and relays to
# Caddy (tailscale:<same port>), which has a matching `:<port>` site block.
# Caddy can't demultiplex these by Host — the mesh has no naming, so clients
# connect to [<node address>]:<port> — hence a port per service.

set -e

KEY=/var/lib/yggdrasil/private.key
if [ ! -f "$KEY" ]; then
	echo "ygg: no node key yet — generating $KEY"
	mkdir -p "$(dirname "$KEY")"
	openssl genpkey -algorithm Ed25519 -out "$KEY"
	chmod 600 "$KEY"
fi

# Start yggdrasil daemon in background
echo "ygg: starting yggdrasil daemon..."
/usr/bin/yggdrasil -useconffile /etc/yggdrasil.conf &
YGG_PID=$!

# Wait for TUN interface to come up
sleep 2

# Start socat forwarders for each enabled service
if [ -d /etc/socat.d ]; then
	for f in /etc/socat.d/*.forward; do
		[ -f "$f" ] || continue
		PORT=""
		TARGET=""
		. "$f"
		[ -n "$PORT" ] && [ -n "$TARGET" ] || continue
		echo "ygg: forwarding TCP6:${PORT} -> TCP4:${TARGET}"
		socat TCP6-LISTEN:${PORT},fork,reuseaddr TCP4:${TARGET} &
	done
fi

# Exit when yggdrasil exits, so the restart policy takes over. Waiting on all
# children instead would leave a container reporting "up" with a dead mesh node
# and a handful of live socats forwarding nothing.
wait $YGG_PID
