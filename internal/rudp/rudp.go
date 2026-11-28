// Package rudp 自研可靠 UDP 有序字节流（ROADMAP #41 自研版）。
//
// 线上格式（单个 UDP 数据报）：
//
//	[4B tag][1B route][body]
//
//	route=0x00        加密内层：body=[nonce12][GCM(kind‖payload)]（#51 简化版）
//	route=rudp kind   明文模式（回环/无密钥）
//	route=0x20/0x21   中继层 PING/PONG（明文，中继自身处理，见 internal/relay）
//
// 设计要点：
//   - 单 Endpoint 拥有一个本地 UDP socket 承载任意多会话，4B tag 路由——
//     NAT 只占一条表项（#52 多路复用）
//   - 应用层分片+重组，MTU 钳 1200 不赌 IP 分片（#46/#47）
//   - 序号 + 累积确认 + 选择性位图 + RTO 重传 + 乱序重排
//   - tag 明文供中继路由；密文对中继不可读（端到端）
package rudp

import (
	"crypto/aes"
	"crypto/cipher"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"runtime"
	"sync"
	"time"
)

const (
	innerDATA = 0x10 // 内层：可靠数据
	innerACK  = 0x11
	innerFIN  = 0x12
	innerKEEP = 0x13
	innerPING = 0x14
	innerPONG = 0x15

	routeENCRYPTED = 0x00 // 加密内层
	routePING      = 0x20 // 中继层探测（relay 包使用）
	routePONG      = 0x21
)

const (
	// MTU 钳制（#46）：IPv4/UDP 28 + tag 4 + route 1 + nonce 12 + GCM 16 + seq 4
	// → 应用分片 ≤1120B，整报文 <1200B，规避 IP 分片黑洞。
	maxDatagram = 1200
	ChunkSize   = 1120

	windowSize    = 64
	reorderMax    = windowSize * 4
	rtoMin        = 200 * time.Millisecond
	rtoMax        = 2 * time.Second
	ackDelay      = 20 * time.Millisecond
	sessionExpiry = 90 * time.Second
)

var (
	ErrClosed    = errors.New("rudp: 会话已关闭")
	ErrDecrypt   = errors.New("rudp: 解密失败（密钥不符或被篡改）")
	ErrNoSession = errors.New("rudp: 等待的会话未到来")
)

// Endpoint 单 socket 多会话端点。
type Endpoint struct {
	conn *net.UDPConn

	mu         sync.Mutex
	sessions   map[uint32]*Stream
	acceptCh   map[uint32]chan *Stream
	acceptKeys map[uint32][]byte
	closed     bool
}

// NewEndpoint 绑定本地 UDP 端口（port=0 随机）。
func NewEndpoint(port int) (*Endpoint, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: port})
	if err != nil {
		return nil, err
	}
	e := &Endpoint{
		conn:       conn,
		sessions:   make(map[uint32]*Stream),
		acceptCh:   make(map[uint32]chan *Stream),
		acceptKeys: make(map[uint32][]byte),
	}
	go e.readLoop()
	return e, nil
}

// Wrap 包装既有 UDPConn 为端点（与中继 PING/选址共用同一 socket——
// 单 socket 多路复用 #52：NAT 只占一条表项，中继按源地址配对才一致）。
func Wrap(conn *net.UDPConn) *Endpoint {
	e := &Endpoint{
		conn:       conn,
		sessions:   make(map[uint32]*Stream),
		acceptCh:   make(map[uint32]chan *Stream),
		acceptKeys: make(map[uint32][]byte),
	}
	go e.readLoop()
	return e
}

// LocalPort 实际本地 UDP 端口。
func (e *Endpoint) LocalPort() int { return e.conn.LocalAddr().(*net.UDPAddr).Port }

// Open 以指定标签向 remote 打开（或复用）一条可靠流（主动侧）。
func (e *Endpoint) Open(remote *net.UDPAddr, tag uint32, key []byte) (*Stream, error) {
	e.mu.Lock()
	if s, ok := e.sessions[tag]; ok && !s.isDead() {
		e.mu.Unlock()
		return s, nil
	}
	e.mu.Unlock()

	s := newStream(e, remote, tag, key)
	e.mu.Lock()
	e.sessions[tag] = s
	e.mu.Unlock()
	startLoops(s)
	go func() { // KEEP 重发：对端 AcceptStream 未就绪时首包会丢
		for i := 0; i < 3 && !s.isDead(); i++ {
			s.sendFrame(innerKEEP, nil)
			time.Sleep(200 * time.Millisecond)
		}
	}()
	return s, nil
}

// AcceptStream 等待指定标签的入站流（被动侧，timeout 0=无限等）。
func (e *Endpoint) AcceptStream(tag uint32, key []byte, timeout time.Duration) (*Stream, error) {
	e.mu.Lock()
	if s, ok := e.sessions[tag]; ok {
		e.mu.Unlock()
		return s, nil
	}
	ch := e.acceptCh[tag]
	if ch == nil {
		ch = make(chan *Stream, 1)
		e.acceptCh[tag] = ch
		e.acceptKeys[tag] = key
	}
	e.mu.Unlock()

	var timerC <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		timerC = timer.C
	}
	select {
	case s := <-ch:
		return s, nil
	case <-timerC:
		e.mu.Lock()
		delete(e.acceptCh, tag)
		delete(e.acceptKeys, tag)
		e.mu.Unlock()
		return nil, ErrNoSession
	}
}

// Close 关闭端点与全部会话。
func (e *Endpoint) Close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	ss := make([]*Stream, 0, len(e.sessions))
	for _, s := range e.sessions {
		ss = append(ss, s)
	}
	e.sessions = map[uint32]*Stream{}
	e.mu.Unlock()
	for _, s := range ss {
		s.close(false)
	}
	_ = e.conn.Close()
}

// ---- 收包循环 ----

func (e *Endpoint) readLoop() {
	buf := make([]byte, 2048)
	for {
		n, raddr, err := e.conn.ReadFromUDP(buf)
		if err != nil {
			// 共享 socket 上别处（如中继选址 Ping）留下的过期 deadline
			// 会立刻 i/o timeout——继续读，不当作端点关闭
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		if n < 5 {
			continue
		}
		tag := binary.BigEndian.Uint32(buf[0:4])
		route := buf[4]
		body := buf[5:n]

		if route == routePING || route == routePONG {
			continue // 中继层帧由 relay 包处理，rudp 端点不关心
		}
		if rudpDebug && route == routeENCRYPTED {
			log.Printf("[rudp@%d] 到达 tag=%x len=%d from=%s head=%x", e.conn.LocalAddr().(*net.UDPAddr).Port, tag, n, raddr, buf[5:21])
		}

		e.mu.Lock()
		s := e.sessions[tag]
		ch := e.acceptCh[tag]
		// 被动建流（加密/明文皆可）：仅当有人 AcceptStream 等着（已鉴权意图）才接受
		if s == nil && (route == routeENCRYPTED || route == innerKEEP || route == innerDATA) {
			if ch != nil {
				key := e.acceptKeys[tag]
				if rudpDebug {
					log.Printf("[rudp@%d] 被动建流 tag=%x keylen=%d", e.conn.LocalAddr().(*net.UDPAddr).Port, tag, len(key))
				}
				ns := newStream(e, raddr, tag, key)
				e.sessions[tag] = ns
				delete(e.acceptCh, tag)
				delete(e.acceptKeys, tag)
				startLoops(ns)
				select {
				case ch <- ns:
				default:
				}
				s = ns
			}
		}
		e.mu.Unlock()

		if s == nil {
			continue // 无人等待的流：丢弃
		}

		kind := route
		payload := body
		if route == routeENCRYPTED {
			if s.key == nil {
				continue // 加密帧必须配密钥流
			}
			var err error
			kind, payload, err = s.decrypt(body)
			if err != nil {
				if rudpDebug {
					log.Printf("[rudp@%d] 解密失败 tag=%x: %v（key len=%d）", e.conn.LocalAddr().(*net.UDPAddr).Port, tag, err, len(s.key))
				}
				s.close(false) // GCM 认证失败：密钥不符/被篡改
				continue
			}
		}
		s.handleFrame(kind, payload)
	}
}

// ---- Stream ----

// Stream 一条可靠有序流，实现 net.Conn。
type Stream struct {
	ep     *Endpoint
	remote *net.UDPAddr
	tag    uint32
	key    []byte
	aead   cipher.AEAD

	mu       sync.Mutex
	nextSeq  uint32
	inflight map[uint32]*packet
	rto      time.Duration
	srtt     float64
	rttvar   float64
	wantSeq  uint32
	reorder  map[uint32][]byte
	ackPend  bool

	mu2      sync.Mutex
	lastSeen time.Time

	deliver chan []byte
	dead    chan struct{}
	deadOne sync.Once
	finSent bool
}

type packet struct {
	data []byte
	sent time.Time
}

func newStream(ep *Endpoint, remote *net.UDPAddr, tag uint32, key []byte) *Stream {
	s := &Stream{
		ep:       ep,
		remote:   remote,
		tag:      tag,
		key:      key,
		inflight: make(map[uint32]*packet),
		reorder:  make(map[uint32][]byte),
		deliver:  make(chan []byte, 256),
		dead:     make(chan struct{}),
		rto:      rtoMin,
		lastSeen: time.Now(),
	}
	if len(key) == 32 {
		if block, err := aes.NewCipher(key); err == nil {
			if a, err2 := cipher.NewGCM(block); err2 == nil {
				s.aead = a
			}
		}
	}
	return s
}

func (s *Stream) startKeepalive() {
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-s.dead:
				return
			case <-t.C:
				s.mu2.Lock()
				stale := time.Since(s.lastSeen) > sessionExpiry
				s.mu2.Unlock()
				if stale {
					s.close(false)
					return
				}
				var b [8]byte
				binary.BigEndian.PutUint64(b[:], uint64(time.Now().UnixMilli()))
				s.sendFrame(innerPING, b[:])
			}
		}
	}()
}

// sendFrame 组帧发送（tag+route 明文，加密模式 body 整体进密文）。
func (s *Stream) sendFrame(kind byte, body []byte) {
	if s.aead != nil {
		inner := make([]byte, 1+len(body))
		inner[0] = kind
		copy(inner[1:], body)
		nonce := make([]byte, 12)
		if _, err := crand.Read(nonce); err != nil {
			return
		}
		out := make([]byte, 4, 4+1+12+len(inner)+16)
		binary.BigEndian.PutUint32(out[0:4], s.tag)
		out = append(out, routeENCRYPTED)
		out = append(out, nonce...)
		out = s.aead.Seal(out, nonce, inner, nil)
		n, werr := s.ep.conn.WriteToUDP(out, s.remote)
		if rudpDebug {
			log.Printf("[rudp@%d] 发送 kind=%d len=%d → %s (n=%d err=%v) head=%x", s.ep.conn.LocalAddr().(*net.UDPAddr).Port, inner[0], len(out), s.remote, n, werr, out[5:21])
		}
		return
	}
	out := make([]byte, 5, 5+len(body))
	binary.BigEndian.PutUint32(out[0:4], s.tag)
	out[4] = kind
	out = append(out, body...)
	_, _ = s.ep.conn.WriteToUDP(out, s.remote)
}

func (s *Stream) decrypt(body []byte) (kind byte, payload []byte, err error) {
	if len(body) < 12+1+16 { // nonce12 + GCM 最小帧（kind1+tag16；KEEP/FIN 无载荷）
		return 0, nil, ErrDecrypt
	}
	plain, err := s.aead.Open(nil, body[:12], body[12:], nil)
	if err != nil {
		return 0, nil, ErrDecrypt
	}
	return plain[0], plain[1:], nil
}

// handleFrame 处理解密后的内层帧。
func (s *Stream) handleFrame(kind byte, body []byte) {
	s.mu2.Lock()
	s.lastSeen = time.Now()
	s.mu2.Unlock()

	switch kind {
	case innerDATA:
		if len(body) < 4 {
			return
		}
		seq := binary.BigEndian.Uint32(body[:4])
		// 读循环内绝不同步发网络包（可能阻塞）——ACK 异步化
		defer func() { go s.sendAck() }()
		s.mu.Lock()
		switch {
		case seqSeqLt(seq, s.wantSeq):
			s.ackPend = true // 重传的旧包：立即补 ACK
			s.mu.Unlock()
		case seq == s.wantSeq:
			s.wantSeq++
			s.mu.Unlock()
			s.deliverChunk(body[4:])
			s.flushReorder()
		default:
			if _, dup := s.reorder[seq]; !dup && len(s.reorder) < reorderMax {
				s.reorder[seq] = append([]byte(nil), body[4:]...)
			}
			s.ackPend = true
			s.mu.Unlock()
		}

	case innerACK:
		if len(body) >= 12 {
			cum := binary.BigEndian.Uint32(body[:4])
			bitmap := binary.BigEndian.Uint64(body[4:12])
			s.onAck(cum, bitmap)
		}

	case innerPING:
		if len(body) >= 8 {
			ts := binary.BigEndian.Uint64(body[:8])
			echo := make([]byte, 8)
			binary.BigEndian.PutUint64(echo, ts)
			go s.sendFrame(innerPONG, echo)
		}

	case innerPONG:
		if len(body) >= 8 {
			ts := int64(binary.BigEndian.Uint64(body[:8]))
			now := time.Now().UnixMilli()
			if now > ts {
				s.mu.Lock()
				updateRTT(s, time.Duration(now-ts)*time.Millisecond)
				s.mu.Unlock()
			}
		}

	case innerFIN:
		s.close(false)

	case innerKEEP:
		// 保活帧：lastSeen 已刷新
	}
}

func (s *Stream) deliverChunk(b []byte) {
	if len(b) == 0 {
		return
	}
	b = append([]byte(nil), b...) // 关键：body 是共享读缓冲切片，必须拷贝
	select {
	case s.deliver <- b:
	default:
		// 读端慢：丢最旧保内存（MC 场景读端始终活跃）
		select {
		case <-s.deliver:
		default:
		}
		select {
		case s.deliver <- b:
		default:
		}
	}
}

func (s *Stream) flushReorder() {
	for {
		s.mu.Lock()
		b, ok := s.reorder[s.wantSeq]
		if !ok {
			s.mu.Unlock()
			return
		}
		delete(s.reorder, s.wantSeq)
		s.wantSeq++
		s.mu.Unlock()
		s.deliverChunk(b)
	}
}

func (s *Stream) sendAck() {
	s.mu.Lock()
	cum := s.wantSeq
	var bitmap uint64
	for id := range s.reorder {
		if id >= cum && id < cum+64 {
			bitmap |= 1 << uint(id-cum)
		}
	}
	s.mu.Unlock()
	body := make([]byte, 12)
	binary.BigEndian.PutUint32(body[0:4], cum)
	binary.BigEndian.PutUint64(body[4:12], bitmap)
	s.sendFrame(innerACK, body)
}

func (s *Stream) onAck(cum uint32, bitmap uint64) {
	s.mu.Lock()
	for id := range s.inflight {
		if seqSeqLt(id, cum) {
			delete(s.inflight, id)
		}
	}
	for i := 0; i < 64; i++ {
		if bitmap&(1<<uint(i)) != 0 {
			id := cum + uint32(i)
			if p, ok := s.inflight[id]; ok {
				rtt := time.Since(p.sent)
				delete(s.inflight, id)
				updateRTT(s, rtt)
			}
		}
	}
	s.mu.Unlock()
}

// updateRTT RFC 6298（调用方已持 s.mu）。
func updateRTT(s *Stream, rtt time.Duration) {
	sample := rtt.Seconds()
	if s.srtt == 0 {
		s.srtt = sample
		s.rttvar = sample / 2
	} else {
		s.rttvar = 0.75*s.rttvar + 0.25*abs(s.srtt-sample)
		s.srtt = 0.875*s.srtt + 0.125*sample
	}
	rto := time.Duration((s.srtt + 4*s.rttvar) * float64(time.Second))
	if rto < rtoMin {
		rto = rtoMin
	}
	if rto > rtoMax {
		rto = rtoMax
	}
	s.rto = rto
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// retransmitLoop 重传扫描 + 会话超时。
func (s *Stream) retransmitLoop() {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.dead:
			return
		case <-t.C:
			now := time.Now()
			var resends [][]byte
			s.mu.Lock()
			for _, p := range s.inflight {
				if now.Sub(p.sent) >= s.rto {
					p.sent = now
					resends = append(resends, p.data)
				}
			}
			s.mu.Unlock()
			// 锁外发送：WriteToUDP 可能阻塞（Windows 偶发），持锁发网络包
			// 会卡死整个读循环（实测教训，ROADMAP #53 同源的工程纪律）
			for _, data := range resends {
				s.sendFrame(innerDATA, data) // seq 已含在 data 头部
			}
			s.mu2.Lock()
			stale := time.Since(s.lastSeen) > sessionExpiry
			s.mu2.Unlock()
			if stale {
				s.close(false)
				return
			}
		}
	}
}

// Write 分片发送，阻塞至全部入窗（可靠重传由 retransmitLoop 负责）。
func (s *Stream) Write(b []byte) (int, error) {
	if rudpDebug {
		log.Printf("[rudp@%d] Write %d 字节 aead=%v dead=%v", s.ep.conn.LocalAddr().(*net.UDPAddr).Port, len(b), s.aead != nil, s.isDead())
	}
	total := len(b)
	off := 0
	for {
		if s.isDead() {
			return off, ErrClosed
		}
		if off >= total {
			return total, nil
		}
		end := off + ChunkSize
		if end > total {
			end = total
		}
		chunk := b[off:end]
		var seq uint32
		s.mu.Lock()
		if len(s.inflight) >= windowSize {
			s.mu.Unlock()
			select {
			case <-s.dead:
				return off, ErrClosed
			case <-time.After(10 * time.Millisecond):
			}
			continue
		}
		seq = s.nextSeq
		s.nextSeq++
		// data 必须含 seq 头：重传直接重发整帧（曾因存裸 chunk 导致
		// 重传帧被对端误读 seq 而全部丢弃——加密回环实测抓到）
		s.inflight[seq] = &packet{data: seqPayload(seq, chunk), sent: time.Now()}
		s.mu.Unlock()
		s.sendFrame(innerDATA, s.inflight[seq].data)
		off = end
	}
}

func seqPayload(seq uint32, chunk []byte) []byte {
	out := make([]byte, 4+len(chunk))
	binary.BigEndian.PutUint32(out[0:4], seq)
	copy(out[4:], chunk)
	return out
}

// Read 依序读取。
func (s *Stream) Read(b []byte) (int, error) {
	select {
	case data := <-s.deliver:
		return copy(b, data), nil
	case <-s.dead:
		select {
		case data := <-s.deliver:
			return copy(b, data), nil
		default:
			return 0, io.EOF
		}
	}
}

// Close 发 FIN 并关流。
func (s *Stream) Close() error {
	s.deadOne.Do(func() {
		if !s.finSent {
			s.finSent = true
			s.sendFrame(innerFIN, nil)
		}
		close(s.dead)
	})
	s.ep.mu.Lock()
	delete(s.ep.sessions, s.tag)
	s.ep.mu.Unlock()
	return nil
}

func (s *Stream) close(sendFin bool) {
	if sendFin {
		s.Close()
		return
	}
	s.deadOne.Do(func() {
		if rudpDebug {
			log.Printf("[rudp] stream %x closed by: %s", s.tag, debugStack())
		}
		close(s.dead)
	})
	s.ep.mu.Lock()
	delete(s.ep.sessions, s.tag)
	s.ep.mu.Unlock()
}

var rudpDebug = false

// RUDPDebugForTest 测试期打开关流日志。
func RUDPDebugForTest() { rudpDebug = true }

func debugStack() string {
	buf := make([]byte, 4096)
	n := runtime.Stack(buf, false)
	return string(buf[:n])
}

func (s *Stream) isDead() bool {
	select {
	case <-s.dead:
		return true
	default:
		return false
	}
}

// Dead 流死亡信号（对端 FIN / 超时 / 解密失败）。
func (s *Stream) Dead() <-chan struct{} { return s.dead }

// RTT 最近平滑 RTT（诊断面板 #62 输入）。
func (s *Stream) RTT() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Duration(s.srtt * float64(time.Second))
}

// net.Conn 接口余项。
func (s *Stream) LocalAddr() net.Addr  { return s.ep.conn.LocalAddr() }
func (s *Stream) RemoteAddr() net.Addr { return s.remote }
func (s *Stream) SetDeadline(t time.Time) error {
	s.SetReadDeadline(t)
	return s.SetWriteDeadline(t)
}
func (s *Stream) SetWriteDeadline(t time.Time) error { return nil } // 写有窗口阻塞，可接受

func (s *Stream) SetReadDeadline(t time.Time) error {
	if t.IsZero() {
		return nil
	}
	d := time.Until(t)
	if d <= 0 {
		s.close(false)
		return nil
	}
	go func() {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-timer.C:
			s.close(false)
		case <-s.dead:
		}
	}()
	return nil
}

// seqSeqLt 环形序号比较 a < b。
func seqSeqLt(a, b uint32) bool {
	const half = uint32(1) << 31
	return a != b && uint32(b-a) < half
}

// startLoops 会话生命周期循环：保活 + 重传。
func startLoops(s *Stream) {
	s.startKeepalive()
	go s.retransmitLoop()
}
