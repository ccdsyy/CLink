// Package relay 自建中继（ROADMAP #31/32/33/38/39）。
//
// 协议（单 UDP 数据报，与 rudp 共用帧头）：
//
//	[4B tag][1B route][body]
//	route 0x20 PING：body = [8B nonce][roomCode] —— 鉴权+保活+选址探测
//	  服务端校验 tag == HMAC-SHA256(psk, room)[:4]（#33 Token 鉴权，
//	  防公共节点被白嫖成开放代理），通过则登记 tag→发送方地址并回 PONG
//	route 0x21 PONG：原样回显 body 前 8B nonce
//	其余 route：rudp 数据帧，服务端按 tag 把 A↔B 两端互转（#31）
//
// 无状态设计（#32）：服务端仅维护 tag→两端地址 的轻量路由表（TTL 5 分钟），
// 不参与可靠传输——可靠性全部由端到端 rudp 承担，中继抗重启、可横扩。
package relay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	routePING = 0x20
	routePONG = 0x21

	RoleGuest = 0 // 房客
	RoleHost  = 1 // 房主

	entryTTL       = 5 * time.Minute // 路由表项存活
	pingCount      = 3               // 选址探测次数
	pingTimeout    = 1200 * time.Millisecond
	keepaliveEvery = 25 * time.Second
)

var ErrNoRelay = errors.New("relay: 无可用中继节点")

// DeriveTag 由 PSK 与房间码派生 4B 会话标签（客户端与服务端一致计算）。
func DeriveTag(psk, roomCode string) uint32 {
	mac := hmac.New(sha256.New, []byte(psk))
	mac.Write([]byte("CLink-relay-v1\n"))
	mac.Write([]byte(roomCode))
	return binary.BigEndian.Uint32(mac.Sum(nil)[:4])
}

// ParseAddrs 解析中继节点列表（"udp://ip:port" / "ip:port"，逗号分隔）。
func ParseAddrs(s string) []*net.UDPAddr {
	var out []*net.UDPAddr
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == '\n' }) {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		p = strings.TrimPrefix(p, "udp://")
		if _, _, err := net.SplitHostPort(p); err != nil {
			continue
		}
		if a, err := net.ResolveUDPAddr("udp4", p); err == nil {
			out = append(out, a)
		}
	}
	return out
}

// Ping 一次中继探测（鉴权 PING → PONG），返回 RTT。
// body = nonce8 + role1 + roomCode；role 显式声明本端角色，
// 服务端按角色登记地址（每端可能先后有两个 socket：探测+常驻，
// 靠"第二地址"猜对端必然错乱）。
func Ping(conn *net.UDPConn, server *net.UDPAddr, tag uint32, psk, roomCode string, role byte) (time.Duration, error) {
	_ = conn.SetReadDeadline(time.Now().Add(pingTimeout))
	nonce := make([]byte, 8)
	binary.BigEndian.PutUint64(nonce, uint64(time.Now().UnixNano()))
	body := append(append(nonce, role), roomCode...)
	msg := buildFrame(tag, routePING, body)
	if _, err := conn.WriteToUDP(msg, server); err != nil {
		return 0, err
	}
	start := time.Now()
	buf := make([]byte, 256)
	for time.Since(start) < pingTimeout {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		if n >= 13 && binary.BigEndian.Uint32(buf[0:4]) == tag && buf[4] == routePONG &&
			from.IP.Equal(server.IP) && bytesEqual(buf[5:13], nonce) {
			_ = conn.SetReadDeadline(time.Time{}) // 清 deadline，共享 socket 的其他读者不受影响
			return time.Since(start), nil
		}
	}
	_ = conn.SetReadDeadline(time.Time{})
	return 0, errors.New("relay: ping 超时")
}

// SelectRelay 多节点选址（#38）：并发 ping 全部节点取 RTT 中位数最小者。
// 顺带完成 tag 注册（所有可达节点都注册，作为 #39 的热备）。
func SelectRelay(psk, roomCode string, role byte, servers []*net.UDPAddr) (*net.UDPAddr, *net.UDPConn, error) {
	if len(servers) == 0 {
		return nil, nil, ErrNoRelay
	}
	tag := DeriveTag(psk, roomCode)

	type res struct {
		addr *net.UDPAddr
		rtt  time.Duration
	}
	results := make(chan res, len(servers))
	for _, s := range servers {
		go func(s *net.UDPAddr) {
			conn, err := net.ListenUDP("udp4", nil)
			if err != nil {
				return
			}
			defer conn.Close()
			var samples []time.Duration
			for i := 0; i < pingCount; i++ {
				if rtt, err := Ping(conn, s, tag, psk, roomCode, role); err == nil {
					samples = append(samples, rtt)
				}
				time.Sleep(50 * time.Millisecond)
			}
			if len(samples) > 0 {
				sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
				results <- res{addr: s, rtt: samples[len(samples)/2]} // 中位数
			}
		}(s)
	}

	var best *net.UDPAddr
	bestRTT := time.Duration(1 << 62)
	for i := 0; i < len(servers); i++ {
		r := <-results
		if r.rtt < bestRTT {
			best, bestRTT = r.addr, r.rtt
		}
	}
	if best == nil {
		return nil, nil, ErrNoRelay
	}
	// 选中节点的 conn：客户端直接在该 socket 上跑（已完成注册）
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return nil, nil, err
	}
	if _, err := Ping(conn, best, tag, psk, roomCode, role); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return best, conn, nil
}

// KeepaliveLoop 周期保活（刷新服务端路由表 + 维持 NAT 映射）。
func KeepaliveLoop(conn *net.UDPConn, server *net.UDPAddr, tag uint32, psk, roomCode string, role byte, stop <-chan struct{}) {
	t := time.NewTicker(keepaliveEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			_, _ = Ping(conn, server, tag, psk, roomCode, role)
		}
	}
}

// buildFrame 组帧。
func buildFrame(tag uint32, route byte, body []byte) []byte {
	out := make([]byte, 5, 5+len(body))
	binary.BigEndian.PutUint32(out[0:4], tag)
	out[4] = route
	out = append(out, body...)
	return out
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---- 服务端（#31/#32）----

type entry struct {
	guest, host *net.UDPAddr // 按角色登记的两端地址
	lastSeen    time.Time
}

// Server 无状态转发服务端。
type Server struct {
	psk    string
	mu     sync.Mutex
	routes map[uint32]*entry
}

// NewServer 创建中继服务端（psk 为共享口令，客户端配置一致）。
func NewServer(psk string) *Server {
	s := &Server{psk: psk, routes: make(map[uint32]*entry)}
	go s.cleanupLoop()
	return s
}

// ServeUDP 处理一个数据报。
func (s *Server) ServeUDP(conn *net.UDPConn) {
	buf := make([]byte, 2048)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n < 5 {
			continue
		}
		tag := binary.BigEndian.Uint32(buf[0:4])
		route := buf[4]
		body := buf[5:n]

		if route == routePING {
			s.handlePing(conn, from, tag, body)
			continue
		}
		if route == routePONG {
			continue
		}
		// 数据帧：按 tag 转发给另一端
		if peer := s.peer(tag, from); peer != nil {
			_, _ = conn.WriteToUDP(buf[:n], peer)
		}
	}
}

// handlePing 鉴权 + 登记地址 + 回 PONG。
func (s *Server) handlePing(conn *net.UDPConn, from *net.UDPAddr, tag uint32, body []byte) {
	if len(body) < 8+1+1 {
		return
	}
	nonce := body[:8]
	role := body[8]
	room := string(body[9:])
	if role != RoleGuest && role != RoleHost {
		return
	}
	if !validTag(s.psk, room, tag) {
		return // #33：鉴权失败静默丢弃
	}
	s.mu.Lock()
	e := s.routes[tag]
	if e == nil {
		e = &entry{lastSeen: time.Now()}
		s.routes[tag] = e
	}
	if role == RoleGuest {
		e.guest = from
	} else {
		e.host = from
	}
	e.lastSeen = time.Now()
	s.mu.Unlock()

	resp := buildFrame(tag, routePONG, nonce)
	_, _ = conn.WriteToUDP(resp, from)
}

// peer 返回 from 角色对应的对端地址（另一角色登记的地址）。
func (s *Server) peer(tag uint32, from *net.UDPAddr) *net.UDPAddr {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.routes[tag]
	if e == nil {
		return nil
	}
	e.lastSeen = time.Now()
	same := func(a, b *net.UDPAddr) bool {
		return a != nil && b != nil && a.IP.Equal(b.IP) && a.Port == b.Port
	}
	if same(from, e.guest) {
		return e.host
	}
	if same(from, e.host) {
		return e.guest
	}
	return nil // 未知来源：丢弃（防伪造）
}

// validTag 校验 tag = HMAC(psk, room)[:4]。
func validTag(psk, room string, tag uint32) bool {
	return DeriveTag(psk, room) == tag
}

func (s *Server) cleanupLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		s.mu.Lock()
		for k, e := range s.routes {
			if time.Since(e.lastSeen) > entryTTL {
				delete(s.routes, k)
			}
		}
		s.mu.Unlock()
	}
}

// AddrString 规范化 "ip:port" 展示。
func AddrString(a *net.UDPAddr) string {
	return net.JoinHostPort(a.IP.String(), strconv.Itoa(a.Port))
}
