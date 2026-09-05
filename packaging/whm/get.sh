#!/bin/sh
# Put cP:Restic on this cPanel server, from a published release.
#
#   curl -fsSL https://github.com/shukiv/cprestic/releases/latest/download/get.sh | sh
#
# It downloads one tarball, checks it against the checksums published beside
# it, unpacks it into a temporary directory and hands over to the installer
# inside. Nothing is left behind but what that installer puts in place.
#
# The checksum proves the download arrived whole. It does not prove who built
# it: both files come from the same release page, so anyone who could replace
# one could replace the other. This script is short so that reading it before
# running it as root is a minute's work rather than an act of faith.
#
# CPREST_VERSION=v1.2.3   install that release instead of the newest
# CPREST_TARBALL=/path    install a tarball already on this machine
set -eu

REPO=shukiv/cprestic
RELEASES=https://github.com/$REPO/releases

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || die "run this as root"
[ -d /usr/local/cpanel ] || die "this does not look like a cPanel server (/usr/local/cpanel is missing)"

case "$(uname -m)" in
    x86_64) ARCH=amd64 ;;
    *)      die "the published builds are x86-64 only; on $(uname -m), build from source with 'make plugin'" ;;
esac
TARBALL=cprest-plugin-$ARCH.tar.gz

if [ -n "${CPREST_VERSION:-}" ]; then
    BASE=$RELEASES/download/$CPREST_VERSION
else
    BASE=$RELEASES/latest/download
fi

for tool in curl tar sha256sum; do
    command -v "$tool" >/dev/null 2>&1 || die "$tool is needed; install it and run this again"
done

WORK=$(mktemp -d /var/tmp/cprest-install.XXXXXX)
trap 'rm -rf -- "$WORK"' 0 1 2 15

if [ -n "${CPREST_TARBALL:-}" ]; then
    say "installing from $CPREST_TARBALL"
    cp "$CPREST_TARBALL" "$WORK/$TARBALL"
else
    say "downloading $BASE/$TARBALL"
    curl -fsSL -o "$WORK/$TARBALL" "$BASE/$TARBALL" \
        || die "could not download $BASE/$TARBALL"
    curl -fsSL -o "$WORK/SHA256SUMS" "$BASE/SHA256SUMS" \
        || die "could not download $BASE/SHA256SUMS"

    # Only the line for the file actually downloaded: the rest of that file
    # names things this machine did not fetch.
    grep " $TARBALL\$" "$WORK/SHA256SUMS" > "$WORK/expected" \
        || die "the published checksums do not mention $TARBALL"
    ( cd "$WORK" && sha256sum -c expected ) >/dev/null \
        || die "the download does not match its published checksum; nothing was installed"
    say "checksum ok"
fi

tar -C "$WORK" -xzf "$WORK/$TARBALL"
[ -f "$WORK/cprest-plugin/install.sh" ] || die "that tarball has no cprest-plugin/install.sh in it"

# Run it through sh rather than as a program. cPanel mounts /tmp and /var/tmp
# noexec, so a script unpacked there cannot be executed even by root even
# though its mode says otherwise. Nothing in the package is executed from
# here; the installer copies its files into place with install(1).
say "running the installer"
sh "$WORK/cprest-plugin/install.sh"
