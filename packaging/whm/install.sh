#!/bin/sh
# Install cprest as a WHM plugin on this cPanel server.
#
# Everything here is idempotent: running it again upgrades in place.
set -eu

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

SOURCE_DIR=$(cd "$(dirname "$0")" && pwd)
[ -f "$SOURCE_DIR/cprest-agent" ] || die "cprest-agent is not next to this script"
[ -f "$SOURCE_DIR/cprest.cgi" ] || die "cprest.cgi is not next to this script"

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
install -m 0755 "$SOURCE_DIR/cprest.cgi" "$CGI_DIR/cprest.cgi"
say "installed $PREFIX/cprest-agent and $CGI_DIR/cprest.cgi"

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

systemctl daemon-reload
systemctl enable cprest >/dev/null 2>&1 || true
systemctl restart cprest
say "service cprest started"

# --- WHM registration ------------------------------------------------------
# Both "acls" and "entryurl" matter here, and getting either wrong fails
# silently: WHM accepts the registration and simply never shows a menu item.
#
#   acls     is required, and must name real ACLs. A made-up value is
#            dropped. "software-cprest" is a name no reseller holds, and
#            root holds every ACL, so this is root-only and fails safe.
#   entryurl is what WHM actually links to from the Plugins section, and is
#            relative to /usr/local/cpanel/whostmgr/docroot/cgi/.
cat > "$APPCONFIG_DIR/cprest.conf" <<'APPCONFIG'
name=cprest
service=whostmgr
url=/cgi/cprest.cgi
entryurl=cprest.cgi
user=root
acls=software-cprest
displayname=cP:Restic Backups
icon=cprest.svg
searchtext=backup restore restic
target=_self
APPCONFIG

# The icon has to be in place before the registration that names it.
ICON_DIR=/usr/local/cpanel/whostmgr/docroot/addon_plugins
if [ -d "$ICON_DIR" ] && [ -f "$SOURCE_DIR/branding/cprestic-icon.svg" ]; then
    install -m 644 "$SOURCE_DIR/branding/cprestic-icon.svg" "$ICON_DIR/cprest.svg"
fi

/usr/local/cpanel/bin/register_appconfig "$APPCONFIG_DIR/cprest.conf"

# Confirm WHM kept what we sent. It accepts a registration with an invalid
# ACL by discarding the ACL, which leaves a plugin that exists and is
# invisible — so check rather than trust.
REGISTERED=$(whmapi1 get_appconfig_application_list 2>/dev/null |
    sed -n '/name: cprest$/,$p; /displayname: cP:Restic Backups/,/name: cprest/p')
for key in entryurl acls; do
    if ! printf '%s' "$REGISTERED" | grep -q "$key"; then
        cat >&2 <<PROBLEM

warning: WHM registered cprest without "$key".

The plugin will not appear in the WHM menu. Check the output of:

  whmapi1 get_appconfig_application_list

PROBLEM
    fi
done

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
       [ -f "$SOURCE_DIR/branding/cprestic-icon.png" ]; then
        install -m 644 "$SOURCE_DIR/branding/cprestic-icon.png" \
            "$FRONTEND/assets/application_icons/cprest.png"
    fi

    install -d -m 755 "$FRONTEND/dynamicui"
    cat > "$FRONTEND/dynamicui/dynamicui_cprest.conf" <<'MENU'
description=>Restore your files and databases from a backup,file=>cprest,group=>files,imgtype=>icon,itemdesc=>cP:Restic,itemorder=>1,subtype=>img,type=>image,url=>cprest/index.live.php,width=>48,height=>48
MENU
    echo "Installed the account-facing plugin in $FRONTEND/cprest."
else
    echo "warning: no jupiter theme at $FRONTEND; the account-facing plugin was not installed." >&2
fi

cat <<DONE

cP:Restic is installed.

  Open WHM and look for "cP:Restic Backups" in the plugins section.

Registered as:
$(sed 's/^/  /' "$APPCONFIG_DIR/cprest.conf")

It is registered against the "software-cprest" ACL, which no reseller holds
and root does, so it is root-only. The plugin also refuses any non-root WHM
user itself. Please still confirm in WHM that resellers cannot see it: this
plugin can read and delete every backup on the server.

If it does not appear, look in WHM under Development > Apps Managed by
AppConfig. The Manage Plugins page lists cPanel RPM addons and will not
show it.

Next: add a backup destination in the plugin, then a schedule.
DONE
