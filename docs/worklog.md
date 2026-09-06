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

