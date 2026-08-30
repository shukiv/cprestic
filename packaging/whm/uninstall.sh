#!/bin/sh
# Remove the cprest WHM plugin. Backups already stored remotely are not
# touched, and neither is the local state file, so a reinstall picks up
# where this left off.
set -eu

[ "$(id -u)" = 0 ] || { echo "run this as root" >&2; exit 1; }

systemctl stop cprest 2>/dev/null || true
systemctl disable cprest 2>/dev/null || true
rm -f /etc/systemd/system/cprest.service
systemctl daemon-reload 2>/dev/null || true

if [ -x /usr/local/cpanel/bin/unregister_appconfig ]; then
    /usr/local/cpanel/bin/unregister_appconfig cprest || true
fi
rm -f /var/cpanel/apps/cprest.conf
rm -f /usr/local/cpanel/whostmgr/docroot/cgi/cprest.cgi /usr/local/cpanel/cgi/cprest.cgi
rm -f /usr/local/bin/cprest-agent

cat <<'DONE'
cprest removed.

Left in place on purpose:
  /etc/cprest/master.key   the key that decrypts your stored credentials
  /var/lib/cprest/state.db your destinations, schedules and history

Delete those only when you are certain you will not reinstall, and never
before you have another way to read your backups: without the key file the
stored destination credentials cannot be recovered.
DONE
