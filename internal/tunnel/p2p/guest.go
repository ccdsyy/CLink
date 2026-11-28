package p2p

import (
	"encoding/json"
	"errors"

	"net"
	"sync"
	"time"

	"github.com/pion/webrtc/v3"
)

var errSDPEmpty = &sdpErr{}

type sdpErr struct{}

func (*sdpErr) Error() string { return "SDP 为空（ICE 收集失败）" }

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// Guest 客机侧 WebRTC 端：接收 Offer → 回 Answer；
// 本地每接受一条 TCP 连接就开一条 DataChannel（经 ctrl 通知房主配对）。
type Guest struct {
	pc     *webrtc.PeerConnection
	cfg    Config
	ctrl   *ctrlChan
	nextID uint16
	ackW   map[uint16]chan struct{} // open 已发、等待 ack 的流 ID（ROADMAP #56 Open()）
	mu     sync.Mutex

	estCh    chan struct{}
	estOnce  sync.Once
	deadCh   chan struct{}
	deadOne  sync.Once
	lastPong int64 // unix millis，原子读（ctrl 回调单线程写入）
	lastRTT  int64 // 最近一次心跳往返延迟（毫秒）

	// #17 自适应保活 / #62 丢包统计（与 mu 共用一把锁：实测双锁模式在此
	// 环境出现过无法解释的互斥卡死，单锁最稳）
	keepInterval int // 当前心跳间隔（毫秒），15s 起、学习 NAT 超时后收敛
	learnedKA    int // 学到的稳定间隔（毫秒，0=未学到）
	pingTotal    int // 累计 ping 数
	pongMiss     int // 连续未收到 pong 的 ping 数
	lossPct      int // 最近 20 次心跳的丢包率（0-100）
}

// NewGuest 创建客机侧端点。
func NewGuest(cfg Config) (*Guest, error) {
	pc, err := newPeerConnection(cfg)
	if err != nil {
		return nil, err
	}
	g := &Guest{
		pc:           pc,
		cfg:          cfg,
		ctrl:         &ctrlChan{},
		nextID:       firstDataID,
		ackW:         map[uint16]chan struct{}{},
		estCh:        make(chan struct{}),
		deadCh:       make(chan struct{}),
		keepInterval: 15000, // #17：从 15s 起步逐步试探 NAT 超时
	}

	// ctrl 通道：协商式 ID=1，客机主动创建同 ID 通道（协商式通道不走 OnDataChannel）
	// 必须在 Answer 前创建，保证 Answer SDP 包含 datachannel m-line
	id := ctrlChannelID
	ordered, negotiated := true, true
	dc, err := pc.CreateDataChannel("clink-ctrl", &webrtc.DataChannelInit{
		ID: &id, Ordered: &ordered, Negotiated: &negotiated,
	})
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	g.ctrl.dc = dc
	dc.OnOpen(func() {
		g.markEstablished()
		g.startHeartbeat()
	})
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		var c ctrlMsg
		if err := jsonUnmarshal(m.Data, &c); err != nil {
			return
		}
		switch c.T {
		case "ping":
			g.ctrl.send(ctrlMsg{T: "pong", TS: c.TS})
		case "pong":
			now := time.Now().UnixMilli()
			g.mu.Lock()
			g.lastPong = now
			g.pongMiss = 0              // #62：pong 到达清零缺失计数
			if c.TS > 0 && now > c.TS { // 回显时间戳 → 往返延迟
				g.lastRTT = now - c.TS
			}
			g.mu.Unlock()
		case "ack":
			g.mu.Lock()
			if w, ok := g.ackW[c.ID]; ok {
				delete(g.ackW, c.ID)
				close(w)
			}
			g.mu.Unlock()
		}
	})

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateClosed {
			g.markDead()
		}
	})
	return g, nil
}

// AcceptOffer 接收房主 Offer。
func (g *Guest) AcceptOffer(sdpType, sdp string) error {
	t, err := sdpTypeFrom(sdpType)
	if err != nil {
		return err
	}
	return g.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: t, SDP: sdp,
	})
}

// Answer 生成完整 Answer SDP（等待 ICE 收集完成）。
func (g *Guest) Answer() (sdpType, sdp string, err error) {
	ans, err := g.pc.CreateAnswer(nil)
	if err != nil {
		return "", "", err
	}
	if err = g.pc.SetLocalDescription(ans); err != nil {
		return "", "", err
	}
	select {
	case <-webrtc.GatheringCompletePromise(g.pc):
	case <-time.After(10 * time.Second):
	}
	ld := g.pc.LocalDescription()
	if ld == nil {
		return "", "", errSDPEmpty
	}
	return ld.Type.String(), ld.SDP, nil
}

// Established ctrl 通道就绪（隧道已打通）。
func (g *Guest) Established() <-chan struct{} { return g.estCh }

// Dead 断线信号（PC Failed / 心跳超时）。
func (g *Guest) Dead() <-chan struct{} { return g.deadCh }

// RTT 最近一次心跳往返延迟（毫秒；未测量过返回 0）。
func (g *Guest) RTT() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lastRTT
}

// Open 为一条本地 MC 连接开通对端 DataChannel 并返回桥接连
// （ROADMAP #56 路径抽象：session 调度器统一 accept 后调 Open）。
// 流程：分配流 ID → 发 open → 等房主 ack → 创建同 ID 通道 → 等 open →
// 返回 dcConn。ack 前不放行数据（协商式通道对端未就绪时数据会被误当
// DCEP 解析——回环实测教训，见 docs/PROTOCOL.md 附录 B）。
func (g *Guest) Open() (net.Conn, error) {
	g.mu.Lock()
	id := g.nextID
	if g.nextID < 65500 {
		g.nextID++
	}
	w := make(chan struct{}, 1)
	g.ackW[id] = w
	g.mu.Unlock()

	g.ctrl.send(ctrlMsg{T: "open", ID: id})

	select {
	case <-w:
	case <-time.After(10 * time.Second):
		g.mu.Lock()
		delete(g.ackW, id)
		g.mu.Unlock()
		return nil, errors.New("open ack 超时")
	case <-g.deadCh:
		g.mu.Lock()
		delete(g.ackW, id)
		g.mu.Unlock()
		return nil, errors.New("连接已断开")
	}

	n := id
	ordered, negotiated := true, true
	dc, err := g.pc.CreateDataChannel("clink", &webrtc.DataChannelInit{
		ID: &n, Ordered: &ordered, Negotiated: &negotiated,
	})
	if err != nil {
		return nil, err
	}
	// net.Pipe 一端返回给调用方，另一端由 bridgeDC 与 DC 桥接——
	// 数据路径与原版 ackChannel/bridgeDC 完全同构（回环实测过的机制）
	pipeIn, pipeOut := net.Pipe()
	dc.OnOpen(func() { go bridgeDC(dc, pipeIn) })
	dc.OnClose(func() { _ = pipeIn.Close() })
	select {
	case <-g.pcOpenWait(dc):
	case <-time.After(10 * time.Second):
		_ = dc.Close()
		_ = pipeIn.Close()
		return nil, errors.New("datachannel open 超时")
	case <-g.deadCh:
		_ = dc.Close()
		_ = pipeIn.Close()
		return nil, errors.New("连接已断开")
	}
	return pipeOut, nil
}

// pcOpenWait 等待 DC 打开（轮询 ReadyState，避免依赖回调时序）。
func (g *Guest) pcOpenWait(dc *webrtc.DataChannel) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ { // 最多 10s（50ms × 200）
			if dc.ReadyState() == webrtc.DataChannelStateOpen {
				close(ch)
				return
			}
			select {
			case <-g.deadCh:
				close(ch)
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
		close(ch)
	}()
	return ch
}

// startHeartbeat 自适应心跳（ROADMAP #17）：
//
//	间隔从 15s 起步；pong 正常则 +2s 逐步拉长试探 NAT UDP 表项超时；
//	一旦 pong 缺失，退回「上次稳定值 × 0.8」收敛（学到该网络的保活周期）。
//	缺失超过 pongDeadline 判死（断线感知不变）；统计丢包率（#62）。
//	全部状态在 g.mu 单锁下（双锁模式实测有诡异卡死，勿拆）。
func (g *Guest) startHeartbeat() {
	g.mu.Lock()
	g.lastPong = time.Now().UnixMilli()
	g.mu.Unlock()
	go func() {
		for {
			g.mu.Lock()
			iv := time.Duration(g.keepInterval) * time.Millisecond
			g.mu.Unlock()
			select {
			case <-g.deadCh:
				return
			case <-time.After(iv):
			}

			g.mu.Lock()
			last := g.lastPong
			g.pingTotal++
			pongOK := time.Since(time.UnixMilli(last)) < iv+2*time.Second
			if !pongOK && g.pongMiss < 20 {
				g.pongMiss++
			}
			win := 20
			if g.pingTotal < win {
				win = g.pingTotal
			}
			m := g.pongMiss
			if m > win {
				m = win
			}
			g.lossPct = m * 100 / win
			g.mu.Unlock()

			g.ctrl.send(ctrlMsg{T: "ping", TS: time.Now().UnixMilli()})

			g.mu.Lock()
			if time.Since(time.UnixMilli(last)) > pongDeadline {
				g.markDead()
				g.mu.Unlock()
				return
			}
			if pongOK {
				if g.keepInterval < 30000 {
					g.keepInterval += 2000
				}
				g.learnedKA = g.keepInterval
			} else {
				base := g.learnedKA
				if base <= 0 {
					base = 15000
				}
				g.keepInterval = base * 8 / 10
				if g.keepInterval < 10000 {
					g.keepInterval = 10000
				}
			}
			g.mu.Unlock()
		}
	}()
}

// Diagnostics 诊断数据（#62）：丢包率 / 当前心跳间隔 / 学到的稳定间隔。
func (g *Guest) Diagnostics() (lossPct, keepIntervalMs, learnedMs int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lossPct, g.keepInterval, g.learnedKA
}

func (g *Guest) markEstablished() {
	g.estOnce.Do(func() { close(g.estCh) })
}

func (g *Guest) markDead() {
	g.deadOne.Do(func() { close(g.deadCh) })
}

// Stop 关闭端点。
func (g *Guest) Stop() {
	g.markDead()
	// 监听器由 session 调度器持有并关闭（#56 路径抽象后 p2p 不再自建监听）
	_ = g.pc.Close()
}
