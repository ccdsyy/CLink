<div align="center">

# ⛏️ CLink

### Minecraft Java 版 · 一键 P2P 联机工具

**没有公网 IP？不会配置端口？看不懂 NAT？**

**CLink 把这些全部藏进底层——你只需要一个 6 位房间码。**

[官网](https://ccdsyy.github.io) · [视频教程（抖音号 ccdsyyznb）](https://www.douyin.com/user/MS4wLjABAAAASKARfxM-PjyGJvthbLpvjSfR9j129Rno8UWs3D-HPWk?from_tab_name=main) · QQ 交流群 `1108157805`

</div>

---

## 🎮 三步联机（保姆级）

### 房主

1. 打开 Minecraft，进入单人世界，按 `Esc` → **「对局域网开放」**
2. 打开 CLink，点 **「创建房间」**（端口会自动识别）
3. 把自动复制的 **6 位房间码** 发给你的小伙伴

### 客机（小伙伴）

1. 打开 CLink，输入房间码
2. 点 **「一键加入」**
3. 显示"连接成功"后，回到游戏：**多人游戏 → 直接连接 → 粘贴**（`127.0.0.1:25565` 已自动复制）

就这么简单。🎉

---

## 🧠 CLink 在底层做了什么

CLink 内置**三层智能网络**，自动选最快的方式把你们连起来：

| 层级 | 方式 | 什么时候用 |
|------|------|-----------|
| ⚡ Tier 1 | **IPv6 极速直连** | 双方网络都支持 IPv6（国内运营商大范围普及中） |
| 🛟 Tier 2 | **WebRTC P2P 打洞** | 普通宽带/校园网（UDP NAT 穿透） |
| 🆘 Tier 3 | **兜底指引 + 自定义中转** | 双方都是对称型 NAT 时，提示切换手机热点；技术党可在高级设置自填 TURN 服务器 |

**信令交换**不依赖任何 Token 和账号：借用国内公共"剪贴板"服务完成加密信件投递，
所有信件**端到端加密（AES-256-GCM）**，密钥由房间码派生——服务器只保管一个谁也打不开的保险箱。

> 打开 CLink 的"网络体检"可以随时查看自己家的网络属于哪一层。

---

## 📦 下载

- Windows：从 [Releases](https://github.com/ccdsyy/CLink/releases/tag/%E5%8F%AF%E6%89%A7%E8%A1%8C%E6%96%87%E4%BB%B6) 下载 `CLink.exe`，双击即用（绿色单文件，无需安装）
- 首次创建房间时 Windows 会弹一次 UAC 授权——这是为了自动添加防火墙放行规则，点"是"即可，仅此一次

<details>
<summary>自己编译（开发者）</summary>

```bash
# 需要 Go 1.21+ 
go install github.com/wailsapp/wails/v2/cmd/wails@latest
git clone https://github.com/ccdsyy/clink.git
cd clink
wails build -platform windows/amd64
# 产物：build/bin/CLink.exe
```

</details>

---

## ❓ 常见问题

<details>
<summary><b>小伙伴一直"正在连接"怎么办？</b></summary>

多半是双方都在对称型 NAT 后面（部分校园网/企业网）。让**一方**切换到**手机热点**再试，成功率立竿见影。进阶用户可在高级设置填写自建 TURN 中转。
</details>

<details>
<summary><b>防火墙弹窗要点允许吗？</b></summary>

要。CLink 需要接收来自小伙伴的入站连接，Windows 防火墙规则会由程序自动添加（需一次 UAC 授权）。
</details>

<details>
<summary><b>房间码安全吗？会被陌生人闯入吗？</b></summary>

房间码 6 位、约 8.9 亿种组合，且房间只保留 6 小时。信箱内容端到端加密，连信箱服务器自己都看不到你们的连接信息。知道房间码 = 是房间成员，这是设计使然（就像知道密码 = 能进门）。
</details>

<details>
<summary><b>支持多少人？</b></summary>

同一房间支持最多 31 位小伙伴**依次**加入（同时加入会自动排队重试）。房主退出程序 = 房间关闭。
</details>

<details>
<summary><b>会泄露我的 IP 吗？</b></summary>

P2P 直连的本质是双方交换网络地址后建立连接，对等端互相可见（这是所有联机工具的共性）；对第三方（信箱服务器）我们只投递加密信封。介意隐私的用户请自建 TURN。
</details>

---

## 🗺️ 原理一图流

```
房主                                    客机
 │ ①加密房间信(Offer) ──投入──▶ 公共信箱 ◀──扫槽位── ②输入房间码
 │                                        │
 │ ◀──轮询── 加密回信(Answer) ──投入────── ③
 │                                         
 └──────────── P2P 隧道建立 ────────────────┘
     Tier1: IPv6直连   Tier2: UDP打洞(DataChannel)
     
 游戏流量: 客机 127.0.0.1:25565 ◀──▶ 隧道 ◀──▶ 房主 MC 端口
```

完整协议规范（含实测记录）：[docs/PROTOCOL.md](docs/PROTOCOL.md)

---

## 🤝 找到我们

- **官网 & 开源地址**：[ccdsyy.github.io](https://ccdsyy.github.io)
- **视频教程 & 更新动态**：抖音号 **ccdsyyznb**
- **作者 QQ**：3310339901
- **官方互助 QQ 群**：1108157805（小白友好，进群随便问）

---

## ⚖️ 免责声明

- CLink 是免费开源工具，仅供学习交流 Minecraft 联机使用
- 信令借用的公共剪贴板服务由第三方运营，内容为端到端密文，本项目与其无隶属关系
- 请勿将 CLink 用于任何违反服务条款或法律法规的用途

## 📄 许可证

MIT License © 2026 ccdsyy
