# PS Portal 旁路由联网助手

Portal Local Proxy 是运行在 OpenWrt 旁路由上的独立 HTTP / HTTPS CONNECT 代理。它为 PS Portal 提供单独的联网入口，不要求更改全家默认网关。

- 仅转发客户端明确发给代理的 HTTP 请求和 HTTPS CONNECT，支持直出或使用已有的可信 HTTP 上游。
- 默认仅监听本机；局域网部署时按客户端地址和目标域名限制访问。
- 不解密 TLS，不转发 UDP，不修改 NAT、无线或防火墙。它不是串流媒体中继，也不是 Sony 或 PlayStation 官方工具。

维护者在实际串流中观察到 1–5 ms 的延迟读数。此记录没有同条件对照，不能据此归因于本代理，也不代表其他网络的延迟或画质。

## 快速检查

使用 Go 1.24 或更新版本。核心程序没有第三方 Go 依赖。

```sh
go test ./...
go run . -config config.example.json -check
```

安装、回退与排障见 [GitHub Wiki](https://github.com/SOSO-245/portal-local-proxy/wiki)，也可直接阅读仓库内的 [文档](docs/wiki/Home.md)。仅在可信局域网使用，不要将监听端口暴露到公网。

本仓库首发仅包含核心代理与 ARM64 OpenWrt 首次安装脚本。没有 LuCI 界面、抓包工具或历史安装包。

## 许可证

[MIT](LICENSE)
