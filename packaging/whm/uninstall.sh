#!/bin/sh
# Remove the cprest WHM plugin. Backups already stored remotely are not
# touched, and neither is the local state file, so a reinstall picks up
# where this left off.
set -eu

PATH=/usr/local/cpanel/3rdparty/bin:/usr/local/cpanel/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH

[ "$(id -u)" = 0 ] || { echo "run this as root" >&2; exit 1; }

SOURCE_DIR=$(cd "$(dirname "$0")" && pwd)

systemctl stop cprest 2>/dev/null || true
systemctl disable cprest 2>/dev/null || true
rm -f /etc/systemd/system/cprest.service
systemctl daemon-reload 2>/dev/null || true

if [ -x /usr/local/cpanel/bin/unregister_appconfig ]; then
    /usr/local/cpanel/bin/unregister_appconfig cprest || true
fi
rm -f /var/cpanel/apps/cprest.conf
rm -f /usr/local/cpanel/whostmgr/docroot/cgi/cprest.cgi /usr/local/cpanel/cgi/cprest.cgi

if [ -x /usr/local/cpanel/bin/manage_hooks ]; then
    # Current releases describe every registration, including the blocking
    # pre-remove hook, from the installed executable.
    if [ -x /usr/local/cpanel/3rdparty/bin/cprest-hook ]; then
        /usr/local/cpanel/bin/manage_hooks delete script /usr/local/cpanel/3rdparty/bin/cprest-hook \
            >/dev/null 2>&1 || true
    fi
    # Also clean up post hooks from releases that registered manually.
    for hook in "Accounts::Create:create" "Accounts::Modify:modify" "Accounts::Remove:remove"; do
        event=${hook%:*}
        action=${hook#*:}
        /usr/local/cpanel/bin/manage_hooks delete script /usr/local/cpanel/3rdparty/bin/cprest-hook --manual \
            --category Whostmgr --event "$event" --stage post \
            --action="--cpanel-hook=$action" >/dev/null 2>&1 || true
    done
fi
rm -f /usr/local/cpanel/3rdparty/bin/cprest-hook
rm -f /usr/local/bin/cprest-agent
rm -f /usr/local/cpanel/Cpanel/API/Cprest.pm
rm -f /var/cpanel/perl/Cpanel/Admin/Modules/Cprest/Session.pm
rmdir /var/cpanel/perl/Cpanel/Admin/Modules/Cprest 2>/dev/null || true

# Remove the account-facing registration with the same supported mechanism
# used at install time. Older releases wrote DynamicUI directly, so that
# exact legacy file is removed as well.
FRONTEND=/usr/local/cpanel/base/frontend/jupiter
if [ -x /usr/local/cpanel/scripts/uninstall_plugin ] &&
   [ -f "$SOURCE_DIR/cpanel/install.json" ] &&
   [ -f "$SOURCE_DIR/branding/cprestic-icon.svg" ]; then
    PLUGIN_META=$(mktemp -d /var/tmp/cprest-cpanel.XXXXXX)
    trap 'if [ -n "${PLUGIN_META:-}" ]; then rm -rf -- "$PLUGIN_META"; fi' 0 1 2 15
    install -m 0644 "$SOURCE_DIR/cpanel/install.json" "$PLUGIN_META/install.json"
    install -m 0644 "$SOURCE_DIR/branding/cprestic-icon.svg" "$PLUGIN_META/cprest.svg"
    /usr/local/cpanel/scripts/uninstall_plugin "$PLUGIN_META" --theme=jupiter || true
    rm -rf -- "$PLUGIN_META"
    PLUGIN_META=
    trap - 0 1 2 15
fi
rm -f "$FRONTEND/dynamicui/dynamicui_cprest.conf"
rm -rf -- "$FRONTEND/cprest"
rm -f "$FRONTEND/assets/application_icons/cprest.png" \
    /usr/local/cpanel/whostmgr/docroot/addon_plugins/cprest.svg

cat <<'DONE'
cprest removed.

Left in place on purpose:
  /etc/cprest/master.key   the key that decrypts your stored credentials
  /var/lib/cprest/state.db your destinations, schedules and history

Delete those only when you are certain you will not reinstall, and never
before you have another way to read your backups: without the key file the
stored destination credentials cannot be recovered.
DONE
