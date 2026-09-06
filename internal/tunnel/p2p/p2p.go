// Package p2p 实现 Tier 2：WebRTC DataChannel 隧道（pion/webrtc）。
//
// 通道规划（全部 Negotiated，双方按固定 ID 创建）：
//   ID 1        clink-ctrl   控制通道：开关数据通道 + 心跳 ping/pong
//   ID 10..     clink-data   每条本地 TCP 连接（MC 客户端）一条
//
// 客机本地每接受一条 TCP 连接（MC 的 ping + 正式连接各一条），
// 经 ctrl 通道请求房主开通对应 ID 的 DataChannel，房主侧 dial MC 服务端口并桥接。
package p2p

import (
        "encoding/json"
        "fmt"
        "net"
        "strconv"
        "strings"
        "sync"
        "time"

        "github.com/pion/webrtc/v3"
)

const (
        ctrlChannelID = uint16(1)
        firstDataID   = uint16(10)
        chunkSize     = 16384 // DataChannel 单消息安全分片
        pingEvery     = 20 * time.Second
        pongDeadline  = 45 * time.Second // 连续 2 个周期无 pong 判定断线
)

// Config P2P 配置。
type Config struct {
        STUN   []string
        TURN   []string // 形如 turn:host:port 或 user:pass@turn:host:port
        MCPort int      // 仅房主使用
}

// ctrlMsg 控制通道消息（JSON）。
type ctrlMsg struct {
        T  string `json:"t"`            // open | close | ping | pong
        ID uint16 `json:"id,omitempty"` // open/close 的通道 ID
        TS int64  `json:"ts,omitempty"` // ping/pong 时间戳
}

func newPeerConnection(cfg Config) (*webrtc.PeerConnection, error) {
        servers := []webrtc.ICEServer{{URLs: cfg.STUN}}
        for _, t := range cfg.TURN {
                t = strings.TrimSpace(t)
                if t == "" {
                        continue
                }
                srv := webrtc.ICEServer{URLs: []string{t}}
                if at := strings.LastIndex(t, "@"); at > 0 { // user:pass@turn:host:port
                        cred := t[:at]
                        srv.URLs = []string{t[at+1:]}
                        if colon := strings.Index(cred, ":"); colon > 0 {
                                srv.Username, srv.Credential = cred[:colon], cred[colon+1:]
                        }
                }
                servers = append(servers, srv)
        }
        return webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: servers})
}

// ctrlChan 控制通道收发器（线程安全）。
type ctrlChan struct {
        mu sync.Mutex
        dc *webrtc.DataChannel
}

func (c *ctrlChan) send(m ctrlMsg) {
        c.mu.Lock()
        defer c.mu.Unlock()
        if c.dc == nil {
                return
        }
        b, err := json.Marshal(m)
        if err != nil {
                return
        }
        _ = c.dc.Send(b)
}

// bridgeDC 将一条 DataChannel 与一条本地 TCP 连接双向接驳。
// 任一方向结束即关闭双方；10 秒内未 open 的通道自动回收。
func bridgeDC(dc *webrtc.DataChannel, conn net.Conn) {
        var once sync.Once
        closed := make(chan struct{})
        finish := func() {
                once.Do(func() {
                        close(closed)
                        _ = dc.Close()
                        _ = conn.Close()
                })
        }

        msgs := make(chan []byte, 128)
        dc.OnMessage(func(m webrtc.DataChannelMessage) {
                select {
                case msgs <- m.Data:
                case <-closed:
                }
        })

        go func() { // TCP → DC
                defer finish()
                buf := make([]byte, chunkSize)
                for {
                        n, err := conn.Read(buf)
                        if n > 0 {
                                if e := dc.Send(buf[:n]); e != nil {
                                        return
                                }
                        }
                        if err != nil {
                                return
                        }
                }
        }()

        go func() { // DC → TCP
                defer finish()
                for {
                        select {
                        case m := <-msgs:
                                if _, err := conn.Write(m); err != nil {
                                        return
                                }
                        case <-closed:
                                return
                        }
                }
        }()

        dc.OnClose(func() { finish() })
        time.AfterFunc(10*time.Second, func() {
                if dc.ReadyState() != webrtc.DataChannelStateOpen {
                        finish()
                }
        })
}

// dialLocalTCP 连接本机 MC 端口（房主侧）。
func dialLocalTCP(port int) (net.Conn, error) {
        d := net.Dialer{Timeout: 5 * time.Second}
        return d.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
}

// sdpTypeFrom / sdpTypeTo —— SDPType 枚举与字符串互转（跨信令传输）。
func sdpTypeFrom(s string) (webrtc.SDPType, error) {
        switch strings.ToLower(strings.TrimSpace(s)) {
        case "offer":
                return webrtc.SDPTypeOffer, nil
        case "answer":
                return webrtc.SDPTypeAnswer, nil
        default:
                return 0, fmt.Errorf("未知 SDP 类型: %q", s)
        }
}
