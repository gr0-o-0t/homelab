#!/bin/sh
# Wrapper entrypoint for purplei2p/i2pd that copies host-mounted
# config files into the data directory with correct ownership
# before handing off to the stock entrypoint.
#
# Config files are mounted read-only from the host at /config-host/.
# Since Docker bind mounts preserve host UID ownership and the
# container runs as UID 166 (i2pd), a simple copy is needed.

set -e

DATA_DIR="${DATA_DIR:-/home/i2pd/data}"

if [ -d /config-host ]; then
	for f in i2pd.conf tunnels.conf; do
		if [ -f "/config-host/$f" ]; then
			cp "/config-host/$f" "$DATA_DIR/$f"
			chown 166:166 "$DATA_DIR/$f"
			chmod 600 "$DATA_DIR/$f"
		fi
	done
fi

# Hand off to stock entrypoint, preserving its default args.
exec /entrypoint.sh "$@"
