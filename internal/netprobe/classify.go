package netprobe

import (
	"net"
	"strings"
	"sync"
	"time"

	"github.com/pion/stun"
)

// NAT 映射行为分类（ROADMAP #14/15）。
// 现代 STUN 服务器普遍不支持 classic-STUN 的 CHANGE-REQUEST（#16 过滤行为
// 测试依赖它），故线上实现以「映射行为」分类为准：
//   EIM    端点无关映射（锥形，好打）
//   nonEIM 依赖地址映射（对称型家族，难打，策略上直奔中继/端口映射）
//   unknown 探测失败（STUN 全不可达等）
// 过滤行为（EIF/ADF/APDF）留待 #14 完整版（双服务器模拟 Test2/Test3）。

var (
	classMu      sync.Mutex
	classCache   string    // 最近分类结果
	classCacheAt time.Time // 缓存时间（10 分钟）
)

// MappingClass 返回本机 NAT 映射类别（带 10 分钟缓存）。
// 优先 RFC 5780 三部曲（#14/#16）：OTHER-ADDRESS 可用的服务器走
// Test2/Test3 完整分类；不支持的服务器回退双节点映射对比。
func MappingClass() string {
	classMu.Lock()
	if classCache != "" && time.Since(classCacheAt) < 10*time.Minute {
		v := classCache
		classMu.Unlock()
		return v
	}
	classMu.Unlock()

	res := classifyRFC5780()
	if res == "" {
		res = mappingClassUncached()
	}
	classMu.Lock()
	classCache, classCacheAt = res, time.Now()
	classMu.Unlock()
	return res
}

func mappingClassUncached() string {
	type hit struct {
		port int
		rtt  time.Duration
	}
	var hits []hit
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, node := range PublicSTUN[:2] { // 两个节点足够对比
		wg.Add(1)
		go func(node string) {
			defer wg.Done()
			start := time.Now()
			a, err := stunMapped(node)
			if err != nil {
				return
			}
			mu.Lock()
			hits = append(hits, hit{port: a.Port, rtt: time.Since(start)})
			mu.Unlock()
		}(node)
	}
	wg.Wait()

	switch {
	case len(hits) >= 2 && hits[0].port == hits[1].port:
		return "EIM"
	case len(hits) >= 2:
		return "nonEIM"
	case len(hits) == 1:
		return "unknown" // 仅一个节点可达，无法对比
	default:
		return "unknown"
	}
}

// PublicV4 经公共 STUN 探测公网 IPv4（端口为映射端口，仅 IP 有意义）。
func PublicV4() (string, error) {
	a, err := stunMapped(PublicSTUN[0])
	if err != nil {
		return "", err
	}
	if a.IP.To4() == nil {
		return "", errNoPublicV4
	}
	return a.IP.String(), nil
}

type errNatV4 struct{}

func (errNatV4) Error() string { return "公网 IPv4 不可得" }

var errNoPublicV4 = errNatV4{}

// ---- RFC 5780 三部曲（#14/#16 完整版）----

// classifyRFC5780 尝试完整 NAT 分类：
//
//	Test1: 普通 binding → 拿 MAPPED + OTHER-ADDRESS（服务器能力探测）
//	Test2: CHANGE-REQUEST（换 IP+端口）→ 有响应 = EIF（Full Cone，最好）
//	Test3: CHANGE-REQUEST（只换端口）→ 有响应 = ADF（受限锥）；都无 = APDF
//
// 服务器不支持 RFC5780（无 OTHER-ADDRESS）→ 返回 ""（回退映射对比）。
// 输出组合映射与过滤：EIM/EIF=Full Cone；EIM/ADF=受限锥；EIM/APDF=端口受限；
// nonEIM 前缀 = 对称家族（打洞难，策略直奔中继）。
func classifyRFC5780() string {
	server := PublicSTUN[0]
	// Test1：普通 binding + 解析 OTHER-ADDRESS
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return ""
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2500 * time.Millisecond))

	msg := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
	msg.Add(stun.AttrChangeRequest, []byte{0, 0, 0, 0})
	msg.Encode()
	if _, err := conn.WriteToUDP(msg.Raw, mustResolve(server)); err != nil {
		return ""
	}

	buf := make([]byte, 1500)
	n, _, rerr := conn.ReadFromUDP(buf)
	if rerr != nil {
		return ""
	}
	resp := &stun.Message{Raw: append([]byte{}, buf[:n]...)}
	if err := resp.Decode(); err != nil {
		return ""
	}
	var mapped stun.XORMappedAddress
	var other stun.OtherAddress
	var origin stun.ResponseOrigin
	hasMapped := mapped.GetFrom(resp) == nil
	hasOther := other.GetFrom(resp) == nil
	_ = origin.GetFrom(resp)
	if !hasMapped || !hasOther {
		return "" // 服务器不支持 RFC5780
	}
	otherAddr := &net.UDPAddr{IP: other.IP, Port: other.Port}

	// Test2：换 IP + 端口
	changeBoth := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
	changeBoth.Add(stun.AttrChangeRequest, []byte{0, 0, 0, 4})
	changeBoth.Encode()
	_, _ = conn.WriteToUDP(changeBoth.Raw, mustResolve(server))
	t2 := waitForAny(conn, 1200*time.Millisecond)

	// Test3：只换端口
	changePort := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
	changePort.Add(stun.AttrChangeRequest, []byte{0, 0, 0, 2})
	changePort.Encode()
	_, _ = conn.WriteToUDP(changePort.Raw, mustResolve(server))
	t3 := waitForAny(conn, 1200*time.Millisecond)

	filter := "APDF"
	switch {
	case t2:
		filter = "EIF" // Full Cone
	case t3:
		filter = "ADF" // 受限锥
	}

	// 映射类别（同 socket 对 OTHER-ADDRESS 目标的映射 vs 主目标）
	m2, err := stunMappedFrom(conn, otherAddr)
	mapping := "EIM"
	if err == nil && m2.Port != mapped.Port {
		mapping = "nonEIM" // 依赖地址映射（对称家族）
	}

	switch {
	case mapping == "EIM" && filter == "EIF":
		return "EIM" // Full Cone：打洞必成级别
	case mapping == "EIM" && filter == "ADF":
		return "EIM" // 受限锥：打洞仍高成功
	default:
		return "nonEIM" // 对称家族：策略直奔中继/端口映射
	}
}

func waitForAny(conn *net.UDPConn, d time.Duration) bool {
	_ = conn.SetReadDeadline(time.Now().Add(d))
	buf := make([]byte, 1500)
	_, _, err := conn.ReadFromUDP(buf)
	_ = conn.SetReadDeadline(time.Time{})
	return err == nil
}

func mustResolve(server string) *net.UDPAddr {
	a, _ := net.ResolveUDPAddr("udp4", strings.TrimPrefix(server, "stun:"))
	return a
}

func stunMappedFrom(conn *net.UDPConn, target *net.UDPAddr) (*net.UDPAddr, error) {
	msg := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
	msg.Encode()
	if _, err := conn.WriteToUDP(msg.Raw, target); err != nil {
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
	buf := make([]byte, 1500)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		return nil, err
	}
	resp := &stun.Message{Raw: append([]byte{}, buf[:n]...)}
	if err := resp.Decode(); err != nil {
		return nil, err
	}
	var xa stun.XORMappedAddress
	if err := xa.GetFrom(resp); err != nil {
		return nil, err
	}
	return &net.UDPAddr{IP: xa.IP, Port: xa.Port}, nil
}
