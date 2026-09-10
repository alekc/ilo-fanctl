#!/bin/sh
# Runs after install and after upgrade, on both deb and rpm.
set -e

have_systemctl() {
	command -v systemctl >/dev/null 2>&1
}

# A container or chroot has no systemd, and a package that fails to install
# there for want of it is worse than one that installs and does nothing.
if have_systemctl; then
	systemctl daemon-reload >/dev/null 2>&1 || true
fi

# Replacing the binary does not change the running process: it holds the old
# inode until something restarts it. Without this an upgrade looks like it
# worked and the machine keeps running the old build, which is the exact
# failure ilo_fanctl_build_info exists to make visible.
#
# Only an already-running service is restarted. A stopped one stays stopped,
# and an unconfigured one is never started at all, see below.
if have_systemctl && systemctl is-active --quiet ilo-fanctl 2>/dev/null; then
	systemctl restart ilo-fanctl || true
fi

# Deliberately neither enabled nor started on a first install. This daemon
# cannot run without /etc/ilo-fanctl/config.yaml and a private key the BMC
# accepts, and a package can ship neither. Starting here would leave a restart
# loop on a machine whose owner has not configured anything yet.
if [ ! -f /etc/ilo-fanctl/config.yaml ]; then
	cat <<'EOF'

ilo-fanctl is installed but not started: it has no config yet.

  cp /etc/ilo-fanctl/config.example.yaml /etc/ilo-fanctl/config.yaml
  # edit it, and put a private key the BMC accepts at the path ilo.key_path
  # names (0600, owned by root)
  ilo-fanctl -config /etc/ilo-fanctl/config.yaml -check
  systemctl enable --now ilo-fanctl

Two prerequisites on the iLO side, both covered in
/usr/share/doc/ilo-fanctl/README.md: an iLO user with Virtual Power and Nodes
Control, and the ilo4_unlock patch. Without the patch the fan commands are
accepted and ignored, which looks like success from here.

EOF
fi
