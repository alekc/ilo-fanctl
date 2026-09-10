#!/bin/sh
# Runs after install and after upgrade, on both deb and rpm.
set -e

config=/etc/ilo-fanctl/config.yaml

have_systemctl() {
	command -v systemctl >/dev/null 2>&1
}

# A container or chroot has no systemd, and a package that fails to install
# there for want of it is worse than one that installs and does nothing.
if have_systemctl; then
	systemctl daemon-reload >/dev/null 2>&1 || true
fi

running=no
if have_systemctl && systemctl is-active --quiet ilo-fanctl 2>/dev/null; then
	running=yes
fi

# Four states, and each wants something different. Both conditions are read
# because neither answers on its own: whether the service is up decides
# between an upgrade and a first install, and whether a config exists decides
# whether starting it can succeed.
if [ "$running" = yes ] && [ -f "$config" ]; then
	# The ordinary upgrade. Replacing the binary does not change the running
	# process: it holds the old inode until something restarts it. Without
	# this an upgrade looks like it worked while the machine keeps running the
	# old build, which is the exact failure ilo_fanctl_build_info exists to
	# make visible.
	systemctl restart ilo-fanctl || true

elif [ "$running" = yes ]; then
	# Running, but the config is gone. It is read once at startup, so a file
	# deleted since then is still live in memory and this machine is still
	# being cooled. Restarting would exit in config.Load, and Restart=always
	# turns that into a crash loop: no controller at all, every fan back on
	# iLO's own curve, which is the under-cooling this tool exists to fix.
	# Keeping a stale build running beats replacing it with nothing.
	cat <<EOF

ilo-fanctl is running but $config is missing, so it was NOT restarted.

The running process still holds the config it started with and is still
driving the fans. Restarting it now would fail before it reached the BMC.
Put the config back, then restart by hand:

  systemctl restart ilo-fanctl

Until then this machine keeps running the previous build.

EOF

elif [ ! -f "$config" ]; then
	# A first install, or near enough. Deliberately neither enabled nor
	# started: this daemon cannot run without a config and a private key the
	# BMC accepts, and a package can ship neither. Starting it here would
	# leave a restart loop on a machine whose owner has configured nothing.
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

# The fourth state, stopped with a config present, says nothing on purpose.
# Somebody stopped this service, and an upgrade is not the moment to reopen
# that decision or to lecture them about it.
