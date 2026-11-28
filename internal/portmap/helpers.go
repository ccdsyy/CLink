package portmap

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/pion/stun"
)

// runCmd 执行外部命令返回 stdout（失败返回空）。
func runCmd(name string, args ...string) string {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func readProcRoute() ([]byte, error) {
	return os.ReadFile("/proc/net/route")
}

// httpGet 发起 GET 并返回正文（IGD 设备描述）。
func httpGet(ctx context.Context, url string) (string, error) {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(c, "GET", url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// httpGetQuiet 发 SOAP POST，吞错（UPnP 控制点）。
func httpGetQuiet(ctx context.Context, url, body string) string {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(c, "POST", url, bytes.NewReader([]byte(body)))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"urn:schemas-upnp-org:service:WANIPConnection:1"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ""
	}
	return string(b)
}

// extractControlURL 从 IGD 设备描述 XML 中抠 WANIPConnection 的 controlURL。
func extractControlURL(desc string) string {
	// 优先 WANIPConnection，退而 WANPPPConnection
	re := regexp.MustCompile(`(?is)<service>.*?</service>`)
	for _, svc := range re.FindAllString(desc, -1) {
		if strings.Contains(svc, "WANIPConnection") || strings.Contains(svc, "WANPPPConnection") {
			m := regexp.MustCompile(`(?is)<controlURL>\s*([^<]+?)\s*</controlURL>`).FindStringSubmatch(svc)
			if len(m) > 1 {
				return m[1]
			}
		}
	}
	return ""
}

// extractExternalIP 从 GetExternalIPAddress 响应抠 IPv4。
func extractExternalIP(resp string) string {
	m := regexp.MustCompile(`(?is)<NewExternalIPAddress>\s*([0-9.]+?)\s*</NewExternalIPAddress>`).FindStringSubmatch(resp)
	if len(m) > 1 {
		return m[1]
	}
	return ""
}

// stunPublicV4 经任一公共 STUN 获取本机公网 IPv4（无 STUN 返回空）。
func stunPublicV4() (string, error) {
	for _, server := range []string{"stun:stun.l.google.com:19302", "stun:stun.cloudflare.com:3478"} {
		conn, err := net.ListenUDP("udp4", nil)
		if err != nil {
			continue
		}
		addr, err2 := net.ResolveUDPAddr("udp4", strings.TrimPrefix(server, "stun:"))
		if err2 != nil {
			conn.Close()
			continue
		}
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		msg := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
		msg.Encode()
		if _, err := conn.WriteToUDP(msg.Raw, addr); err != nil {
			conn.Close()
			continue
		}
		buf := make([]byte, 1500)
		n, _, rerr := conn.ReadFromUDP(buf)
		conn.Close()
		if rerr != nil {
			continue
		}
		resp := &stun.Message{Raw: append([]byte{}, buf[:n]...)}
		if perr := resp.Decode(); perr == nil {
			var xa stun.XORMappedAddress
			if gerr := xa.GetFrom(resp); gerr == nil && xa.IP.To4() != nil {
				return xa.IP.String(), nil
			}
		}
	}
	return "", ErrNoStun
}

// ErrNoStun 无可用 STUN 服务器（公网 IP 未知）。
var ErrNoStun = errNoStun{}

type errNoStun struct{}

func (errNoStun) Error() string { return "STUN 不可达，公网 IP 未知" }
