# 安装与配置

## 1. 在电脑上检查源码

在源码根目录使用 Go 1.24 或更新版本。核心程序无第三方 Go 依赖。

```sh
go test ./...
go vet ./...
go build -trimpath -o bin/portal-local-proxy .
./bin/portal-local-proxy -config config.example.json -check
```

也可去掉最后一条命令的 `-check` 做本机试运行。样例只监听 `127.0.0.1:18081`，不供局域网设备访问；按 Ctrl+C 退出。本机测试不会配置路由器。

## 2. 准备旁路由配置

以下地址仅为示例。将 `listen` 替换为旁路由真实 LAN 地址，将 `allowed_clients` 替换为掌机在旁路由看来实际使用的地址。建议在现有 DHCP 管理端为掌机保留地址，保持单设备 `/32`，不要放开整个子网。配置文件不要上传到 GitHub。

```json
{
  "listen": "192.168.88.2:18081",
  "allowed_clients": ["192.168.88.50/32"],
  "probe_mode": "forward",
  "upstream": ""
}
```

- 省略 `allowed_domains` 时使用内置白名单。确有需要才为白名单增加经过核实的域名；自定义列表会替换默认列表。
- `upstream` 留空时本服务不指定上游，设备原有透明代理仍可能处理出站。需要借用同机已有 HTTP 代理时，可填 `http://127.0.0.1:7890`，端口以自己的配置为准。不支持在此字段填写账号、路径或订阅链接。
- 保持 `probe_mode: forward`，让真实服务器回答联网检测。不要把检测成功当作 PSN 或串流成功。

## 3. 安装到 ARM64 OpenWrt

要求旁路由为 `aarch64`，使用 OpenWrt `procd`，且已有 `nobody` 用户。先确认 `18081` 未被占用、掌机可访问旁路由 LAN 地址。本安装器仅用于首次安装，发现已有安装会拒绝覆盖。

### 下载并校验

先在旁路由运行 `uname -m`，确认输出是 `aarch64`。然后在电脑打开 [v0.1.0 Releases](https://github.com/SOSO-245/portal-local-proxy/releases/tag/v0.1.0)，从 Assets 下载：

- `portal-local-proxy-openwrt-arm64.tar.gz`
- `portal-local-proxy-openwrt-arm64.tar.gz.sha256`

在下载目录校验，看到 `OK` 才继续。Linux/OpenWrt 用 `sha256sum`，macOS 用 `shasum`，二选一：

```sh
# Linux / OpenWrt
sha256sum -c portal-local-proxy-openwrt-arm64.tar.gz.sha256
# macOS
shasum -a 256 -c portal-local-proxy-openwrt-arm64.tar.gz.sha256
```

校验失败时不要执行安装脚本。校验文件用于检查传输完整性，不代替从官方项目仓库获取文件。GitHub 自动提供的 Source code 压缩包不是可直接安装的 ARM64 包。

### 解压并首次安装

将已校验的安装包和自己准备的配置上传到旁路由 `/tmp`（配置命名为 `portal-config.json`），以管理员身份在旁路由终端执行：

```sh
cd /tmp
tar -xzf portal-local-proxy-openwrt-arm64.tar.gz
cd portal-local-proxy
sh install-openwrt.sh /tmp/portal-config.json
/etc/init.d/portal-local-proxy status
logread -e portal-local-proxy
```

安装器检查包内校验和及配置，启动成功才开启自启动。不改网关、DNS、无线或防火墙，不安装上游代理。如果包通过网络取得，包内校验和只能检错，仍需从可信来源取得包。

需要自行构建时，可在公开源码目录用 Go、Python 3 和 POSIX shell 执行 `sh build.sh`，生成 `release/portal-local-proxy-openwrt-arm64.tar.gz`。

## 4. 配置掌机并验证

在 PS Portal 当前 Wi-Fi 的手动或高级网络设置中启用代理，填写旁路由 LAN 地址和服务端口，例如 `192.168.88.2` 与 `18081`，不填代理账号密码。设置入口可能随系统版本变化。若当前系统未提供此选项，不要为此改全家的默认网关。

先确认掌机能联网，再启动实际远程游玩，检查是否进入 PS5 画面。PS5 仍需启用远程游玩、与掌机使用同一账号；有待机唤醒需求时，按 [PlayStation 官方说明](https://www.playstation.com/en-us/support/hardware/playstation-remote-play-on-psportal/) 配置。

## 5. 配置变更与回退

先备份旧配置，以候选文件做验证，通过后才替换配置并重启本服务。重启会断开本服务的现有连接。

```sh
/usr/libexec/portal-local-proxy/portal-local-proxy -config /tmp/portal-config.json -check
```

撤销时，先把掌机的代理恢复到原设置，再停止并取消本服务的自启动：

```sh
/etc/init.d/portal-local-proxy stop
/etc/init.d/portal-local-proxy disable
```

这些命令不删除配置，不操作上游代理或路由器其他服务。
