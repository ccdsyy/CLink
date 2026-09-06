# CLink 信令协议规范（v1）

> Step 1 交付物 · 本文档中全部 API 行为均已于 2026-09-04 实测验证通过（见附录 A 验证记录）

## 1. 设计目标

在不要求用户填写任何 Token、不部署自有服务器的前提下，利用国内免鉴权公共 API 完成
WebRTC SDP / IPv6 地址的交换，实现"6 位房间码"即可建连的极简体验。

**核心思路：两级信箱接力**

```
┌────────┐   ①写入Offer信箱    ┌──────────────────┐
│  房主   │ ─────────────────> │ paste.sdjz.wiki   │
│        │ <───────────────── │  (UAPI Clipzy)    │
│        │   ③轮询Answer信箱   │                  │
└───┬────┘                    └─────────▲────────┘
    │ ②注册短链 customCode=R           │ ④短链寻址查询
    │   （房间码=短码）                 │
    ▼                                   │
┌────────┐  输入6位房间码R ─────────────┘
│  客机   │  GET /api/redirect-info/{槽位code}
└────────┘
```

- **内容信箱**（Clipzy 粉贴）：加密存储 SDP，服务器只见密文，`id` 由服务器随机签发
- **寻址层**（Clipzy 短链）：把 6 位房间码映射到信箱 `id`，客机凭房间码查询

## 2. 底层 API 契约（已实测）

| # | 操作 | 请求 | 响应 | 已验证行为 |
|---|------|------|------|-----------|
| 1 | 存密文 | `POST https://paste.sdjz.wiki/api/store`<br>body `{"compressedData":"<b64>","ttl":21600}` | `200 {"id":"SQ7ui3SHgS"}` | ttl 精确到期后 `404`；不传 ttl 默认 3600s；可重复 GET；`compressedData` 为任意字符串，服务端不校验格式 |
| 2 | 取密文 | `GET /api/get?id={id}` | `200 {"compressedData":"<b64>"}` | 多次读返回一致内容（非读后即焚），支持房主轮询 |
| 3 | 注册短码 | `POST /api/shorten`<br>body `{"url":"https://...","customCode":"x9y8z7"}` | `200 {"code":"x9y8z7","shortUrl":"/s/x9y8z7"}` | code 仅允许**小写字母+数字、3~8 位、禁用 `0 1 i l o`**（防混淆）；url 必须为合法 http(s) URL；code 冲突返回 `400 "该自定义代码已被使用"` |
| 4 | 查询短码 | `GET /api/redirect-info/{code}` | `200 {"url":"...","createdAt":...,"expiresAt":null}` | 不存在/已过期返回 `404`；短链无客户端过期字段（视为永久），生命周期由应用层管理 |

> 注：UAPI 文档页 (`uapis.cn/docs/api-reference/clipzy-`) 同样描述了这三个能力，
> 网关地址 `https://uapis.cn/api/v1/api/*`，当前直连网关会 404，**实际后端为 paste.sdjz.wiki**。
> 客户端内置双端点容灾：主用 `paste.sdjz.wiki`，备用 `uapis.cn` 网关（二者互通性在 Step 4 联调时复测）。

## 3. 房间码与槽位命名

### 3.1 防混淆字母表（31 字符）

```
ALPHA = "abcdefghjkmnpqrstuvwxyz23456789"
```

- 与 Clipzy customCode 规则**严格兼容**（已实测禁用 `0 1 i l o`）
- 纯小写输入，UI 层自动把用户输入的大写转小写（防呆）
- 空间：31^6 ≈ 8.9 亿房间，碰撞概率可忽略

### 3.2 房间码

```
roomCode R = 6 个 ALPHA 字符（房主本地 crypto/rand 生成）
```

### 3.3 信箱槽位（Slot）

房间生命周期内用 **epoch** 区分轮次：每完成一位玩家加入 epoch+1（挂新 Offer 槽，
支持后续玩家）；客机断线重连也通过重扫最新 epoch 实现：

```
槽位 = R + ALPHA[epoch] + 角色符
     角色符：'h' = 房主投递(offer)   'g' = 客机投递(answer)

offer 槽(epoch e)  = R + ALPHA[e] + "h"    // 8 位，如 "abc234ah"
answer 槽(epoch e) = R + ALPHA[e] + "g"    // 8 位，如 "abc234ag"

epoch ∈ [0, 30]（ALPHA 共 31 字符 → 单房间最多 31 位玩家依次加入）
```

**客机扫槽**：加入时并发查询全部 epoch 的 offer 槽，取最高且可解密校验通过者。

**槽位冲突处理**：两位玩家同时抢注同一 answer 槽（Clipzy shorten 一经注册永久占用，
后到者返回"已被使用"）→ 后到客机随机退避 2–8 秒后重扫槽位（房主已轮转新 epoch），
自动排队加入，UI 提示"加入排队中"。

## 4. 加密信封格式

```
信封 envelope(plaintext, key) =
    base64( iv[12] ‖ AES-256-GCM(key, iv, plaintext) )
    // GCM 输出尾部自带 16 字节 authTag —— 与 WebCrypto/browsers 及
    // Go crypto/cipher 原生格式完全一致，跨端互操作零转换
```

```
信令密钥 K = HKDF-SHA256(
    ikm  = roomCode(6字节ASCII),
    salt = "CLink-Signal-v1",     // 协议常量
    info = "mailbox",
    L    = 32                     // AES-256
)
```

- 知道房间码 = 房间成员（可解密自己的信令），模型简单且贴合使用直觉
- SDP 中含有内网候选等轻敏信息，房间码即门禁已足够；服务器侧始终只见密文
- 兼容官方 Clipzy 网页（可选实现 `LZString.compressToUTF16` 前置于加密，
  非必需——服务端不校验格式，SDP 经 GCM+Base64 后 < 8KB，压缩收益小，
  Go 端直接省去 LZString 移植，**协议 v1 决定：不压缩**）

## 5. 信令数据结构（payload）

```go
// 信封内的 JSON（Go 结构体，前后两端共用）

type Signal struct {
    Ver    int    `json:"v"`      // 协议版本，当前 1
    Type   string `json:"type"`   // "offer" | "answer"
    Epoch  int    `json:"epoch"`  // 重连轮次，从 0 起
    Room   string `json:"room"`   // 房间码（冗余校验，防投错槽）
    Ts     int64  `json:"ts"`     // Unix 毫秒，接收方校验新鲜度
    Nonce  string `json:"nonce"`  // 随机 8 字符，防重放（同槽重复投递时取新 ts）

    Tier   string `json:"tier"`   // "v6" | "webrtc" —— 本次连接选用的层级

    // Tier 1（IPv6 直连）时填充：
    V6     *V6Info `json:"v6,omitempty"`

    // Tier 2（WebRTC）时填充：
    SDP    string  `json:"sdp,omitempty"`  // pion 序列化后的 SDP
    Ice    []string `json:"ice,omitempty"` // 房主侧额外 iceServers（含用户自填 TURN）

    // 房主描述信息（客机 UI 显示用）
    HostPort int   `json:"mcPort,omitempty"` // 房主 MC 局域网端口（展示"房主端口"）
}

type V6Info struct {
    Addr string `json:"addr"` // 房主公网 IPv6（如 2408:xxxx::1234）
    Port int    `json:"port"` // 房主本地监听的隧道端口（随机高位端口）
}
```

**接收校验规则**：
1. `v == 1`，`room == 房间码`，`type` 与槽位角色一致
2. `|now - ts| ≤ 10 分钟`（超出 → "房间已过期/已关闭"）
3. 同一 nonce 已见过 → 丢弃（防重放）

## 6. 完整流程伪代码

### 6.1 房主：CreateRoom(mcPort)

```
func CreateRoom(mcPort int) -> roomCode:

    R = random_chars(ALPHA, 6)

    # ---- 网络探测（三层降级决策，Step 2/3 实现）----
    tier, offerPayload = NetProbe.decide()      # 返回 tier 及待交换的载荷
    #   tier == "v6":     监听 [::]:随机端口, offerPayload.V6 = {公网v6, 端口}
    #   tier == "webrtc": pion 生成 Offer SDP, offerPayload.SDP = ...

    epoch = 0
    offerPayload 填充 {v:1, type:"offer", epoch:0, room:R, ts:now, tier, mcPort}

    # ---- ① 投递 Offer 信箱 ----
    K   = HKDF(R)
    env = Envelope(json(offerPayload), K)
    id  = POST /api/store { compressedData: env, ttl: 21600 }  # 房间存活 6 小时

    # ---- ② 注册房间码短链（寻址层）----
    retry ≤ 3:
        resp = POST /api/shorten {
            url: "https://ccdsyy.github.io/r/?id=<id>&code=<R>&epoch=0",
            #     ↑ 人类点开短链会到达官网（引流+教程）；客户端仅解析参数
            customCode: R + ALPHA[0] + "h"
        }
        冲突 → R = random_chars(ALPHA,6); continue
    注册成功 → 剪贴板 = R, UI 显示房间码

    # ---- ③ 轮询 Answer 信箱 ----
    loop 每 2s:
        resp = GET /api/redirect-info/{R + ALPHA[0] + "g"}
        404 → continue（客机未上线）
        200 → id2 = 解析 resp.url 的 id 参数
              env2 = GET /api/get?id=id2
              answer = Decrypt(env2, K)     # GCM 校验失败 → continue
              校验 §5 规则 (v/room/type/ts/nonce)
        成功 → 按 tier 建立隧道（v6: 直连 / webrtc: SetRemoteDescription）
              break

    # ---- ④ 断线重连（epoch 递增）----
    onDisconnect:
        epoch += 1
        重新 NetProbe.decide() → 新 offer（新 SDP / 新 v6 地址）
        新信箱 + 新槽位 R+ALPHA[epoch]+"h"，通过【已建立的隧道内控制信道】
        通知客机（控制帧 type:"restart"），仅当隧道已死才回退到信箱轮询模式
        （客机同样轮询 R+ALPHA[epoch]+"g" 侧的 offer 槽，双向兜底）
```

### 6.2 客机：JoinRoom(inputCode)

```
func JoinRoom(input string):

    R = normalize(input)        # 转小写 + 校验 6 位且全部 ∈ ALPHA
    不合法 → UI 提示 "房间码为 6 位字母数字（不含 0/1/i/l/o）"

    epoch = 0
    deadline = now + 30s

    # ---- ④ 查询房间码 → 拿到 Offer 信箱 ----
    loop 每 2s (超 30s → "房间不存在或已关闭"):
        resp = GET /api/redirect-info/{R + ALPHA[0] + "h"}
        404 → continue
        200 → id = 解析 resp.url 的 id 参数
              env = GET /api/get?id=id
              offer = Decrypt(env, HKDF(R))
              校验 §5 → ts 超 10 分钟 → "房间已过期"

    # ---- 生成 Answer 并投递 ----
    按 offer.tier 建立对应端:
        webrtc: pion SetRemoteDescription(offer.SDP) → 生成 Answer SDP
        v6:     无需应答载荷（直连型 tier），answer 仅回执 {type:"answer", tier:"v6"}
    answer 填充 {v:1, type:"answer", epoch:0, room:R, ts:now, tier, sdp?...}

    id2 = POST /api/store { envelope(answer), ttl: 600 }
    POST /api/shorten {
        url: "https://ccdsyy.github.io/r/?id=<id2>&code=<R>&epoch=0",
        customCode: R + ALPHA[0] + "g"
    }

    # ---- 本地起代理，等待隧道就绪 ----
    listen 127.0.0.1:25565
    隧道就绪 → 剪贴板 = "127.0.0.1:25565"
    UI: "连接成功！请在游戏内直接连接粘贴"

    # ---- 重连跟随 ----
    onDisconnect: epoch += 1
        webrtc: 监听隧道控制帧 → ICE Restart（新 SDP 走新 epoch 槽位）
        v6:     重新解析 offer 槽 R+ALPHA[epoch]+"h"（房主重拨后更新地址）
```

### 6.3 时间线（正常路径）

```
房主                          信箱/短链服务                     客机
 │ ①store(offer)               │                              │
 │ ②shorten(R+"ah")            │                              │
 │─────────────────────────────│◄──── ④redirect-info(R+"ah")──│ 输入房间码
 │                             │──────► url→id→get→解密 ─────►│ 拿到 offer
 │                             │◄─ ⑤store(answer) ────────────│ 生成 answer
 │ ③轮询 redirect-info(R+"ag") │◄─ ⑥shorten(R+"ag") ──────────│
 │◄──── id→get→解密 ───────────│                              │
 │ 建立隧道 ◄═════════════════ P2P 数据通道 ═════════════════►│ 127.0.0.1:25565
 │ ⑦20s 应用层心跳（走隧道内部，不占信箱）                    │
```

## 7. 关键工程参数

| 参数 | 值 | 说明 |
|------|-----|------|
| 信箱 TTL | 21600s（6h，覆盖一整局） | 房间信 Offer；Answer 信箱 600s；房间码 24h 时间窗过期 |
| 信箱轮询间隔 | 2s | 双方各自轮询对向槽位，QPS ≈ 0.5/端 |
| 房间新鲜度 | ts ≤ 24h | 应用层过期（信封携带 ts，超窗拒收），补偿短链永久占用 |
| 加入轮次上限 | 31（epoch 0..30） | 槽位 8 字符的硬上限；重连不占 epoch（重扫最新槽位） |
| 房码冲突重试 | 自动退避 2–8s 重扫 | 并发加入同一 answer 槽的排队机制 |
| 加密 | AES-256-GCM + HKDF | 信封 = b64(iv‖ct‖tag)，WebCrypto 兼容 |
| HTTP 超时 | 8s / 3 次退避重试 | 所有信箱请求统一封装 |

## 8. 安全模型

1. **端到端**：服务器（Clipzy/UAPI）只见密文；密钥由房间码派生，不离开两端
2. **门禁 = 房间码**：6 位防混淆码 ≈ 31 bit 熵，10 分钟窗口内暴力枚举不可行
   （服务端限频 + 32^6 空间 + 600s TTL）
3. **防投毒**：GCM 认证标签保证伪造/篡改信箱内容直接解密失败被丢弃
4. **防重放**：nonce + ts 双重校验
5. **隐私**：SDP 中的内网候选仅房间码持有者可见；STUN 采用 Google 公共服务
6. **免责**：信箱内容由客户端加密，公共信箱运营方无法审查（在 README 中如实声明）

## 附录 A：实测验证记录（2026-09-04）

| 验证项 | 结果 |
|--------|------|
| AES-256-GCM 信封 Node 端到端（store→get→解密） | ✅ 内容逐字节一致 |
| store ttl=5s 到期后 get | ✅ 404 `{"error":"Data not found or expired"}` |
| get 重复读 | ✅ 返回一致内容 |
| shorten customCode="x9y8z7" | ✅ 200，跳转链 /s/→307→/redirect/（HTML 安全页）|
| shorten customCode 含 `1 0 i l o` / 9 位 | ✅ 正确拒绝（400） |
| shorten url 非法 scheme（clink://） | ✅ 拒绝"请提供有效的URL"（协议采用 github.io URL 规避） |
| redirect-info 查询 | ✅ `{"url":...}` / 404 |
| 房间码槽位方案推演 | ✅ 6 位房码 + 2 位后缀 = 8 位（长度上限内） |
| shorten 目标 URL 带查询参数 | ✅ 完整保留 `?id=`（2026-09-06 复测） |

## 附录 B：Go 实现要点（2026-09-06 实现并测试通过）

1. **协商式 DataChannel**：全部通道 `Negotiated: true + 固定 ID`。
   - ID 1 = ctrl 控制通道（JSON 消息：open/ack/close/ping/pong）
   - ID 10.. = 数据通道，每条客机本地 TCP 连接一条（兼容 MC 的 status ping + 正式连接两条连接）
2. **open/ack 两段握手**（重要实测教训）：客机收到房主 ack 后才创建同 ID 通道并放行
   TCP 数据。否则对端 SCTP 流未建通道时，数据会被误当 DCEP 解析
   （pion 报 "Payload Protocol Identifier is value we can't handle"）导致流被重置。
3. **心跳**：WebRTC 层走 ctrl 通道 ping/pong（20s 间隔，45s 无 pong 判死→自动重连）；
   IPv6 直连层用 TCP KeepAlive 20s（系统级等价实现，不污染 MC 字节流）。
4. **重连**：WebRTC 断线 → 客机重扫全部 offer 槽取最新 epoch → 重新走加入流程
   （等价于 ICE Restart 的全量重协商，但复用同一房间码，对用户无感）。
5. **多层并发桥接**：`io.Copy` 双向泵 + sync.Once 资源回收；DataChannel 单消息 16KB 分片。
6. **测试矩阵**（全部通过）：
   - mailbox 单元（信封往返/错误密钥/防篡改/槽位命名）
   - mailbox 真网端到端（加密→store→shorten→redirect-info→get→解密，槽位占用语义）
   - p2p 本机回环（完整 Offer/Answer/ack/echo/96KB 大包/多连接顺序复用）
   - v6direct 本机回环（[::1] 双向桥接）
   - session 真网集成（房主+客机全流程，含真实信箱与 WebRTC 打通，约 4s 完成）
