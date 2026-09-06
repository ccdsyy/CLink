package p2p

import (
        "sync"
        "time"

        "github.com/pion/webrtc/v3"
)

// Host 房主侧 WebRTC 端：主动建 PC + ctrl 通道，等客机 Answer；
// 收到客机 open 请求后创建对应 DataChannel 并桥接本机 MC 端口。
type Host struct {
        pc   *webrtc.PeerConnection
        cfg  Config
        ctrl *ctrlChan

        estCh   chan struct{}
        estOnce sync.Once
        deadCh  chan struct{}
        deadOne sync.Once
}

// NewHost 创建房主侧端点（不产生网络请求，Offer 前安静）。
func NewHost(cfg Config) (*Host, error) {
        pc, err := newPeerConnection(cfg)
        if err != nil {
                return nil, err
        }
        h := &Host{
                pc:    pc,
                cfg:   cfg,
                ctrl:  &ctrlChan{},
                estCh: make(chan struct{}),
                deadCh: make(chan struct{}),
        }

        // ctrl 通道：协商式 ID=1，Offer 生成前创建
        id := ctrlChannelID
        ordered, negotiated := true, true
        dc, err := pc.CreateDataChannel("clink-ctrl", &webrtc.DataChannelInit{
                ID: &id, Ordered: &ordered, Negotiated: &negotiated,
        })
        if err != nil {
                _ = pc.Close()
                return nil, err
        }
        h.ctrl.dc = dc
        dc.OnOpen(func() { h.markEstablished() })
        dc.OnMessage(h.onCtrlMsg)

        pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
                if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateClosed {
                        h.markDead()
                }
        })
        return h, nil
}

func (h *Host) onCtrlMsg(m webrtc.DataChannelMessage) {
        var c ctrlMsg
        if err := jsonUnmarshal(m.Data, &c); err != nil {
                return
        }
        switch c.T {
        case "open":
                h.openDataChannel(c.ID)
        case "close":
                // DataChannel 关闭由 bridgeDC 兜底回收，无需额外处理
        case "ping":
                h.ctrl.send(ctrlMsg{T: "pong", TS: c.TS})
        }
}

// openDataChannel 创建协商式数据通道并回 ack；
// 客机收到 ack 后才创建同 ID 通道并放行 TCP 数据（避免对端未建通道时
// 数据被误当 DCEP 解析导致整条流被重置——本机回环测试实测复现过）。
func (h *Host) openDataChannel(id uint16) {
        if id < firstDataID {
                return
        }
        n := id
        ordered, negotiated := true, true
        dc, err := h.pc.CreateDataChannel("clink", &webrtc.DataChannelInit{
                ID: &n, Ordered: &ordered, Negotiated: &negotiated,
        })
        if err != nil {
                return
        }
        h.ctrl.send(ctrlMsg{T: "ack", ID: id})
        dc.OnOpen(func() {
                conn, err := dialLocalTCP(h.cfg.MCPort)
                if err != nil {
                        _ = dc.Close()
                        return
                }
                go bridgeDC(dc, conn)
        })
}

// Offer 生成完整 SDP（等待 ICE 收集完成，非 trickle）。
func (h *Host) Offer() (sdpType, sdp string, err error) {
        offer, err := h.pc.CreateOffer(nil)
        if err != nil {
                return "", "", err
        }
        if err = h.pc.SetLocalDescription(offer); err != nil {
                return "", "", err
        }
        select {
        case <-webrtc.GatheringCompletePromise(h.pc):
        case <-time.After(10 * time.Second): // STUN 不可达时兜底
        }
        ld := h.pc.LocalDescription()
        if ld == nil {
                return "", "", errSDPEmpty
        }
        return ld.Type.String(), ld.SDP, nil
}

// AcceptAnswer 接收客机 Answer。
func (h *Host) AcceptAnswer(sdpType, sdp string) error {
        t, err := sdpTypeFrom(sdpType)
        if err != nil {
                return err
        }
        return h.pc.SetRemoteDescription(webrtc.SessionDescription{
                Type: t, SDP: sdp,
        })
}

// Established ctrl 通道就绪信号（客机已打通）。
func (h *Host) Established() <-chan struct{} { return h.estCh }

// Dead 连接死亡信号（ICE Failed / PC Closed）。
func (h *Host) Dead() <-chan struct{} { return h.deadCh }

func (h *Host) markEstablished() {
        h.estOnce.Do(func() { close(h.estCh) })
}

func (h *Host) markDead() {
        h.deadOne.Do(func() { close(h.deadCh) })
}

// Stop 关闭端点。
func (h *Host) Stop() {
        h.markDead()
        _ = h.pc.Close()
}
