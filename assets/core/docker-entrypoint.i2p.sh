#!/bin/sh
# Wrapper entrypoint for purplei2p/i2pd.
#
# Config files are mounted read-only from the host at /config-host/ (the whole
# directory, so edits on the host are visible here without a restart).
#
# tunnels.conf is read in place: i2pd is pointed at /config-host/tunnels.conf
# with --tunconf, so `homelab enable/disable --i2p` only has to send SIGHUP
# (`docker kill -s HUP i2p`) and i2pd reloads its tunnel list — adding new
# eepsites, dropping removed ones, leaving the rest up. A copy made at startup,
# as this script used to do, meant every change needed a container restart,
# which tears down every eepsite's tunnels for minutes.
#
# The file is written 0600 by the host user, so it is only readable here
# because this image runs i2pd as root (the upstream entrypoint never drops
# privileges). If that ever changes, i2pd will log that it cannot open it.
#
# i2pd.conf is still copied: it changes rarely and needs a restart anyway.

set -e

DATA_DIR="${DATA_DIR:-/home/i2pd/data}"

if [ -f /config-host/i2pd.conf ]; then
	cp /config-host/i2pd.conf "$DATA_DIR/i2pd.conf"
	chown 166:166 "$DATA_DIR/i2pd.conf"
	chmod 600 "$DATA_DIR/i2pd.conf"
fi

if [ -d /config-host ]; then
	# Passed even when the file does not exist yet: the first
	# `homelab enable --i2p` creates it, and the SIGHUP after that loads it.
	# Drop the stale copy a previous version of this script left, so nobody
	# edits a file i2pd no longer reads.
	rm -f "$DATA_DIR/tunnels.conf"
	set -- "$@" --tunconf=/config-host/tunnels.conf
fi

# Hand off to stock entrypoint, preserving its default args. It execs i2pd,
# so i2pd is PID 1 and receives `docker kill -s HUP` directly.
exec /entrypoint.sh "$@"
