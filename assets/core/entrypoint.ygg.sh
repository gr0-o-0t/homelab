#!/bin/sh
# Yggdrasil entrypoint: starts yggdrasil daemon in background,
# then starts socat port-forwarders for each enabled service.

set -e

# Start yggdrasil daemon in background
echo "ygg: starting yggdrasil daemon..."
/usr/bin/yggdrasil -useconffile /etc/yggdrasil.conf &

# Wait for TUN interface to come up
sleep 2

# Start socat forwarders for each enabled service
if [ -d /etc/socat.d ]; then
	for f in /etc/socat.d/*.forward; do
		[ -f "$f" ] || continue
		. "$f"
		echo "ygg: forwarding TCP6:${PORT} -> TCP4:${TARGET}"
		socat TCP6-LISTEN:${PORT},fork,reuseaddr TCP4:${TARGET} &
	done
fi

# Wait for all background processes
wait
