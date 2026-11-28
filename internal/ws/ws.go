// Package ws 最小 WebSocket 实现（RFC 6455，ROADMAP #36）。
//
// 仅支持本项目的需要：二进制帧、无子协议、无分片发送（接收端容忍分片）。
// 客户端帧掩码、服务端帧不掩码；Ping/Pong/Close 自动处理。
// 用途：中继通道走 wss/ws（蹭 CDN 边缘当免费数据面，UDP QoS 杀不死），
// 可叠加 HTTP CONNECT 代理出站（#37）。
package ws

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

var (
	ErrClosed    = errors.New("ws: 连接已关闭")
	ErrBadFrame  = errors.New("ws: 非法帧")
	ErrHandshake = errors.New("ws: 握手失败")
)

// Conn WebSocket 连接（二进制消息语义）。
type Conn struct {
	raw   net.Conn
	br    *bufio.Reader
	mask  bool // 客户端侧：发出的帧必须掩码
	mu    sync.Mutex
	closed bool
}

// Dial 客户端拨号（URL 形如 ws://host:port/path）。
// dialer 为 nil 时直连；可传入经 HTTP CONNECT 的拨号函数（#37）。
func Dial(rawURL string, dialer func(network, addr string) (net.Conn, error)) (*Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if _, _, e := net.SplitHostPort(host); e != nil {
		if u.Scheme == "wss" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	d := dialer
	if d == nil {
		d = net.Dial
	}
	timeoutD := func(network, addr string) (net.Conn, error) {
		c, e := d(network, addr)
		if e == nil {
			_ = c.SetDeadline(time.Now().Add(8 * time.Second))
		}
		return c, e
	}
	var raw net.Conn
	if u.Scheme == "wss" {
		// wss：CONNECT/TLS 由 dialer 负责（本实现仅 ws 明文 + 自定义 TLS dialer）
		raw, err = timeoutD("tcp", host)
		if err != nil {
			return nil, err
		}
	} else {
		raw, err = timeoutD("tcp", host)
		if err != nil {
			return nil, err
		}
	}

	// Upgrade 握手
	keyBytes := make([]byte, 16)
	_, _ = rand.Read(keyBytes)
	wsKey := base64.StdEncoding.EncodeToString(keyBytes)
	path := u.Path
	if path == "" {
		path = "/"
	}
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + wsKey + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := raw.Write([]byte(req)); err != nil {
		raw.Close()
		return nil, err
	}
	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		raw.Close()
		return nil, err
	}
	defer resp.Body.Close()
	accept := computeAccept(wsKey)
	if resp.StatusCode != 101 || resp.Header.Get("Sec-WebSocket-Accept") != accept {
		raw.Close()
		return nil, ErrHandshake
	}
	_ = raw.SetDeadline(time.Time{})
	return &Conn{raw: raw, br: br, mask: true}, nil
}

func computeAccept(key string) string {
	h := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

// Serve 在 HTTP 处理器上升级并返回 Conn（服务端，要求 handler 已校验路径）。
func Serve(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "expected websocket", http.StatusBadRequest)
		return nil, ErrHandshake
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, ErrHandshake
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("ws: 不支持 hijack")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	accept := computeAccept(key)
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := conn.Write([]byte(resp)); err != nil {
		conn.Close()
		return nil, err
	}
	_ = rw.Flush()
	return &Conn{raw: conn, br: rw.Reader, mask: false}, nil
}

// ReadMessage 读一条二进制消息（自动拼分片、应答 Ping）。
func (c *Conn) ReadMessage() ([]byte, error) {
	var assembled []byte
	for {
		h1, err := c.br.ReadByte()
		if err != nil {
			return nil, err
		}
		h2, err := c.br.ReadByte()
		if err != nil {
			return nil, err
		}
		fin := h1&0x80 != 0
		opcode := h1 & 0x0F
		masked := h2&0x80 != 0
		length := int(h2 & 0x7F)
		switch length {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(c.br, ext[:]); err != nil {
				return nil, err
			}
			length = int(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(c.br, ext[:]); err != nil {
				return nil, err
			}
			length = int(binary.BigEndian.Uint64(ext[:]))
		}
		if length > 4<<20 {
			return nil, ErrBadFrame // 4MB 上限
		}
		var maskKey [4]byte
		if masked {
			if _, err := io.ReadFull(c.br, maskKey[:]); err != nil {
				return nil, err
			}
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= maskKey[i%4]
			}
		}
		switch opcode {
		case 0x2, 0x0: // 二进制 / 延续
			assembled = append(assembled, payload...)
			if fin {
				return assembled, nil
			}
		case 0x9: // ping → pong
			_ = c.writeFrame(0xA, payload)
		case 0xA: // pong
		case 0x8: // close
			_ = c.writeFrame(0x8, nil)
			return nil, ErrClosed
		default:
			return nil, ErrBadFrame
		}
	}
}

// WriteMessage 发一条二进制消息（客户端侧自动掩码）。
func (c *Conn) WriteMessage(b []byte) error {
	return c.writeFrame(0x2, b)
}

func (c *Conn) writeFrame(opcode byte, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	header := make([]byte, 0, 14)
	header = append(header, 0x80|opcode) // FIN + opcode
	length := len(payload)
	maskBit := byte(0)
	if c.mask {
		maskBit = 0x80
	}
	switch {
	case length < 126:
		header = append(header, maskBit|byte(length))
	case length < 65536:
		header = append(header, maskBit|126)
		var ext [2]byte
		binary.BigEndian.PutUint16(ext[:], uint16(length))
		header = append(header, ext[:]...)
	default:
		header = append(header, maskBit|127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(length))
		header = append(header, ext[:]...)
	}
	if c.mask {
		var key [4]byte
		_, _ = rand.Read(key[:])
		header = append(header, key[:]...)
		masked := make([]byte, len(payload))
		for i := range payload {
			masked[i] = payload[i] ^ key[i%4]
		}
		payload = masked
	}
	if _, err := c.raw.Write(append(header, payload...)); err != nil {
		return err
	}
	return nil
}

// Close 关闭连接（发 close 帧 + 关底层）。
func (c *Conn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	_ = c.writeFrame(0x8, nil)
	return c.raw.Close()
}

// ReadDeadline 尽力支持（读超时 → 下一次 Read 返回错误）。
func (c *Conn) SetReadDeadline(t time.Time) error { return c.raw.SetReadDeadline(t) }
