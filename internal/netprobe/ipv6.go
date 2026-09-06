// Package netprobe 网络环境体检：IPv6 公网地址探测 + NAT 类型初判。
// 结果用于三层降级（Tier1 IPv6 直连 / Tier2 WebRTC / Tier3 兜底提示）。
package netprobe

import (
	"net"
	"time"
)

// IPv6 探测用公共 DNS（仅做 UDP 路由探测，不实际发包）
var v6Probes = []string{
	"[2400:3200::53]:53",     // 阿里 DNS
	"[2001:4860:4860::8888]:53", // Google DNS
}

// HasIPv6Route 检测本机是否具备 IPv6 出口路由。
// UDP "拨号"只做本地路由查找，不产生实际流量，无路由立即失败。
func HasIPv6Route() bool {
	for _, p := range v6Probes {
		d := net.Dialer{Timeout: 2 * time.Second}
		c, err := d.Dial("udp6", p)
		if err == nil {
			_ = c.Close()
			return true
		}
	}
	return false
}

// PublicIPv6 列出网卡上的公网 IPv6（全局单播 2000::/3），
// 排除链路本地（fe80）、ULA（fc00::/7）、回环与 v4 映射地址。
func PublicIPv6() []net.IP {
	var out []net.IP
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP.To16()
			if ip == nil || ip.To4() != nil {
				continue // 跳过 v4
			}
			if ip.IsLinkLocalUnicast() || ip.IsLoopback() {
				continue
			}
			if ip[0] == 0xfc || ip[0] == 0xfd { // ULA 私有
				continue
			}
			if (ip[0] & 0xe0) != 0x20 { // 非全局单播 2000::/3
				continue
			}
			out = append(out, ip)
		}
	}
	return out
}

// HasIPv6 综合：有公网 v6 地址 且 有 v6 出口路由。
func HasIPv6() bool {
	return len(PublicIPv6()) > 0 && HasIPv6Route()
}
