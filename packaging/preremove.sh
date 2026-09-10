#!/bin/sh
# Runs before the old package is removed, on both deb and rpm.
set -e

# The two packagers disagree about what they pass here. dpkg sends a word
# ("remove", "upgrade", "purge"); rpm sends a count of the versions that will
# remain, so 0 means this is the last one going away and 1 means an upgrade.
# Matching both is the only way to tell a removal from an upgrade.
case "$1" in
remove | purge | 0)
	# Stop only on a real removal. Stopping during an upgrade would wind
	# every fan back down to min_floor_pct, hand the machine to iLO's own
	# curve, and then take it all back seconds later when postinstall
	# restarts the service. One SSH round trip per fan, twice, for nothing.
	if command -v systemctl >/dev/null 2>&1; then
		systemctl --no-reload disable --now ilo-fanctl >/dev/null 2>&1 || true
	fi
	;;
esac
