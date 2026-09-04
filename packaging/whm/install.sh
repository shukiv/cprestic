#!/bin/sh
# Install cprest as a WHM plugin on this cPanel server.
#
# Everything here is idempotent: running it again upgrades in place.
set -eu

# This script runs as root. Do not let the invoking shell choose binaries
# such as install, systemctl, sed or restic from a writable directory.
PATH=/usr/local/cpanel/3rdparty/bin:/usr/local/cpanel/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH

PREFIX=/usr/local/bin
CONFIG_DIR=/etc/cprest
STATE_DIR=/var/lib/cprest
STAGING_DIR=/var/lib/cprest/staging
CACHE_DIR=/var/cache/cprest/restic
RUN_DIR=/var/run/cprest
APPCONFIG_DIR=/var/cpanel/apps
SERVICE=/etc/systemd/system/cprest.service

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || die "run this as root"
[ -d /usr/local/cpanel ] || die "this does not look like a cPanel server (/usr/local/cpanel is missing)"
umask 077

SOURCE_DIR=$(cd "$(dirname "$0")" && pwd)
[ -f "$SOURCE_DIR/cprest-agent" ] || die "cprest-agent is not next to this script"
[ -f "$SOURCE_DIR/cprest.cgi" ] || die "cprest.cgi is not next to this script"
[ -f "$SOURCE_DIR/cpanel/install.json" ] || die "cpanel/install.json is missing from the package"
[ -f "$SOURCE_DIR/cpanel/uapi/Cprest.pm" ] || die "the cPanel UAPI bridge is missing from the package"
[ -f "$SOURCE_DIR/cpanel/admin/Cprest/Session.pm" ] || die "the cPanel AdminBin bridge is missing from the package"
[ -f "$SOURCE_DIR/branding/cpr-badge.svg" ] || die "the cPanel plugin icon is missing from the package"
[ -d /var/cpanel/sessions/raw ] || die "cPanel's session store is missing (/var/cpanel/sessions/raw)"
[ -x /usr/local/cpanel/bin/uapi ] || die "cPanel's uapi command is missing"

# --- where WHM keeps plugin CGIs -------------------------------------------
# Sources disagree about this path and it has moved between versions, so it
# is probed rather than assumed.
CGI_DIR=""
for candidate in \
    /usr/local/cpanel/whostmgr/docroot/cgi \
    /usr/local/cpanel/cgi
do
    if [ -d "$candidate" ]; then CGI_DIR=$candidate; break; fi
done
[ -n "$CGI_DIR" ] || die "could not find WHM's plugin cgi directory; is this WHM?"
say "WHM plugin directory: $CGI_DIR"

# --- restic ----------------------------------------------------------------
if ! command -v restic >/dev/null 2>&1 && [ ! -x /usr/local/bin/restic ]; then
    cat <<'RESTIC'
error: restic is not installed.

  curl -L https://github.com/restic/restic/releases/download/v0.19.1/restic_0.19.1_linux_amd64.bz2 \
    | bunzip2 > /usr/local/bin/restic
  chmod 755 /usr/local/bin/restic

Then run this installer again.
RESTIC
    exit 1
fi
say "restic: $(restic version 2>/dev/null | head -1)"

# --- files -----------------------------------------------------------------
install -d -m 0700 "$CONFIG_DIR" "$STATE_DIR" "$STAGING_DIR" "$CACHE_DIR" "$RUN_DIR"
install -d -m 0755 "$APPCONFIG_DIR"
install -m 0755 "$SOURCE_DIR/cprest-agent" "$PREFIX/cprest-agent"
HOOK_BIN=/usr/local/cpanel/3rdparty/bin/cprest-hook
install -m 0755 "$SOURCE_DIR/cprest-agent" "$HOOK_BIN"
install -m 0755 "$SOURCE_DIR/cprest.cgi" "$CGI_DIR/cprest.cgi"
say "installed $PREFIX/cprest-agent and $CGI_DIR/cprest.cgi"

# LivePHP intentionally does not expose the browser's cpsession cookie. The
# UAPI module enters cPanel's authenticated engine; its root-owned AdminBin
# counterpart restores the matching server-side session and exchanges that
# opaque proof with cprest over the root-only admin socket. An account process
# calling the module outside a live cPanel session has no session to restore.
CPANEL_UAPI_DIR=/usr/local/cpanel/Cpanel/API
CPANEL_ADMIN_DIR=/var/cpanel/perl/Cpanel/Admin/Modules/Cprest
install -d -o root -g root -m 0755 "$CPANEL_ADMIN_DIR"
install -o root -g root -m 0644 "$SOURCE_DIR/cpanel/uapi/Cprest.pm" \
    "$CPANEL_UAPI_DIR/Cprest.pm"
install -o root -g root -m 0700 "$SOURCE_DIR/cpanel/admin/Cprest/Session.pm" \
    "$CPANEL_ADMIN_DIR/Session.pm"
say "installed cPanel UAPI/AdminBin session bridge"

# cPanel account lifecycle integration. Standardized Hooks are the
# supported registry; do not edit /var/cpanel/hooks/data directly. Remove
# the old manual post-hook registrations first when upgrading from a release
# before --describe support, then let the executable register all current
# descriptors. cPanel requires descriptor registration for a blocking hook.
MANAGE_HOOKS=/usr/local/cpanel/bin/manage_hooks
[ -x "$MANAGE_HOOKS" ] || die "cPanel's manage_hooks utility is missing"
for hook in "Accounts::Create:create" "Accounts::Modify:modify" "Accounts::Remove:remove"; do
    event=${hook%:*}
    action=${hook#*:}
    "$MANAGE_HOOKS" delete script "$HOOK_BIN" --manual \
        --category Whostmgr --event "$event" --stage post \
        --action="--cpanel-hook=$action" >/dev/null 2>&1 || true
done
"$MANAGE_HOOKS" delete script "$HOOK_BIN" >/dev/null 2>&1 || true
"$MANAGE_HOOKS" add script "$HOOK_BIN"
say "registered cPanel account lifecycle hooks"

if [ -f "$SOURCE_DIR/cprest.png" ]; then
    install -m 0644 "$SOURCE_DIR/cprest.png" "$CGI_DIR/cprest.png" || true
fi

# --- service ---------------------------------------------------------------
cat > "$SERVICE" <<'UNIT'
[Unit]
Description=cprest backups
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# pkgacct and reading every account's home directory both need root.
#
# HOME matters more than it looks: cPanel keeps root's MySQL credentials in
# /root/.my.cnf, and systemd does not set HOME by default. Without it every
# "SHOW DATABASES" fails with "Access denied", and an account's databases
# would be missing from its backup.
Environment=HOME=/root
UMask=0077
ExecStart=/usr/local/bin/cprest-agent -standalone
Restart=always
RestartSec=15
RuntimeDirectory=cprest
RuntimeDirectoryMode=0700
NoNewPrivileges=true
ProtectKernelTunables=true
ProtectControlGroups=true

[Install]
WantedBy=multi-user.target
UNIT
chown root:root "$SERVICE"
chmod 0644 "$SERVICE"

systemctl daemon-reload
systemctl enable cprest >/dev/null 2>&1 || true
systemctl restart cprest
say "service cprest started"

# --- WHM registration ------------------------------------------------------
# Both "acls" and "entryurl" matter here, and getting either wrong fails
# silently: WHM accepts the registration and simply never shows a menu item.
#
#   acls     is required, and must name a real ACL. cPanel documents "all"
#            as its root-level ACL. The CGI independently calls hasroot on
#            every request, so menu registration is not the trust boundary.
#   entryurl is what WHM actually links to from the Plugins section, and is
#            relative to /usr/local/cpanel/whostmgr/docroot/cgi/.
cat > "$APPCONFIG_DIR/cprest.conf" <<'APPCONFIG'
name=cprest
service=whostmgr
url=/cgi/cprest.cgi
entryurl=cprest.cgi
user=root
acls=all
displayname=cP:Restic Backups
icon=cprest.svg
searchtext=backup restore restic
target=_self
APPCONFIG
chown root:root "$APPCONFIG_DIR/cprest.conf"
chmod 0644 "$APPCONFIG_DIR/cprest.conf"

# The icon has to be in place before the registration that names it.
ICON_DIR=/usr/local/cpanel/whostmgr/docroot/addon_plugins
if [ -d "$ICON_DIR" ] && [ -f "$SOURCE_DIR/branding/cpr-badge.svg" ]; then
    install -m 644 "$SOURCE_DIR/branding/cpr-badge.svg" "$ICON_DIR/cprest.svg"
fi

/usr/local/cpanel/bin/register_appconfig "$APPCONFIG_DIR/cprest.conf"

# Confirm WHM kept what we sent. It accepts a registration with an invalid
# ACL by discarding the ACL, which leaves a plugin that exists and is
# invisible — so check rather than trust.
CPANEL_PERL=/usr/local/cpanel/3rdparty/bin/perl
[ -x "$CPANEL_PERL" ] || die "cPanel's bundled perl is missing"
if ! /usr/local/cpanel/bin/whmapi1 --output=json get_appconfig_application_list 2>/dev/null |
    "$CPANEL_PERL" -MJSON::PP -e '
        local $/;
        my $response = decode_json(<STDIN>);
        for my $app (@{$response->{data}{whostmgr} || []}) {
            next unless (($app->{name} || "") eq "cprest");
            my %acls = map { $_ => 1 } @{$app->{acls} || []};
            exit((($app->{entryurl} || "") eq "cprest.cgi" && $acls{all}) ? 0 : 2);
        }
        exit 3;
    '
then
    cat >&2 <<PROBLEM

warning: WHM did not retain cprest's entry URL and root-level ACL.

The plugin will not appear in the WHM menu. Check the output of:

  /usr/local/cpanel/bin/whmapi1 get_appconfig_application_list

PROBLEM
fi

# ---------------------------------------------------------------------------
# The account-facing plugin.
#
# cPanel runs these as the account they belong to, which is the whole
# point: the service reads who is asking from the socket rather than from
# anything the page says. Installed by copying, because a plugin that is
# four PHP files and one menu entry does not need a tarball and a script
# that unpacks it.
FRONTEND=/usr/local/cpanel/base/frontend/jupiter
if [ -d "$FRONTEND" ]; then
    install -d -m 755 "$FRONTEND/cprest"
    for page in index.live.php browse.live.php restore.live.php download.live.php proxy.php; do
        if [ -f "$SOURCE_DIR/cpanel/$page" ]; then
            install -m 644 "$SOURCE_DIR/cpanel/$page" "$FRONTEND/cprest/$page"
        fi
    done

    # cPanel draws a plugin's tile from assets/application_icons/<file>.png,
    # where <file> is what the menu entry below calls itself.
    if [ -d "$FRONTEND/assets/application_icons" ] &&
       [ -f "$SOURCE_DIR/branding/cpr-badge-48.png" ]; then
        install -m 644 "$SOURCE_DIR/branding/cpr-badge-48.png" \
            "$FRONTEND/assets/application_icons/cprest.png"
        # Jupiter draws the tile from a sprite sheet, not from that file:
        # the page asks for .icon-cprest, a background-position into
        # icon_spritemap.png. Replacing the icon without rebuilding the
        # sheet changes nothing an account can see, and the sheet's URL
        # carries a content hash, so a stale one is cached hard.
        if [ -x /usr/local/cpanel/bin/sprite_generator ]; then
            /usr/local/cpanel/bin/sprite_generator \
                --application=cpanel --theme=jupiter >/dev/null 2>&1 || true
        fi
    fi

    # Register through cPanel's supported plugin installer. Besides keeping
    # the DynamicUI cache consistent, install.json registers "cprest" with
    # Feature Manager, so a host can withhold backup access from a package
    # instead of every account receiving it unconditionally.
    [ -x /usr/local/cpanel/scripts/install_plugin ] || \
        die "/usr/local/cpanel/scripts/install_plugin is missing"
    PLUGIN_META=$(mktemp -d /var/tmp/cprest-cpanel.XXXXXX)
    trap 'if [ -n "${PLUGIN_META:-}" ]; then rm -rf -- "$PLUGIN_META"; fi' 0 1 2 15
    install -m 0644 "$SOURCE_DIR/cpanel/install.json" "$PLUGIN_META/install.json"
    install -m 0644 "$SOURCE_DIR/branding/cpr-badge-48.png" "$PLUGIN_META/cprest.png"
    # Remove the entry written by older releases; if left behind it can
    # override the Feature Manager-aware entry generated below.
    DYNAMICUI="$FRONTEND/dynamicui/dynamicui_cprest.conf"
    rm -f "$DYNAMICUI"
    # Jupiter prefers an SVG over a PNG with the same application id. cPanel's
    # SVG installer rewrites geometry attributes in custom artwork, so remove
    # the generated SVG left by older releases and register the exact 48px PNG.
    rm -f "$FRONTEND/assets/application_icons/cprest.svg"
    # This command creates only public plugin metadata. Keep the restrictive
    # process-wide umask for service state and credentials, but do not pass it
    # into cPanel's registration writer.
    ( umask 022; /usr/local/cpanel/scripts/install_plugin "$PLUGIN_META" --theme=jupiter )
    [ -f "$DYNAMICUI" ] || die "cPanel did not generate $DYNAMICUI"
    # install_plugin inherits this script's security-first umask (077), but
    # Jupiter reads DynamicUI records while building an account's application
    # list. A root-only record is silently omitted even when Feature Manager
    # enables cprest for the account. Plugin metadata is public, like every
    # other record in this directory, so make the generated file readable.
    chown root:root "$DYNAMICUI"
    chmod 0644 "$DYNAMICUI"
    # Permission changes update ctime, while cPanel invalidates its parsed
    # per-account application cache using this record's mtime.
    touch "$DYNAMICUI"
    rm -rf -- "$PLUGIN_META"
    PLUGIN_META=
    trap - 0 1 2 15
    echo "Installed the account-facing plugin in $FRONTEND/cprest."
else
    echo "warning: no jupiter theme at $FRONTEND; the account-facing plugin was not installed." >&2
fi

cat <<DONE

cP:Restic is installed.

  Open WHM and look for "cP:Restic Backups" in the plugins section.

Registered as:
$(sed 's/^/  /' "$APPCONFIG_DIR/cprest.conf")

It is registered against cPanel's root-level "all" ACL. The plugin also
checks that ACL itself on every request. Please still confirm in WHM that
only administrators with root-level privileges can see it: this plugin can
read and delete every backup on the server.

If it does not appear, look in WHM under Development > Apps Managed by
AppConfig. The Manage Plugins page lists cPanel RPM addons and will not
show it.

Next: add a backup destination in the plugin, then a schedule.
DONE
