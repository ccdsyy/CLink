package p2p

import (
        "context"
        "encoding/json"
        "net"
        "strconv"
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
        ln     net.Listener
        nextID uint16
        pending map[uint16]net.Conn // open 已发、等待 ack 的本地连接
        mu     sync.Mutex

        estCh    chan struct{}
        estOnce  sync.Once
        deadCh   chan struct{}
        deadOne  sync.Once
        lastPong int64 // unix millis，原子读（ctrl 回调单线程写入）
}

// NewGuest 创建客机侧端点。
func NewGuest(cfg Config) (*Guest, error) {
        pc, err := newPeerConnection(cfg)
        if err != nil {
                return nil, err
        }
        g := &Guest{
                pc:      pc,
                cfg:     cfg,
                ctrl:    &ctrlChan{},
                nextID:  firstDataID,
                pending: map[uint16]net.Conn{},
                estCh:   make(chan struct{}),
                deadCh:  make(chan struct{}),
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
                        g.mu.Lock()
                        g.lastPong = time.Now().UnixMilli()
                        g.mu.Unlock()
                case "ack":
                        go g.ackChannel(c.ID)
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

// ServeLocal 在 127.0.0.1:localPort 监听（localPort=0 随机），供 MC 客户端连入。
func (g *Guest) ServeLocal(ctx context.Context, localPort int) error {
        ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(localPort))
        if err != nil {
                return err
        }
        g.mu.Lock()
        g.ln = ln
        g.mu.Unlock()
        go g.acceptLoop(ctx, ln)
        return nil
}

// LocalPort 实际监听端口。
func (g *Guest) LocalPort() int {
        g.mu.Lock()
        defer g.mu.Unlock()
        if g.ln == nil {
                return 0
        }
        return g.ln.Addr().(*net.TCPAddr).Port
}

func (g *Guest) acceptLoop(ctx context.Context, ln net.Listener) {
        for {
                conn, err := ln.Accept()
                if err != nil {
                        return
                }
                go g.openChannel(conn)
        }
}

// openChannel 为一条本地 TCP 连接开通 DataChannel 并桥接。
// 流程：发 open{id} → 挂起等待房主 ack{id} → 创建同 ID 通道 → OnOpen 桥接。
// ack 前不放行 TCP 数据（协商式通道对端未就绪时，数据会被误当 DCEP 解析）。
func (g *Guest) openChannel(conn net.Conn) {
        g.mu.Lock()
        id := g.nextID
        if g.nextID < 65500 {
                g.nextID++
        }
        g.pending[id] = conn
        g.mu.Unlock()

        g.ctrl.send(ctrlMsg{T: "open", ID: id})

        // 10 秒内未获 ack 则放弃该连接
        time.AfterFunc(10*time.Second, func() {
                g.mu.Lock()
                _, still := g.pending[id]
                delete(g.pending, id)
                g.mu.Unlock()
                if still {
                        _ = conn.Close()
                }
        })
}

// ackChannel 收到房主 ack 后创建同 ID 通道并桥接挂起的连接。
func (g *Guest) ackChannel(id uint16) {
        g.mu.Lock()
        conn, ok := g.pending[id]
        delete(g.pending, id)
        g.mu.Unlock()
        if !ok || conn == nil {
                return
        }
        n := id
        ordered, negotiated := true, true
        dc, err := g.pc.CreateDataChannel("clink", &webrtc.DataChannelInit{
                ID: &n, Ordered: &ordered, Negotiated: &negotiated,
        })
        if err != nil {
                _ = conn.Close()
                return
        }
        dc.OnOpen(func() { go bridgeDC(dc, conn) })
        dc.OnClose(func() { _ = conn.Close() })
        // 10 秒未配对成功（房主离线/超时）→ 放弃本连接
        time.AfterFunc(10*time.Second, func() {
                if dc.ReadyState() != webrtc.DataChannelStateOpen {
                        _ = dc.Close()
                        _ = conn.Close()
                }
        })
}

// startHeartbeat 应用层心跳：20s ping，45s 无 pong 判死（防 NAT 映射老化 + 断线感知）。
func (g *Guest) startHeartbeat() {
        g.mu.Lock()
        g.lastPong = time.Now().UnixMilli()
        g.mu.Unlock()
        go func() {
                t := time.NewTicker(pingEvery)
                defer t.Stop()
                for {
                        select {
                        case <-g.deadCh:
                                return
                        case <-t.C:
                        }
                        g.ctrl.send(ctrlMsg{T: "ping", TS: time.Now().UnixMilli()})
                        g.mu.Lock()
                        last := g.lastPong
                        g.mu.Unlock()
                        if time.Since(time.UnixMilli(last)) > pongDeadline {
                                g.markDead()
                                return
                        }
                }
        }()
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
        g.mu.Lock()
        if g.ln != nil {
                _ = g.ln.Close()
        }
        g.mu.Unlock()
        _ = g.pc.Close()
}
