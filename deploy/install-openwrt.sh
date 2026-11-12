#!/bin/sh
# First installation only. Does not alter firewall, network, DNS or Nikki.
set -eu
package_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
configuration=${1:?Usage: sh install-openwrt.sh /absolute/path/config.json}
case "$configuration" in /*) ;; *) echo 'Use an absolute configuration path.' >&2; exit 1;; esac
[ "$(id -u)" = 0 ] || { echo 'Root is required for service installation.' >&2; exit 1; }
[ "$(uname -m)" = aarch64 ] || { echo 'This package is for aarch64 OpenWrt only.' >&2; exit 1; }
[ -f /etc/rc.common ] && command -v procd >/dev/null && id nobody >/dev/null 2>&1 || {
    echo 'OpenWrt procd and user nobody are required.' >&2; exit 1;
}
for target in /usr/libexec/portal-local-proxy /etc/portal-local-proxy /etc/init.d/portal-local-proxy; do
    [ ! -e "$target" ] || { echo "Existing installation found: $target; no changes made." >&2; exit 1; }
done
(cd "$package_dir" && sha256sum -c SHA256SUMS >/dev/null)
"$package_dir/portal-local-proxy" -config "$configuration" -check

mkdir -p /usr/libexec/portal-local-proxy /etc/portal-local-proxy
cp "$package_dir/portal-local-proxy" /usr/libexec/portal-local-proxy/portal-local-proxy
cp "$configuration" /etc/portal-local-proxy/config.json
cp "$package_dir/portal-local-proxy.init" /etc/init.d/portal-local-proxy
chmod 755 /usr/libexec/portal-local-proxy /etc/portal-local-proxy /usr/libexec/portal-local-proxy/portal-local-proxy /etc/init.d/portal-local-proxy
chmod 644 /etc/portal-local-proxy/config.json

record=/etc/portal-local-proxy/install-record.txt
{ date -u '+installed_utc=%Y-%m-%dT%H:%M:%SZ'; sha256sum /usr/libexec/portal-local-proxy/portal-local-proxy /etc/portal-local-proxy/config.json /etc/init.d/portal-local-proxy; } > "$record"
chmod 644 "$record"
/etc/init.d/portal-local-proxy start
sleep 2
if /etc/init.d/portal-local-proxy running; then
    /etc/init.d/portal-local-proxy enable
    echo 'Portal proxy is running and enabled at boot.'
else
    /etc/init.d/portal-local-proxy stop || true
    /etc/init.d/portal-local-proxy disable || true
    echo 'Startup failed; service stopped and disabled. Files retained for diagnosis.' >&2
    exit 1
fi
