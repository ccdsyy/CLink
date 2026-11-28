// Package lanscan LAN 组播发现（ROADMAP #66）。
//
// 房主在 239.255.66.66:46666 组播声明房间（复用双栈隧道端口）；
// 同一物理局域网的客机收到后直接 TCP 直连，跳过一切信令与穿透。
// 使用自定义组播地址与 MC 官方 224.0.2.60:4445 区分开，互不干扰。
package lanscan

import (
	"encoding/json"
	"net"
	"sync"
	"time"
)

const (
	groupAddr     = "239.255.66.66:46666"
	announceEvery = 3 * time.Second
	freshWindow   = 12 * time.Second
)

// announce 载荷。
type announce struct {
	CLink int    `json:"clink"` // 协议标记（恒为 1）
	Code  string `json:"code"`  // 房间码
	Port  int    `json:"port"`  // 房主双栈隧道端口
}

// Announcer 房主侧：周期声明。
type Announcer struct {
	code string
	port int
	conn *net.UDPConn
	stop chan struct{}
}

// StartAnnounce 开始组播声明（失败静默——无组播环境自动跳过）。
func StartAnnounce(code string, tunnelPort int) *Announcer {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 0})
	if err != nil {
		return nil
	}
	dst, err := net.ResolveUDPAddr("udp4", groupAddr)
	if err != nil {
		conn.Close()
		return nil
	}
	a := &Announcer{code: code, port: tunnelPort, conn: conn, stop: make(chan struct{})}
	go func() {
		t := time.NewTicker(announceEvery)
		defer t.Stop()
		send := func() {
			b, _ := json.Marshal(announce{CLink: 1, Code: code, Port: tunnelPort})
			_, _ = conn.WriteToUDP(b, dst)
		}
		send()
		for {
			select {
			case <-a.stop:
				return
			case <-t.C:
				send()
			}
		}
	}()
	return a
}

// Stop 停止声明。
func (a *Announcer) Stop() {
	if a == nil {
		return
	}
	select {
	case <-a.stop:
	default:
		close(a.stop)
	}
	a.conn.Close()
}

// ---- 客机侧 ----

// Scanner 监听组播，维护「房间码 → LAN 地址」新鲜表。
type Scanner struct {
	conn *net.UDPConn

	mu    sync.Mutex
	rooms map[string]lanEntry // code → 地址+时间
	stop  chan struct{}
}

type lanEntry struct {
	addr string
	seen time.Time
}

// StartScan 开始监听（失败返回 nil——无组播环境自动跳过）。
func StartScan() *Scanner {
	group, _ := net.ResolveUDPAddr("udp4", "239.255.66.66")
	// ListenMulticastUDP 自动在默认接口加组（端口被占等多开场景静默跳过）
	conn, err := net.ListenMulticastUDP("udp4", nil, group)
	if err != nil {
		return nil
	}
	s := &Scanner{conn: conn, rooms: make(map[string]lanEntry), stop: make(chan struct{})}
	go s.readLoop()
	return s
}

func (s *Scanner) readLoop() {
	buf := make([]byte, 512)
	for {
		n, from, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n < 8 {
			continue
		}
		var a announce
		if json.Unmarshal(buf[:n], &a) != nil || a.CLink != 1 || len(a.Code) != 6 || a.Port <= 0 {
			continue
		}
		s.mu.Lock()
		s.rooms[a.Code] = lanEntry{addr: net.JoinHostPort(from.IP.String(), itoa(a.Port)), seen: time.Now()}
		s.mu.Unlock()
	}
}

// Lookup 查询房间码是否在本局域网内可见（新鲜窗口内）。
func (s *Scanner) Lookup(code string) (string, bool) {
	if s == nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.rooms[code]
	if !ok || time.Since(e.seen) > freshWindow {
		delete(s.rooms, code)
		return "", false
	}
	return e.addr, true
}

// Close 停止监听。
func (s *Scanner) Close() {
	if s == nil {
		return
	}
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	s.conn.Close()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
