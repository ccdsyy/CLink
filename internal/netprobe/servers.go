package netprobe

import (
	"strings"
	"sync"
	"time"
)

// 公共 STUN / TURN 节点预设。
//
// STUN 列表为 2026-11-21 中国电信网络实测排序（详见 docs/PROTOCOL.md 附录 C）：
// Google 节点在 ICE 中最稳（对照实验 4/4 通过，小米/Cloudflare 探测可达但
// ICE 数据通道易失灵），故 ICE 首选 Google；不可达时自动回退国内可达节点。
var PublicSTUN = []string{
	"stun:stun.l.google.com:19302",  // Google 1（ICE 实测最稳，首选）
	"stun:stun1.l.google.com:19302", // Google 2
	"stun:stun.miwifi.com:3478",     // 小米（国内延迟最低，实测 83ms）
	"stun:stun.cloudflare.com:3478", // Cloudflare（国际）
	"stun:stun.nextcloud.com:443",   // Nextcloud（国际）
}

// STUNNamed 具名 STUN 节点（设置页下拉框；value 与 stunMode 对应）。
var STUNNamed = []struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}{
	{"auto", "自动选优（推荐 · Google 首选，不通自动换国内节点）"},
	{"google", "Google 公共 STUN"},
	{"miwifi", "小米公共 STUN（国内）"},
	{"cloudflare", "Cloudflare 公共 STUN"},
	{"nextcloud", "Nextcloud 公共 STUN"},
	{"custom", "自定义列表…"},
}

// TurnNode 预设公共 TURN 中继节点。
// ⚠️ 社区免费节点随时可能失效（实测 OpenRelay 已长期不可达），
//
//	仅作 Tier 3 兜底，可靠性以设置页标注为准；稳定中转建议自建 coturn。
type TurnNode struct {
	Name string `json:"name"` // 展示名（设置页复选框）
	URL  string `json:"url"`  // turn:host:port（含 transport 参数）
	Cred string `json:"cred"` // user:pass
	Note string `json:"note"` // 可靠性备注
	Dead bool   `json:"dead"` // 实测已不可达（默认不勾选）
}

// PresetTURN 预设 TURN 节点列表（设置页数据源）。
var PresetTURN = []TurnNode{
	{Name: "ExpressTurn（UDP）", URL: "turn:relay1.expressturn.com:3478", Cred: "free:free", Note: "社区免费节点，不稳定", Dead: false},
	{Name: "OpenRelay UDP 80", URL: "turn:openrelay.metered.ca:80", Cred: "openrelayproject:openrelayproject", Note: "2026-11 实测不可达", Dead: true},
	{Name: "OpenRelay TCP 443", URL: "turn:openrelay.metered.ca:443?transport=tcp", Cred: "openrelayproject:openrelayproject", Note: "2026-11 实测不可达", Dead: true},
}

// NormalizeSTUN 清洗自定义 STUN 列表（逗号/空白分隔，补 stun: 前缀）。
func NormalizeSTUN(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == '\n' }) {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		if !strings.Contains(p, ":") {
			p = "stun:" + p
		}
		out = append(out, p)
	}
	return out
}

// ---- STUN 选优 ----
//
// 实测教训（2026-11-21，电信 CGNAT 环境）：把多个 STUN 节点同时交给 ICE
// 会在 CGNAT/多网卡机器上产生大量 srflx 候选，打洞变慢甚至数据通道失灵
// （对照实验：Google 单节点 4/4 稳定，5 节点 3/3 失败）。因此 ICE 只挂一个节点。
// PickSTUN 负责"多节点容灾"：首选 ICE 实测最稳的 Google；探测不通时
// 回退最快可达的国内节点（结果缓存 10 分钟）。

var (
	stunPickMu  sync.Mutex
	stunPickVal string    // 最近一次选中的节点
	stunPickAt  time.Time // 缓存时间
)

// PickSTUN 返回本机当前可用的最佳 STUN 节点（整体 3s 超时上限）。
// 逻辑：Google 探测通过 → 直接用；否则并发探测其余节点取最快应答者；
// 全部失败时回退列表首位。结果缓存 10 分钟。
func PickSTUN() string {
	stunPickMu.Lock()
	if stunPickVal != "" && time.Since(stunPickAt) < 10*time.Minute {
		v := stunPickVal
		stunPickMu.Unlock()
		return v
	}
	stunPickMu.Unlock()

	best := PublicSTUN[0]
	if _, err := stunMapped(PublicSTUN[0]); err != nil {
		// 首选不可达：并发探测其余节点，取最快应答
		type result struct {
			node string
			rtt  time.Duration
		}
		ch := make(chan result, len(PublicSTUN)-1)
		for _, node := range PublicSTUN[1:] {
			go func(node string) {
				start := time.Now()
				if _, err := stunMapped(node); err == nil {
					ch <- result{node: node, rtt: time.Since(start)}
				}
			}(node)
		}
		bestRTT := time.Duration(1 << 62)
		timeout := time.After(3 * time.Second)
		for i := 0; i < len(PublicSTUN)-1; i++ {
			select {
			case r := <-ch:
				if r.rtt < bestRTT {
					best, bestRTT = r.node, r.rtt
				}
			case <-timeout:
				i = len(PublicSTUN) // 超时即收手
			}
		}
	}
	stunPickMu.Lock()
	stunPickVal, stunPickAt = best, time.Now()
	stunPickMu.Unlock()
	return best
}
