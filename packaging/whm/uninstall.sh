#!/bin/sh
# Remove the Gniza WHM plugin. Backups already stored remotely are not
# touched, and neither is the local state file, so a reinstall picks up
# where this left off.
set -eu

PATH=/usr/local/cpanel/3rdparty/bin:/usr/local/cpanel/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH

[ "$(id -u)" = 0 ] || { echo "run this as root" >&2; exit 1; }

SOURCE_DIR=$(cd "$(dirname "$0")" && pwd)

systemctl stop gniza 2>/dev/null || true
systemctl disable gniza 2>/dev/null || true
rm -f /etc/systemd/system/gniza.service
systemctl daemon-reload 2>/dev/null || true

if [ -x /usr/local/cpanel/bin/unregister_appconfig ]; then
    /usr/local/cpanel/bin/unregister_appconfig Gniza || true
fi
rm -f /var/cpanel/apps/gniza.conf
rm -f /usr/local/cpanel/whostmgr/docroot/cgi/gniza.cgi /usr/local/cpanel/cgi/gniza.cgi

if [ -x /usr/local/cpanel/bin/manage_hooks ]; then
    # Current releases describe every registration, including the blocking
    # pre-remove hook, from the installed executable.
    if [ -x /usr/local/cpanel/3rdparty/bin/gniza-hook ]; then
        /usr/local/cpanel/bin/manage_hooks delete script /usr/local/cpanel/3rdparty/bin/gniza-hook \
            >/dev/null 2>&1 || true
    fi
    # Also clean up post hooks from releases that registered manually.
    for hook in "Accounts::Create:create" "Accounts::Modify:modify" "Accounts::Remove:remove"; do
        event=${hook%:*}
        action=${hook#*:}
        /usr/local/cpanel/bin/manage_hooks delete script /usr/local/cpanel/3rdparty/bin/gniza-hook --manual \
            --category Whostmgr --event "$event" --stage post \
            --action="--cpanel-hook=$action" >/dev/null 2>&1 || true
    done
fi
rm -f /usr/local/cpanel/3rdparty/bin/gniza-hook
rm -f /usr/local/bin/gniza-agent
rm -f /usr/local/cpanel/Cpanel/API/Gniza.pm
rm -f /var/cpanel/perl/Cpanel/Admin/Modules/Gniza/Session.pm
rmdir /var/cpanel/perl/Cpanel/Admin/Modules/Gniza 2>/dev/null || true

# Remove the account-facing registration with the same supported mechanism
# used at install time. Older releases wrote DynamicUI directly, so that
# exact legacy file is removed as well.
FRONTEND=/usr/local/cpanel/base/frontend/jupiter
if [ -x /usr/local/cpanel/scripts/uninstall_plugin ] &&
   [ -f "$SOURCE_DIR/cpanel/install.json" ] &&
   [ -f "$SOURCE_DIR/branding/badge.svg" ]; then
    PLUGIN_META=$(mktemp -d /var/tmp/gniza-cpanel.XXXXXX)
    trap 'if [ -n "${PLUGIN_META:-}" ]; then rm -rf -- "$PLUGIN_META"; fi' 0 1 2 15
    install -m 0644 "$SOURCE_DIR/cpanel/install.json" "$PLUGIN_META/install.json"
    install -m 0644 "$SOURCE_DIR/branding/badge.svg" "$PLUGIN_META/gniza.svg"
    /usr/local/cpanel/scripts/uninstall_plugin "$PLUGIN_META" --theme=jupiter || true
    rm -rf -- "$PLUGIN_META"
    PLUGIN_META=
    trap - 0 1 2 15
fi
rm -f "$FRONTEND/dynamicui/dynamicui_gniza.conf"
rm -rf -- "$FRONTEND/gniza"
rm -f "$FRONTEND/assets/application_icons/gniza.png" \
    /usr/local/cpanel/whostmgr/docroot/addon_plugins/gniza.svg

# restic's cache. It is rebuilt from the repository on the next backup, and
# it is the one thing here that can reach gigabytes, so leaving it behind is
# not caution, only clutter.
rm -rf -- /var/cache/gniza

# Account events the hooks left for a service that is now going away. The
# hooks are unregistered above, so nothing will add more and nothing will
# ever read these; reinstalling later must not replay account changes from
# whenever Gniza was last installed.
rm -rf -- /var/lib/gniza/hooks

cat <<'DONE'
Gniza removed.

Left in place on purpose:
  /etc/gniza/master.key   the key that decrypts your stored credentials
  /var/lib/gniza/state.db your destinations, schedules and history

Delete those only when you are certain you will not reinstall, and never
before you have another way to read your backups: without the key file the
stored destination credentials cannot be recovered.

Reinstalling picks up from there: the same destinations, schedules and
history come back with it.
DONE

# Last, because this script is reading itself out of that directory: the copy
# of the uninstaller the installer left behind, and the two cPanel files it
# needs to remove the account-facing tile.
rm -rf -- /usr/local/share/gniza
