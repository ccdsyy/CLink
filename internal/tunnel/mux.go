// 统一 Muxer：在一条可靠有序流上承载多条独立 MC 连接。
// （ROADMAP #52 统一 socket 多路复用的内层实现——中继路径单流承载全部连接）
//
// 帧格式：[2B connID][2B len][payload(len)]
//
//	len == 0xFFFF → 该 connID 关闭
package tunnel

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

const muxFinLen = 0xFFFF

// MuxConn 单通道的 net.Conn 语义（Read 只收本通道，Write 自动加帧）。
type MuxConn struct {
	id      uint16
	rd      chan []byte
	w       func(id uint16, payload []byte, fin bool) error
	once    sync.Once
	dead    chan struct{}
	writeMu sync.Mutex
}

func (c *MuxConn) Read(b []byte) (int, error) {
	select {
	case data := <-c.rd:
		n := copy(b, data)
		return n, nil
	case <-c.dead:
		return 0, io.EOF
	}
}

func (c *MuxConn) Write(b []byte) (int, error) {
	select {
	case <-c.dead:
		return 0, errors.New("mux: 通道已关闭")
	default:
	}
	c.writeMu.Lock()
	err := c.w(c.id, b, false)
	c.writeMu.Unlock()
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *MuxConn) Close() error {
	_ = c.w(c.id, nil, true) // 发关闭帧
	c.closeLocal()
	return nil
}

func (c *MuxConn) closeLocal() {
	c.once.Do(func() { close(c.rd) })
}

func (c *MuxConn) LocalAddr() net.Addr                { return nilLocal{} }
func (c *MuxConn) RemoteAddr() net.Addr               { return nilLocal{} }
func (c *MuxConn) SetDeadline(t time.Time) error      { return nil }
func (c *MuxConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *MuxConn) SetWriteDeadline(t time.Time) error { return nil }

type nilLocal struct{}

func (nilLocal) Network() string { return "mux" }
func (nilLocal) String() string  { return "mux-channel" }

// Muxer 单流多路复用器。
type Muxer struct {
	c net.Conn

	mu       sync.Mutex
	conns    map[uint16]*MuxConn
	acceptCh chan *MuxConn // 新通道（服务端模式：未知 ID 自动登记）

	dead chan struct{}
	once sync.Once
}

// NewMuxer 客户端模式：用 Channel(id) 打开已知 ID 通道。
func NewMuxer(c net.Conn) *Muxer {
	m := &Muxer{c: c, conns: make(map[uint16]*MuxConn), dead: make(chan struct{})}
	go m.readLoop()
	return m
}

// NewMuxerServer 服务端模式：未知 ID 的首帧自动进入 Accept 队列。
func NewMuxerServer(c net.Conn) *Muxer {
	m := NewMuxer(c)
	m.acceptCh = make(chan *MuxConn, 32)
	return m
}

// Channel 返回（或创建）指定 ID 的通道。
func (m *Muxer) Channel(id uint16) *MuxConn {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.conns[id]; ok {
		return c
	}
	c := &MuxConn{id: id, rd: make(chan []byte, 128), dead: make(chan struct{}), w: m.writeFrame}
	m.conns[id] = c
	return c
}

// Accept 等待新通道（服务端模式）。
func (m *Muxer) Accept() (*MuxConn, error) {
	select {
	case c := <-m.acceptCh:
		return c, nil
	case <-m.dead:
		return nil, io.EOF
	}
}

// writeFrame 加帧写出。
func (m *Muxer) writeFrame(id uint16, payload []byte, fin bool) error {
	head := make([]byte, 4)
	binary.BigEndian.PutUint16(head[0:2], id)
	if fin {
		binary.BigEndian.PutUint16(head[2:4], muxFinLen)
		_, err := m.c.Write(head)
		return err
	}
	binary.BigEndian.PutUint16(head[2:4], uint16(len(payload)))
	if _, err := m.c.Write(head); err != nil {
		return err
	}
	_, err := m.c.Write(payload)
	return err
}

// readLoop bufio 帧解析并路由。
func (m *Muxer) readLoop() {
	r := bufio.NewReaderSize(m.c, 16*1024)
	header := make([]byte, 4)
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			m.Close()
			return
		}
		id := binary.BigEndian.Uint16(header[0:2])
		length := int(binary.BigEndian.Uint16(header[2:4]))
		if length == muxFinLen {
			m.closeLocal(id)
			continue
		}
		if length == 0 {
			continue
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(r, payload); err != nil {
			m.Close()
			return
		}
		c := m.channel(id)
		select {
		case c.rd <- payload:
		default: // 背压：丢最旧
			select {
			case <-c.rd:
			default:
			}
			select {
			case c.rd <- payload:
			default:
			}
		}
	}
}

// channel 内部建通道（不触发 Accept 的客户端模式直接建）。
func (m *Muxer) channel(id uint16) *MuxConn {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.conns[id]; ok {
		return c
	}
	c := &MuxConn{id: id, rd: make(chan []byte, 128), dead: make(chan struct{}), w: m.writeFrame}
	m.conns[id] = c
	if m.acceptCh != nil {
		select {
		case m.acceptCh <- c:
		default:
			delete(m.conns, id) // Accept 队列满：丢弃该通道
		}
	}
	return c
}

func (m *Muxer) closeLocal(id uint16) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.conns[id]; ok {
		c.closeLocal()
		delete(m.conns, id)
	}
}

// Done 复用器死亡信号。
func (m *Muxer) Done() <-chan struct{} { return m.dead }

// Close 关闭复用器（连带底层流）。
func (m *Muxer) Close() {
	m.once.Do(func() { close(m.dead) })
	_ = m.c.Close()
}
