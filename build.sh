#!/bin/sh
set -eu
cd "$(dirname "$0")"
go_bin=${GO_BIN:-go}
export GOTOOLCHAIN=local
"$go_bin" test ./...
"$go_bin" vet ./...
mkdir -p bin release/openwrt-arm64
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 "$go_bin" build -trimpath -ldflags='-s -w' -o bin/portal-local-proxy-darwin-arm64 .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 "$go_bin" build -trimpath -ldflags='-s -w' -o release/openwrt-arm64/portal-local-proxy .
cp deploy/portal-local-proxy.init deploy/install-openwrt.sh config.example.json README.md LICENSE release/openwrt-arm64/
python3 - <<'PY'
import hashlib, pathlib, tarfile
root = pathlib.Path('release/openwrt-arm64')
names = ['portal-local-proxy', 'portal-local-proxy.init', 'install-openwrt.sh', 'config.example.json', 'README.md', 'LICENSE']
(root/'SHA256SUMS').write_text(''.join(hashlib.sha256((root/n).read_bytes()).hexdigest()+'  '+n+'\n' for n in names))
with tarfile.open('release/portal-local-proxy-openwrt-arm64.tar.gz', 'w:gz') as package:
    for n in names + ['SHA256SUMS']:
        package.add(root/n, arcname='portal-local-proxy/'+n)
print('Created release/portal-local-proxy-openwrt-arm64.tar.gz')
PY
