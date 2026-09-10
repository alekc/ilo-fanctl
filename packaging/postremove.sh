#!/bin/sh
# Runs after the package is removed, on both deb and rpm.
set -e

# The unit file is gone by now; this is what stops systemd reporting it as
# not-found on the next command.
if command -v systemctl >/dev/null 2>&1; then
	systemctl daemon-reload >/dev/null 2>&1 || true
fi

# /etc/ilo-fanctl is left alone on purpose. It holds a config somebody tuned
# against their own hardware and a private key for the BMC, neither of which
# is the package's to delete. Removing the directory is a deliberate act.
