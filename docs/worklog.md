# CLink 开发工作日志

## 2026-09-06 · Step 2-5 全量交付（"全部搞"指令）

### 环境

- Go 1.27.1 安装于 ~/sdk/go（用户态，无 sudo）
- 依赖：pion/webrtc/v3 + pion/stun + wails v2.15.0 + x/crypto(hkdf)
- wails CLI 交叉编译 Windows：**CLink.exe 17.9MB 单文件构建成功**

### 实现清单

| 模块 | 文件 | 要点 |
|------|------|------|
| 信箱客户端 | internal/mailbox/ | HKDF(roomCode)→AES-GCM 信封；store/get/shorten/redirect-info 带重试退避 |
| IPv6 探测 | internal/netprobe/ipv6.go | 全局单播过滤（排除 fe80/ULA）+ UDP 路由探测（2400:3200::53 等双目标） |
| NAT 体检 | internal/netprobe/nat.go | 双 STUN 服务器映射端口对比判对称 NAT |
| Tier1 隧道 | internal/tunnel/v6direct/ | [::]随机端口↔127.0.0.1:mcPort 双向桥接，TCP KeepAlive 20s |
| Tier2 隧道 | internal/tunnel/p2p/ | 协商式 DC（ctrl ID=1 + data ID≥10）；open/ack 握手；20s 心跳；16KB 分片 |
| 会话状态机 | internal/session/ | 房主 epoch 轮转挂新 Offer；客机并发扫槽/退避排队/自动重连 |
| MC 日志 | internal/mclog/ | latest.log 增量轮询；两个正则（LAN 开放 + 服端启动） |
| 防火墙 | internal/sysguard/ | UAC runas 一次性提权；幂等 netsh（程序规则 + 25565） |
| Wails 应用 | app.go / main.go / frontend/ | 保姆级 UI；事件推送；剪贴板自动复制；高级设置（TURN/MC目录/信箱） |
| 开源物料 | README.md / docs-site/ | 保姆级 README；GitHub Pages 官网 + /r/ 中转页 |

### 关键 bug 修复（实测发现）

1. **协商式 DC 不走 OnDataChannel**：客机必须主动 CreateDataChannel(同 ID)。
   症状：回环测试 ctrl 通道 20s 超时。
2. **DCEP 误解析竞态**：客机先发数据、房主未建同 ID 通道 → SCTP 流被误判重置。
   症状：pion "Payload Protocol Identifier" 错误 + echo 超时。
   修复：open → host 创建并回 ack → guest 收 ack 才建通道放行数据。
3. pion v3 API 适配：SDPType 是整型枚举（需 String()/解析函数）；stun.Do 回调签名是 stun.Event。

### 测试结果（全绿）

- go vet：干净
- 单元+真网：mailbox（含真实 Clipzy 端到端、槽位占用语义）✅
- p2p 回环：echo/96KB/多连接 ✅；v6direct 回环 ✅
- **session 真网集成：房主+客机 3.8s 完成全流程（真实信箱+WebRTC）** ✅
- Windows 交叉编译 + wails 正式打包 ✅

### 遗留事项

- [ ] GitHub 仓库创建 + Pages 发布（用户操作：docs-site → ccdsyy.github.io 仓库）
- [ ] 真机双端联调（本环境只能回环/真网信令验证，无法模拟两台不同 NAT 的 Windows）
- [ ] Clipzy 限频策略未知：429/5xx 已有退避，长期观察
- [ ] 房主 UI 侧"延长房间"功能（当前 6h TTL 足够，暂无需求）
- [ ] 潜在优化：SDP 用 zlib 压缩（当前 ~8KB 已可接受）

---

## 2026-09-04 · Step 1：项目骨架与信箱设计

### 实测侦察结论（信箱选型）

- UAPI (`uapis.cn`) 文档描述的 Clipzy 临时文本存储，**网关路径 `uapis.cn/api/v1/api/*` 实测 404**，
  真实后端在 `paste.sdjz.wiki`（其官方 curl 示例同源）
- 已验证端点：`POST /api/store`（ttl 生效、可重复读）、`GET /api/get?id=`、
  `POST /api/shorten`（**支持自定义 customCode 3-8 位**，禁字符 0/1/i/l/o）、
  `GET /api/redirect-info/{code}`（寻址查询，404=不存在）
- 加密格式：AES-256-GCM，信封 `b64(iv‖ct‖tag)` 与 WebCrypto 兼容；
  Node 端到端实测（加密→store→get→解密）逐字节一致 ✅
- 短链 url 字段必须合法 http(s)（clink:// 被拒）→ 协议采用
  `https://ccdsyy.github.io/r/?id=...`（顺带引流官网）

### 架构决策

- **两级信箱**：Clipzy 粉贴做内容信箱（存 SDP 密文），Clipzy 短链做寻址层（房间码→信箱 id）
- **槽位规则**：`offer=R+ALPHA[epoch]+"h"` / `answer=R+ALPHA[epoch]+"g"`（8 位）
- **房间码**：6 位防混淆字母表 `abcdefghjkmnpqrstuvwxyz23456789`（31^6≈8.9亿），
  与 Clipzy customCode 字符规则严格对齐
- **密钥**：HKDF(roomCode) 派生 AES-256；知房间码即房间成员
- 短链永久占用 → 房间码一次性使用，应用层 ts（24 小时）判房间过期
- 信封不做 LZString 压缩（服务端不校验、SDP<8KB、省 Go 移植）

### 交付物

- `docs/PROTOCOL.md` —— 信令协议规范（含完整伪代码与实测附录）
- `docs/ARCHITECTURE.md` —— Wails 目录结构与 Step 映射
- `tests/mailbox-test.js` —— 信箱协议端到端实测脚本（Node，已跑通）

### 引流信息（已硬编码 UI 与 README）

ccdsyy.github.io / 抖音 ccdsyyznb / QQ 3310339901 / 群 1108157805


---

## 2026-11-21 · v1.3：信令服务器选项 + STUN/TURN 多节点 + bug 修复 + P2P 体验对齐

### 用户指令

删掉意义不明的功能；加入信令服务器选项（剪贴板服务1 / UAPI剪贴板服务 / 公共 STUN 和 TURN 多节点）；修 bug；模仿其他成功的 P2P 应用；功能越多越好。

### 实测侦察（本机 = 中国电信 CGNAT 网络）

- paste.sdjz.wiki（剪贴板服务1）：store/shorten/redirect-info 全部可用 ✅
- uapis.cn/api/v1（UAPI剪贴板服务）：OpenAPI 声明了 Clipzy 端点但网关未路由
  （404 `API endpoint not found`，与 2026-09 结论一致）→ 保留为容灾备节点
- STUN 探测 RTT：小米 83ms < Cloudflare 187ms < Google ~200ms < Nextcloud 206ms
- TURN：OpenRelay 全端口超时（已死）；ExpressTurn 对 Binding 有响应但 Allocate 无回包
- **关键对照实验**：ICE 多 STUN 节点并发候选在 CGNAT 下失灵
  （5 节点 3/3 失败；Google 单节点 4/4 稳过）→ 采纳"多节点探测选优 + ICE 单节点"

### 功能新增

| 功能 | 实现 |
|------|------|
| 信令服务器选项 | 设置页下拉：自动容灾 / 剪贴板服务1 / UAPI剪贴板 / 自定义；`mailbox.Client` 多节点轮换 + 网关 404 与业务 404 区分 |
| STUN 多节点 | `netprobe.PickSTUN()` Google 首选探测选优（缓存 10 分钟）；设置页可指定具名节点或自定义列表 |
| TURN 多节点 | 预设节点复选框 + 自定义列表叠加；可靠性如实标注 |
| 连接质量徽标 | 客机心跳 pong 回显时间戳测 RTT，连接成功显示"方式 · 延迟" |
| 最近房间 | 最近 8 个房间码（房主+客机都记），一键回填重连 |
| 房主掉线感知 | WebRTC 成员断线 → 移出成员表 + `guest-left` 事件 + 系统通知 |
| 自定义本地端口 | 客机 25565 被占用/自开服时可改（设置页） |
| 一键复制邀请文案 | playit.gg 式：房间码 + 下载链接一条消息直接发朋友 |
| 网络体检增强 | 显示命中的 STUN 节点与 RTT；修复 IPv6 判断字段错位（读 `hasV6` 而非不存在的 `ipv6`） |

### Bug 修复

1. **Offer 槽位毒化死循环**：房间码注册撞"已被使用"后原地重试永远失败 → 改为轮转下一 epoch
2. **打洞超时僵尸端点**：45s 超时未回收 pc、成员表残留 → 回收 + 清理
3. **加入按钮卡死**：异步错误（房间不存在等）后按钮永远禁用 → `error` 事件恢复按钮
4. **关窗误清剪贴板**：OnBeforeClose 进入悬浮球时把用户剪贴板清空 → 移除
5. **NeoForge 永远搜不到**：加载器 value 误为 `neo forged`（Modrinth slug 是 `neoforge`）
6. **网络体检 IPv6 永远显示不支持**：前端读 `r.ipv6` 但 JSON 字段是 `hasV6`
7. **Google STUN 唯一默认无容灾**：国内部分 ISP UDP 不通即完全无 srflx → 多节点选优兜底
8. **Kick 不减人数 / 人数漂移**：Guests() 改为以成员表为准；同名重连旧监控误报掉线 → 端点身份校验
9. **版本号各处不一致**：v1.1 硬编码（标题栏/关于/wails.json）→ 全部对齐 v1.3.0，标题栏运行时取值
10. **心跳 RTT**：顺带支持 pong 回显测延迟（见新功能）

### 删减

- 移除「必应每日壁纸」工具（与联机工具定位无关，意义不明项）

### 其他

- 全仓 gofmt 规范化（原上传代码未格式化）
- 手工同步 wailsjs 绑定（App.d.ts / App.js / models.ts）：GetNet/SetNet/PresetNodes/RecentRooms 替代 GetAdvanced/SetAdvanced/TbBing
- 版本 v1.3.0（AppBuild=4）；website/update.json 未动（发布时再 bump，避免指向不存在的 exe）

### 验证

- go build / go vet 全绿
- 单测：blacklist / mailbox（含真网端到端）/ modhub / smsbox / updater / v6direct 全过
- session 真网全流程：信令注册 + 扫槽 + Answer + ctrl 打通 ✅（本沙箱 UDP 数据通道随会话时长退化，
  同配置原代码同样复现，判为环境噪声；真机需复测——见遗留事项）

### 遗留事项

- [ ] 真机双 NAT 联调 v1.3（重点复测：多节点选优 / TURN 勾选 / RTT 展示）
- [ ] UAPI 网关恢复 Clipzy 路由后复测容灾切换
- [ ] ExpressTurn 正确凭据调研（当前 free:free 不被 Allocate 接受）
