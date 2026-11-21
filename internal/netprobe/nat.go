package netprobe

import (
	"fmt"
	"net"
	"time"

	"github.com/pion/stun"
)

// Report 网络体检报告（UI 展示 + 降级决策）。
type Report struct {
	HasV6      bool     `json:"hasV6"`      // 具备 IPv6 直连条件
	V6Addrs    []string `json:"v6Addrs"`    // 公网 IPv6 列表
	V4         bool     `json:"v4"`         // STUN 探测成功（有公网 UDP 出口）
	PublicAddr string   `json:"publicAddr"` // 公网映射地址
	NATType    string   `json:"natType"`    // NAT 类型描述
	Tier       string   `json:"tier"`       // 预期连接方式
	STUNNode   string   `json:"stunNode"`   // 首个应答的 STUN 节点（诊断展示）
	RTTMS      int64    `json:"rttMs"`      // STUN 往返延迟（毫秒）
}

// Probe 执行体检：IPv6 检测 + 多节点 STUN 对比判断 NAT 类型。
// 取前两个可达节点的映射端口对比：端口不同 → 对称型 NAT（打洞成功率低，Tier 3 风险）。
func Probe() Report {
	var r Report
	for _, ip := range PublicIPv6() {
		r.V6Addrs = append(r.V6Addrs, ip.String())
	}
	r.HasV6 = len(r.V6Addrs) > 0 && HasIPv6Route()

	type hit struct {
		addr *net.UDPAddr
		node string
		rtt  int64
	}
	var hits []hit
	for _, node := range PublicSTUN {
		start := time.Now()
		a, err := stunMapped(node)
		if err != nil {
			continue // 该节点不可达（国内/国外差异常态），换下一个
		}
		hits = append(hits, hit{addr: a, node: node, rtt: time.Since(start).Milliseconds()})
		if len(hits) == 2 {
			break
		}
	}

	if len(hits) > 0 {
		r.V4 = true
		r.PublicAddr = hits[0].addr.String()
		r.STUNNode = hits[0].node
		r.RTTMS = hits[0].rtt
	}
	if len(hits) == 2 && hits[0].addr.Port == hits[1].addr.Port {
		r.NATType = "普通 NAT（打洞成功率高）"
	} else if len(hits) == 2 {
		r.NATType = "对称 NAT（打洞较难，建议热点或中转）"
	} else if len(hits) == 1 {
		r.NATType = "STUN 部分可达"
	} else {
		r.NATType = "无公网 UDP 出口"
	}

	switch {
	case r.HasV6:
		r.Tier = "IPv6 极速直连"
	case r.V4:
		r.Tier = "P2P 打洞"
	default:
		r.Tier = "网络受限（可能需要中转）"
	}
	return r
}

// stunMapped 向 STUN 服务器查询本机公网映射地址（3 秒超时）。
func stunMapped(server string) (*net.UDPAddr, error) {
	conn, err := net.DialTimeout("udp", server, 3*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	c, err := stun.NewClient(conn)
	if err != nil {
		return nil, err
	}
	defer c.Close()

	msg := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
	var xa stun.XORMappedAddress
	var qErr error
	if err := c.Do(msg, func(ev stun.Event) {
		if ev.Error != nil {
			qErr = ev.Error
			return
		}
		if e := xa.GetFrom(ev.Message); e != nil {
			qErr = e
		}
	}); err != nil || qErr != nil {
		return nil, fmt.Errorf("stun query failed: %v/%v", err, qErr)
	}
	return &net.UDPAddr{IP: xa.IP, Port: xa.Port}, nil
}
