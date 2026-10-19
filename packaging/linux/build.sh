#!/bin/sh
# Packages a built Linux mobd: packaging/linux/build.sh VERSION ARCH BINARY OUTDIR
# ARCH is amd64 or arm64. Writes mobd_VERSION_linux_ARCH.tar.gz and mobd_VERSION_ARCH.deb.
set -eu
[ "$#" -eq 4 ] || { echo "usage: $0 VERSION amd64|arm64 BINARY OUTDIR" >&2; exit 2; }
version=$1 arch=$2 binary=$3 out=$4
case "$arch" in amd64|arm64) ;; *) echo "unknown arch $arch" >&2; exit 2 ;; esac
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
mkdir -p "$out"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Tarball: the binary and its documents, as on macOS.
mkdir "$work/tar"
install -m 755 "$binary" "$work/tar/mobd"
cp "$root/LICENSE" "$root/README.md" "$work/tar/"
tar --owner=0 --group=0 --numeric-owner -czf "$out/mobd_${version}_linux_${arch}.tar.gz" -C "$work/tar" mobd LICENSE README.md

# Debian package.
pkg="$work/deb"
install -d -m 755 "$pkg/DEBIAN" "$pkg/usr/bin" "$pkg/usr/share/doc/mobd"
install -m 755 "$binary" "$pkg/usr/bin/mobd"
install -m 644 "$root/README.md" "$pkg/usr/share/doc/mobd/README.md"
{ printf 'Format: https://www.debian.org/doc/packaging-manuals/copyright-format/1.0/\nUpstream-Name: mobd\nSource: https://github.com/xrdavies/mobd\n\nFiles: *\nLicense: MIT\n'; sed 's/^/ /; s/^ $/ ./' "$root/LICENSE"; } > "$pkg/usr/share/doc/mobd/copyright"
chmod 644 "$pkg/usr/share/doc/mobd/copyright"
sed "s/@VERSION@/$version/; s/@ARCH@/$arch/" "$here/deb/control.in" > "$pkg/DEBIAN/control"
size=$(du -sk "$pkg/usr" | cut -f1)
echo "Installed-Size: $size" >> "$pkg/DEBIAN/control"
install -m 755 "$here/deb/prerm" "$pkg/DEBIAN/prerm"
dpkg-deb --root-owner-group --build "$pkg" "$out/mobd_${version}_${arch}.deb" >/dev/null
echo "$out/mobd_${version}_linux_${arch}.tar.gz"
echo "$out/mobd_${version}_${arch}.deb"
