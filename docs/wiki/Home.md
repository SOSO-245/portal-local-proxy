# PS Portal 旁路由联网助手

Portal Local Proxy 是运行在 OpenWrt 旁路由上的独立 HTTP / HTTPS CONNECT 代理。它给自己的 PS Portal 提供一个可单独配置的联网入口，不要求更改全家默认网关。

| 做什么 | 不做什么 |
| --- | --- |
| 转发掌机明确发给它的 HTTP 请求，为 HTTPS 建立 TCP 隧道 | 不解密 TLS，不接管 UDP，不改 NAT、无线信道或防火墙 |
| 限制可接入的客户端和可访问的域名 | 不是公网代理，不提供账号登录、区服解锁或串流画质保证 |
| 选择由本机发起出站连接，或交给已有的可信 HTTP 代理 | 不创建加速节点，不保证降低游戏延迟，不替代任何加速器 |

典型路径：`PS Portal → OpenWrt 上的本服务 → 直接出站或已有 HTTP 上游 → 目标服务`。这条路径只说明代理请求如何出站，不代表 PS5 与掌机之间的音视频也经过本服务。

## 从哪里开始

- [安装与配置](Install.md)：本地试运行、OpenWrt 首装、掌机设置和回退。
- [故障排查](Troubleshooting.md)：区分代理拒绝、上游故障、PSN 登录和远程游玩。
- [安全与兼容范围](Security.md)：局域网限制、诊断边界与问题反馈。

这是非官方工具，不由 Sony、PlayStation 或 SonyAgent 提供。已在一台 ARM64 OpenWrt 系设备验证核心服务运行，并有一次通过该配置进入 PS5 画面的实机反馈；不代表其他固件、冷启动、休眠唤醒、异地串流或长期稳定性已验证。优先阅读 [PlayStation 官方远程游玩说明](https://www.playstation.com/en-us/support/hardware/playstation-remote-play-on-psportal/)。

维护者在实际串流中观察到 1–5 ms 的延迟读数。没有同条件对照与完整测量记录，不能据此认定延迟降低由本代理造成，也不保证其他环境可以复现。代理 HTTP 探测耗时和游戏串流延迟是不同指标。
