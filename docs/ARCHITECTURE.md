# CLink 项目架构与目录结构（Step 1 设计）

> 技术栈：Go 1.21+ / Wails v2（前端无框架纯 HTML/JS/CSS）/ pion/webrtc/v3 / 原生 net
> 产物：单一可执行文件（CLink.exe / CLink）

## 1. 目录总览

```
clink/
├── main.go                      # Wails 应用入口：装配窗口 + 后端服务
├── app.go                       # ★ 前后端桥接层：所有导出给 JS 的方法（Wails bindings）
│                                #   CreateRoom / JoinRoom / GetStatus / CopyClipboard...
│                                #   只做参数校验与状态转发，业务逻辑全部下沉 internal/
├── wails.json                   # Wails 构建配置（窗口尺寸、前端目录、打包参数）
├── go.mod / go.sum
│
├── build/                       # Wails 打包资源（跨平台）
│   ├── appicon.png
│   ├── windows/
│   │   ├── icon.ico
│   │   ├── info.json
│   │   └── wails.exe.manifest   # ← 声明 requireAdministrator（首次启动请求管理员权限，
│   │                                防火墙放行需要；UI 中向用户解释原因）
│   ├── darwin/                  #    （macOS Info.plist：本地网络权限描述）
│   └── linux/
│
├── internal/                    # ══ 核心实现（不对外暴露，单元测试主战场）══
│   │
│   ├── mailbox/                 # ★ Step 1 交付：UAPI/Clipzy 公共信箱客户端
│   │   ├── client.go            #   HTTP 封装：store / get / shorten / redirectInfo
│   │   │                        #   统一 8s 超时 + 3 次指数退避 + 双端点容灾
│   │   ├── envelope.go          #   信封加解密：AES-256-GCM + HKDF(roomCode)（见 PROTOCOL.md §4）
│   │   ├── signal.go            #   Signal 数据结构定义与校验（§5）
│   │   ├── slot.go              #   槽位命名：R+ALPHA[epoch]+role，房间码生成/校验
│   │   └── mailbox_test.go      #   协议回归测试（对真实信箱的集成测试，打 short-live tag）
│   │
│   ├── netprobe/                # 网络环境探测（三层降级决策入口）
│   │   ├── probe.go             #   Probe() → Report{HasPublicV6, NATType, StunReachable}
│   │   ├── ipv6.go              #   网卡 IPv6 枚举（过滤 fe80 链路本地/ULA）Step 2
│   │   └── nat.go               #   STUN 探测 NAT 类型（full-cone / restricted / symmetric）
│   │
│   ├── tunnel/                  # 隧道统一抽象（UI/信令层不感知 Tier 细节）
│   │   ├── tunnel.go            #   interface Tunnel { Dial/Listen/Events() }
│   │   ├── v6direct/            #   Tier 1：原生 net 包 TCP 直连（Step 2）
│   │   │   └── v6direct.go      #     房主: [::]:randPort 监听桥接；客机: 拨号
│   │   └── p2p/                 #   Tier 2：pion/webrtc DataChannel（Step 3）
│   │       ├── peer.go          #     PeerConnection 生命周期、ICE Restart
│   │       ├── bridge.go        #     DataChannel ⇄ net.Conn 双向桥（io.Copy 双泵）
│   │       └── ctrl.go          #     控制帧信道（重连通知/心跳，复用 DC）
│   │
│   ├── session/                 # 会话状态机（Step 4 联动 UI）
│   │   ├── host.go              #   房主流程编排：探测→信箱→隧道→心跳（PROTOCOL §6.1）
│   │   ├── guest.go             #   客机流程编排（PROTOCOL §6.2）
│   │   ├── heartbeat.go         #   20s 应用层心跳 + 断线判定 + epoch 重连（§6 重连）
│   │   └── events.go            #   事件总线：状态变更推送到前端（Wails EventsEmit）
│   │
│   ├── mclog/                   # Minecraft 联机端口自动嗅探（Step 4）
│   │   ├── finder.go            #   定位 .minecraft/logs/latest.log（含多启动器路径探测）
│   │   └── watcher.go           #   增量 tail 监听 "Local game hosted on port XXXXX"
│   │
│   └── sysguard/                # 系统权限与防火墙（Step 4，构建标签按平台编译）
│       ├── firewall_windows.go  #   netsh advfirewall 放行 25565 + 随机隧道端口
│       ├── firewall_other.go    #   非 Windows 平台 no-op（Linux 无需/发教程提示）
│       └── admin_windows.go     #   UAC 自检（manifest 声明 requireAdministrator）
│
├── frontend/                    # ══ Wails 前端（无框架，零依赖，秒开）══
│   ├── index.html               #   主界面：模块A(房主)/模块B(客机) + 状态条 + 关于页
│   ├── src/
│   │   ├── main.js              #   Wails runtime 初始化、后端方法调用封装
│   │   ├── ui.js                #   界面状态机：空闲→创建中→等待客机→已连接→重连中
│   │   ├── clipboard.js         #   房间码/127.0.0.1:25565 自动复制 + 降级方案
│   │   └── styles.css           #   保姆级视觉：大按钮、大字号、进度态、去术语化文案
│   ├── wailsjs/                 #   Wails 自动生成的 Go 绑定（go/main/App.js）
│   └── assets/                  #   图标等静态资源
│
├── tests/                       # 独立验证脚本（已含 mailbox-test.js 实测脚本）
│   └── mailbox-test.js          #   ★ Step 1 信箱协议端到端实测（Node，已跑通）
│
├── docs/
│   ├── PROTOCOL.md              # ★ Step 1 交付：信令协议规范（全部实测背书）
│   ├── ARCHITECTURE.md          #   本文档
│   └── worklog.md               #   各 Step 决策与验证记录（持续追加）
│
├── scripts/                     # 开发/发布辅助
│   ├── build-all.sh             #   跨平台交叉编译（win/mac/linux，amd64+arm64）
│   └── release.sh               #   打 tag → GitHub Release 附件
│
└── README.md                    # Step 5：保姆级使用文档 + 引流矩阵
```

## 2. 分层与依赖方向

```
app.go (桥接)
   │
   ▼
session/ (流程编排·状态机) ──► frontend (EventsEmit 状态推送)
   │            │
   ▼            ▼
mailbox/     tunnel/ ─► v6direct | p2p
(信令)          │
   ▲            ▼
   │         netprobe/ (Tier 决策)
   │
mclog/ (端口嗅探，供 session 房主侧自动填端口)
sysguard/ (防火墙/权限，main 启动时调用)
```

- 依赖只允许自上而下；`mailbox`/`netprobe`/`tunnel` 互相不感知
- 前端**只**通过 `app.go` 暴露的方法 + 事件总线交互，永不直接碰 internal 包

## 3. 各 Step 与目录的映射（协作计划）

| Step | 新增/实现 | 交付验收 |
|------|-----------|----------|
| **1（本次）** | `docs/PROTOCOL.md`、`tests/mailbox-test.js`（信箱链路已实测）、目录蓝图 | 协议评审确认 |
| 2 | `netprobe/ipv6.go`、`tunnel/v6direct/`、`mailbox/`（真实 Go 实现） | 两台 v6 主机互通 MC |
| 3 | `netprobe/nat.go`、`tunnel/p2p/`（pion 集成、DC 桥） | 双内网 UDP 打洞互通 |
| 4 | `frontend/`、`session/`、`mclog/`、`sysguard/`、`main.go/app.go` | 完整 GUI 走通三层降级 |
| 5 | `README.md`、GitHub Pages 官网、`scripts/` | 开源发布 |

## 4. 关键技术决策备忘

1. **信箱用 paste.sdjz.wiki 直连而非 uapis.cn 网关**：实测网关 404，直连后端可用；
   client.go 保留双端点列表做容灾（详见 PROTOCOL.md §2 注）
2. **信封不做 LZString 压缩**：服务端不校验格式；SDP 加密后 < 8KB，
   省去 Go 移植 LZString 的维护成本（决策记录于 PROTOCOL.md §4）
3. **房间码字符集去掉 0/1/i/l/o**：与 Clipzy customCode 规则硬绑定（实测），顺带防呆
4. **短链永久占用 → 房间码一次性**：每次开房新码（10.7 亿空间），应用层 ts 过期兜底
5. **Wails 而非 fyne/lorca**：前后端分离，纯 HTML 前端零构建依赖，符合"单一可执行文件+秒开"
6. **管理员权限**：manifest 声明（防火墙规则需要），UI 首次启动弹窗解释"仅需一次"
7. **心跳走隧道内部控制信道**：不占公共信箱 QPS，20s 间隔防 NAT 老化
