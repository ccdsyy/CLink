// Package portmap 自实现端口映射三件套（ROADMAP #22/23/24）：
// PCP(RFC 6887) → NAT-PMP(RFC 6886) → UPnP IGD(SSDP+SOAP)，外加映射有效性
// 自检（#25，经公网回打/STUN 实测，不信路由器的"成功了"）。
// 成功后房主可把公网 TCP 端点写进信令，对端免穿透直连（Tier 1.5）。
package portmap

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// Result 端口映射结果。
type Result struct {
	Method       string // pcp | natpmp | upnp
	InternalPort int
	ExternalPort int
	ExternalIP   string // 公网 IPv4（UPnP GetExternalIPAddress 或接口推断）
	Verified     bool   // #25：经回环/公网实测可达
}

// gatewayIP 探测默认网关 IPv4（Windows route print / Linux /proc/net.route）。
func gatewayIP() net.IP {
	if ips := gatewayCandidates(); len(ips) > 0 {
		return ips[0]
	}
	return nil
}

func gatewayCandidates() []net.IP {
	var out []net.IP
	// Windows: route print -4 的 "0.0.0.0  0.0.0.0  <gw>" 行
	if out2 := gatewaysFromCommand("route", "print", "-4"); len(out2) > 0 {
		out = append(out, out2...)
	}
	// Linux/macOS 常规
	if ips, err := gatewaysFromProc(); err == nil && len(ips) > 0 {
		out = append(out, ips...)
	}
	// 去重
	seen := map[string]bool{}
	var uniq []net.IP
	for _, ip := range out {
		if ip == nil || ip.IsLoopback() {
			continue
		}
		k := ip.String()
		if !seen[k] {
			seen[k] = true
			uniq = append(uniq, ip)
		}
	}
	return uniq
}

func gatewaysFromCommand(name string, args ...string) []net.IP {
	out := runCmd(name, args...)
	var res []net.IP
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) >= 3 && f[0] == "0.0.0.0" && f[1] == "0.0.0.0" {
			if ip := net.ParseIP(f[2]); ip != nil && ip.To4() != nil {
				res = append(res, ip)
			}
		}
	}
	return res
}

// gatewaysFromProc 解析 /proc/net/route（Linux）。
func gatewaysFromProc() ([]net.IP, error) {
	data, err := readProcRoute()
	if err != nil {
		return nil, err
	}
	var res []net.IP
	for _, line := range strings.Split(string(data), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) >= 3 && f[0] != "Iface" && f[1] == "00000000" {
			// 小端 hex
			b := make([]byte, 4)
			for i := 0; i < 4; i++ {
				if hi, err := strconv.ParseUint(f[2][i*2:i*2+2], 16, 8); err == nil {
					b[3-i] = byte(hi)
				}
			}
			res = append(res, net.IP(b))
		}
	}
	return res, nil
}

// ---- NAT-PMP（#23）与 PCP（#24）----

// mapPMP 用 NAT-PMP 请求 TCP 端口映射（opcode 2）。
func mapPMP(ctx context.Context, gw net.IP, internalPort int, lifetime time.Duration) (int, error) {
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "udp4", net.JoinHostPort(gw.String(), "5351"))
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	sec := uint32(lifetime / time.Second)
	req := make([]byte, 12)
	req[0] = 0 // version
	req[1] = 2 // opcode: MAP TCP
	binary.BigEndian.PutUint16(req[4:], uint16(internalPort))
	binary.BigEndian.PutUint16(req[6:], 0) // 请求随机外部端口
	binary.BigEndian.PutUint32(req[8:], sec)
	if _, err := conn.Write(req); err != nil {
		return 0, err
	}

	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if err != nil {
		return 0, err
	}
	if n < 12 || buf[1] != 0x82 {
		return 0, errors.New("pmp 响应异常")
	}
	if code := binary.BigEndian.Uint16(buf[2:4]); code != 0 {
		return 0, fmt.Errorf("pmp 拒绝（code=%d）", code)
	}
	return int(binary.BigEndian.Uint16(buf[6:8])), nil
}

// mapPCP 用 PCP MAP 请求 TCP 端口映射（RFC 6887）。
func mapPCP(ctx context.Context, gw net.IP, internalPort int, lifetime time.Duration) (int, error) {
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "udp4", net.JoinHostPort(gw.String(), "5351"))
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	sec := uint32(lifetime / time.Second)
	// 24B 头 + 12B MAP opcode：protocol(3)=TCP(6) reserved internalPort externalPort
	req := make([]byte, 24+12)
	req[0] = 2 // version 2 (PCP)
	req[1] = 1 // opcode MAP
	binary.BigEndian.PutUint32(req[4:], sec)
	nonce := []byte("CLinkPcpNonce01") // 12B
	copy(req[8:20], nonce)
	op := req[24:]
	op[2] = 6 // protocol TCP
	binary.BigEndian.PutUint16(op[4:6], uint16(internalPort))
	binary.BigEndian.PutUint16(op[6:8], 0)
	if _, err := conn.Write(req); err != nil {
		return 0, err
	}

	buf := make([]byte, 60)
	n, err := conn.Read(buf)
	if err != nil {
		return 0, err
	}
	if n < 24 || buf[0] != 2 || buf[1] != 0x81 {
		return 0, errors.New("pcp 响应异常")
	}
	if code := buf[3]; code != 0 {
		return 0, fmt.Errorf("pcp 拒绝（code=%d）", code)
	}
	if n < 36 {
		return 0, errors.New("pcp 响应缺 MAP 载荷")
	}
	return int(binary.BigEndian.Uint16(buf[24+8 : 24+10])), nil
}

// ---- UPnP IGD（#22）----

// mapUPnP 经 SSDP 发现 IGD 并 SOAP 调 AddPortMapping（TCP）。
func mapUPnP(ctx context.Context, internalPort int, lifetime time.Duration) (extPort int, extIP string, err error) {
	location, lerr := ssdpDiscover(ctx)
	if lerr != nil {
		return 0, "", lerr
	}
	desc, derr := httpGet(ctx, location)
	if derr != nil {
		return 0, "", derr
	}
	controlURL := extractControlURL(desc)
	if controlURL == "" {
		return 0, "", errors.New("未找到 WANIPConnection controlURL")
	}
	// controlURL 可能是相对路径
	if !strings.HasPrefix(controlURL, "http") {
		base := location
		if i := strings.Index(base[8:], "/"); i >= 0 {
			base = base[:8+i]
		}
		controlURL = base + controlURL
	}
	// 公网 IP
	extIP = extractExternalIP(httpGetQuiet(ctx, controlURL, soapBody("GetExternalIPAddress", "")))

	extPort = internalPort
	body := fmt.Sprintf(`<NewRemoteHost></NewRemoteHost><NewExternalPort>%d</NewExternalPort>`+
		`<NewProtocol>TCP</NewProtocol><NewInternalPort>%d</NewInternalPort>`+
		`<NewInternalClient>127.0.0.1</NewInternalClient>`+
		`<NewEnabled>1</NewEnabled><NewPortMappingDescription>CLink</NewPortMappingDescription>`+
		`<NewLeaseDuration>%d</NewLeaseDuration>`, extPort, internalPort, int(lifetime/time.Second))
	resp := httpGetQuiet(ctx, controlURL, soapBody("AddPortMapping", body))
	if strings.Contains(resp, "errorCode") && !strings.Contains(resp, "716") && !strings.Contains(resp, "ConflictInMappingEntry") {
		// 外部端口被占时退化为不指定端口重试一次
		return 0, extIP, errors.New("UPnP AddPortMapping 被拒")
	}
	return extPort, extIP, nil
}

func soapBody(action, args string) string {
	return `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" ` +
		`s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>` +
		`<u:` + action + ` xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1">` + args +
		`</u:` + action + `></s:Body></s:Envelope>`
}

func ssdpDiscover(ctx context.Context) (string, error) {
	pc, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return "", err
	}
	defer pc.Close()
	gw, _ := net.ResolveUDPAddr("udp4", "239.255.255.250:1900")
	msg := strings.ReplaceAll("M-SEARCH * HTTP/1.1\r\n"+
		"HOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\n"+
		"ST: urn:schemas-upnp-org:service:WANIPConnection:1\r\n\r\n", "\n", "\r\n")
	if _, err := pc.WriteTo([]byte(msg), gw); err != nil {
		return "", err
	}
	deadline := time.Now().Add(3 * time.Second)
	buf := make([]byte, 2048)
	for time.Now().Before(deadline) {
		_ = pc.SetReadDeadline(deadline)
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			break
		}
		for _, line := range strings.Split(string(buf[:n]), "\n") {
			if strings.HasPrefix(strings.ToLower(line), "location:") {
				return strings.TrimSpace(line[9:]), nil
			}
		}
	}
	return "", errors.New("SSDP 未发现 IGD")
}

// ---- 组合入口 ----

// Map 尝试建立 internalPort(TCP) 的公网映射：PCP → NAT-PMP → UPnP，
// 成功后做 #25 有效性自检（公网 IP 回环拨号实测）。
func Map(ctx context.Context, internalPort int) *Result {
	gw := gatewayIP()
	lifetime := 2 * time.Hour
	// 先拿公网 IPv4（STUN），既用于校验也用于回环自检
	extIP := ""
	if a, err := stunPublicV4(); err == nil {
		extIP = a
	}

	try := []struct {
		name string
		fn   func() (int, error)
	}{
		{"pcp", func() (int, error) {
			if gw == nil {
				return 0, errors.New("无网关")
			}
			return mapPCP(ctx, gw, internalPort, lifetime)
		}},
		{"natpmp", func() (int, error) {
			if gw == nil {
				return 0, errors.New("无网关")
			}
			return mapPMP(ctx, gw, internalPort, lifetime)
		}},
		{"upnp", func() (int, error) {
			p, ip, err := mapUPnP(ctx, internalPort, lifetime)
			if err == nil && ip != "" {
				extIP = ip // UPnP 报告的公网 IP 更可靠
			}
			return p, err
		}},
	}
	for _, t := range try {
		if ctx.Err() != nil {
			return nil
		}
		extPort, err := t.fn()
		if err != nil || extPort == 0 {
			continue
		}
		r := &Result{Method: t.name, InternalPort: internalPort, ExternalPort: extPort, ExternalIP: extIP}
		r.Verified = verifyHairpin(extIP, extPort)
		return r
	}
	return nil
}

// verifyHairpin #25：从本机经公网 IP:端口拨回，通了才认为映射真有效
// （多层 NAT 下 UPnP 常常映射的是第一层，等于白映射）。
func verifyHairpin(extIP string, extPort int) bool {
	if extIP == "" {
		return false
	}
	conn, err := net.DialTimeout("tcp4", net.JoinHostPort(extIP, strconv.Itoa(extPort)), 3*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
