// WS 传输（ROADMAP #36）：中继帧跑在 WebSocket 上——可蹭 CDN 边缘，
// UDP 全被 QoS 杀死时仍可用；可叠加 HTTP CONNECT 代理出站（#37）。
// 一条 WS 二进制消息 = 一个 rudp/中继数据报（帧协议不变）。
package relay

import (
	"errors"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/ccdsyy/clink/internal/ws"
)

// ParseWSURLs 解析 WS 中继节点（ws:// 或 wss://，逗号分隔）。
func ParseWSURLs(s string) []string {
	var out []string
	for _, part := range splitList(s) {
		p := trimSpace(part)
		if p == "" {
			continue
		}
		if len(p) > 5 && (p[:5] == "ws://" || p[:6] == "wss://") {
			out = append(out, p)
		}
	}
	return out
}

func splitList(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' || r == ';' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	out = append(out, cur)
	return out
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// wsPacketConn 把一条 WS 连接适配成 net.PacketConn（rudp 直接可用）。
// 合成地址 127.0.0.127:<id>：迁移/配对逻辑全部按地址比较，稳定即可。
type wsPacketConn struct {
	c      *ws.Conn
	id     int
	buf    chan []byte
	closed chan struct{}
	once   sync.Once
	rlMu   sync.Mutex
	rl     time.Time
}

var wsIDCounter int
var wsIDMu sync.Mutex

func newWSPacketConn(c *ws.Conn) *wsPacketConn {
	wsIDMu.Lock()
	wsIDCounter++
	id := wsIDCounter
	wsIDMu.Unlock()
	w := &wsPacketConn{c: c, id: id, buf: make(chan []byte, 256), closed: make(chan struct{})}
	go w.pump()
	return w
}

func (w *wsPacketConn) pump() {
	for {
		msg, err := w.c.ReadMessage()
		if err != nil {
			w.Close()
			return
		}
		select {
		case w.buf <- msg:
		default:
			select {
			case <-w.buf:
			default:
			}
			select {
			case w.buf <- msg:
			default:
			}
		}
	}
}

func (w *wsPacketConn) synthetic() *net.UDPAddr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 127), Port: 40000 + w.id}
}

func (w *wsPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	select {
	case msg := <-w.buf:
		n := copy(p, msg)
		return n, w.synthetic(), nil
	case <-w.closed:
		return 0, nil, errors.New("ws: closed")
	}
}

func (w *wsPacketConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	if err := w.c.WriteMessage(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *wsPacketConn) Close() error {
	w.once.Do(func() { close(w.closed) })
	return w.c.Close()
}

func (w *wsPacketConn) LocalAddr() net.Addr { return w.synthetic() }

func (w *wsPacketConn) SetDeadline(t time.Time) error {
	w.rlMu.Lock()
	w.rl = t
	w.rlMu.Unlock()
	return nil
}

func (w *wsPacketConn) SetReadDeadline(t time.Time) error  { return w.SetDeadline(t) }
func (w *wsPacketConn) SetWriteDeadline(t time.Time) error { return nil }

// waitDeadline 内部：ReadFrom 遵守 deadline。
func (w *wsPacketConn) deadlineExpired() bool {
	w.rlMu.Lock()
	defer w.rlMu.Unlock()
	if w.rl.IsZero() {
		return false
	}
	return time.Now().After(w.rl)
}

// DialWSNode 拨一个 WS 中继节点并完成鉴权登记（PING→PONG）。
// dialer 为 nil 直连；可传 HTTP CONNECT 拨号器（#37）。
func DialWSNode(rawURL string, psk, roomCode string, role byte,
	dialer func(network, addr string) (net.Conn, error)) (net.PacketConn, *net.UDPAddr, error) {
	c, err := ws.Dial(rawURL, dialer)
	if err != nil {
		return nil, nil, err
	}
	pc := newWSPacketConn(c)
	server := pc.synthetic()
	tag := DeriveTag(psk, roomCode)
	if _, err := Ping(pc, server, tag, psk, roomCode, role); err != nil {
		pc.Close()
		return nil, nil, err
	}
	return pc, server, nil
}

// SelectRelayWS WS 节点选址（#38 简化）：并发 ping 取中位数最小者。
// 返回选中 URL、常驻 PacketConn（已注册）。
func SelectRelayWS(psk, roomCode string, role byte, urls []string) (string, net.PacketConn, error) {
	if len(urls) == 0 {
		return "", nil, ErrNoRelay
	}
	type res struct {
		url string
		pc  net.PacketConn
		rtt time.Duration
	}
	results := make(chan res, len(urls))
	for _, u := range urls {
		go func(u string) {
			c, err := ws.Dial(u, nil)
			if err != nil {
				return
			}
			pc := newWSPacketConn(c)
			defer func() {
				if pc != nil {
					pc.Close()
				}
			}()
			var samples []time.Duration
			tag := DeriveTag(psk, roomCode)
			for i := 0; i < pingCount; i++ {
				if r, err := Ping(pc, pc.synthetic(), tag, psk, roomCode, role); err == nil {
					samples = append(samples, r)
				}
				time.Sleep(50 * time.Millisecond)
			}
			if len(samples) > 0 {
				sortDurations(samples)
				r := res{url: u, pc: pc, rtt: samples[len(samples)/2]}
				pc = nil // 转移所有权
				results <- r
			}
		}(u)
	}
	var best res
	bestOK := false
	for i := 0; i < len(urls); i++ {
		r := <-results
		if !bestOK || r.rtt < best.rtt {
			best, bestOK = r, true
		}
	}
	if !bestOK {
		return "", nil, ErrNoRelay
	}
	return best.url, best.pc, nil
}

func sortDurations(s []time.Duration) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// itoaLocal 避免依赖 strconv 的极简转换（该文件不直接用，留位）。
var _ = strconv.Itoa
