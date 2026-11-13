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
python3 - "$("$go_bin" env GOROOT)" <<'PY'
import hashlib, os, pathlib, subprocess, sys, tarfile

root = pathlib.Path('release/openwrt-arm64')
goroot = pathlib.Path(sys.argv[1])
vendor = goroot / 'src' / 'vendor'
go_bin = goroot / 'bin' / 'go'
dependencies = subprocess.check_output(
    [str(go_bin), 'list', '-deps', '-f', '{{.Dir}}', '.'], text=True,
    env={**os.environ, 'CGO_ENABLED': '0', 'GOOS': 'linux', 'GOARCH': 'arm64'}
).splitlines()
notices = {goroot / name for name in ('LICENSE', 'PATENTS') if (goroot / name).is_file()}
for dependency in map(pathlib.Path, dependencies):
    if vendor not in dependency.parents:
        continue
    for directory in (dependency, *dependency.parents):
        if directory == vendor:
            break
        notices.update(path for path in directory.iterdir()
                       if path.is_file() and path.name.startswith(('LICENSE', 'COPYING', 'NOTICE', 'PATENTS')))
(root/'THIRD-PARTY-NOTICES.txt').write_text(
    '\n\n'.join(str(path.relative_to(goroot)) + '\n\n' + path.read_text(encoding='utf-8').rstrip()
                 for path in sorted(notices)) + '\n', encoding='utf-8')
names = ['portal-local-proxy', 'portal-local-proxy.init', 'install-openwrt.sh', 'config.example.json',
         'README.md', 'LICENSE', 'THIRD-PARTY-NOTICES.txt']
(root/'SHA256SUMS').write_text(''.join(hashlib.sha256((root/n).read_bytes()).hexdigest()+'  '+n+'\n' for n in names))
with tarfile.open('release/portal-local-proxy-openwrt-arm64.tar.gz', 'w:gz') as package:
    for n in names + ['SHA256SUMS']:
        package.add(root/n, arcname='portal-local-proxy/'+n)
print('Created release/portal-local-proxy-openwrt-arm64.tar.gz')
PY
