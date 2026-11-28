// Package mcproto Minecraft 协议感知（ROADMAP #65）：
// 解析本地连接的首包 handshake，识别 server list ping 与 login。
// 隧道未就绪时直接代答，让游戏内服务器列表显示「隧道建立中…」，
// 而不是卡黑/连接错误——体验细节与通用工具拉开差距。
package mcproto

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
)

// Handshake 解析结果。
type Handshake struct {
	Protocol  int32
	Host      string
	Port      uint16
	NextState int32 // 1=Status(server list ping) 2=Login
}

// State 分类结果。
type Decision int

const (
	Passthrough  Decision = iota // 非 handshake / 已就绪：原样转发
	AnswerStatus                 // 状态 ping 且未就绪 → 代答 MOTD
	AnswerReject                 // login 且未就绪 → 代答断开消息
)

// Gate 检查一条本地连接的首包。
// 返回 (决定, 已消费字节)。Passthrough 时 consumed 原样回灌（调用方先写回再桥接）。
func Gate(conn net.Conn, ready bool) (Decision, []byte, error) {
	// VarInt 长度前缀
	length, buf, err := readVarIntFrom(conn)
	if err != nil {
		return Passthrough, buf, err
	}
	if length <= 0 || length > 1024 {
		return Passthrough, buf, nil // 不是合理的 MC 包：放行（可能是 legacy ping）
	}
	rest := make([]byte, length)
	if _, err := io.ReadFull(conn, rest); err != nil {
		return Passthrough, buf, err
	}
	buf = append(buf, rest...)

	// packetID = 0x00 → handshake
	if len(rest) < 2 || rest[0] != 0x00 {
		return Passthrough, buf, nil
	}
	hs, err := parseHandshake(rest[1:])
	if err != nil {
		return Passthrough, buf, nil
	}
	if hs.NextState == 1 && !ready {
		return AnswerStatus, buf, nil
	}
	if hs.NextState == 2 && !ready {
		return AnswerReject, buf, nil
	}
	return Passthrough, buf, nil
}

// ReplyStatus 代答状态 ping（含后续 ping 包的 0x01 响应也一并处理：
// 直接发 status 响应后关闭，客户端会显示 MOTD）。
func ReplyStatus(conn net.Conn, motd string) {
	status := `{"version":{"name":"CLink 隧道建立中…","protocol":-1},"players":{"max":0,"online":0},"description":{"text":"` +
		motd + `"}}`
	writeRaw(conn, varIntBytes(len(status)+1))
	writeRaw(conn, []byte{0x00})
	writeRaw(conn, varIntBytes(len(status)))
	writeRaw(conn, []byte(status))
}

// ReplyReject 代答登录断开。
func ReplyReject(conn net.Conn, reason string) {
	chat := `{"text":"` + reason + `"}`
	body := append([]byte{0x00}, varIntBytes(len(chat))...)
	body = append(body, chat...)
	writePacket(conn, body)
}

// ---- VarInt / 包读写 ----

func readVarIntFrom(conn net.Conn) (int, []byte, error) {
	var num uint32
	var read int
	buf := make([]byte, 0, 5)
	for i := 0; i < 5; i++ {
		var b [1]byte
		if _, err := io.ReadFull(conn, b[:]); err != nil {
			return 0, buf, err
		}
		buf = append(buf, b[0])
		read++
		num |= uint32(b[0]&0x7F) << (7 * i)
		if b[0]&0x80 == 0 {
			return int(num), buf, nil
		}
	}
	return 0, buf, errors.New("varint 过长")
}

func parseHandshake(b []byte) (*Handshake, error) {
	hs := &Handshake{}
	proto, n := readVarInt(b)
	if n <= 0 {
		return nil, errors.New("协议版本解析失败")
	}
	hs.Protocol = proto
	b = b[n:]
	hostLen, n := readVarInt(b)
	if n <= 0 || int(hostLen) > len(b)-n {
		return nil, errors.New("主机名长度异常")
	}
	hs.Host = string(b[n : n+int(hostLen)])
	b = b[n+int(hostLen):]
	if len(b) < 3 {
		return nil, errors.New("handshake 过短")
	}
	hs.Port = binary.BigEndian.Uint16(b[:2])
	nextState, _ := readVarInt(b[2:])
	hs.NextState = nextState
	return hs, nil
}

func readVarInt(b []byte) (int32, int) {
	var num int32
	for i := 0; i < 5 && i < len(b); i++ {
		num |= int32(b[i]&0x7F) << (7 * i)
		if b[i]&0x80 == 0 {
			return num, i + 1
		}
	}
	return 0, 0
}

func varIntBytes(v int) []byte {
	var out []byte
	u := uint32(v)
	for {
		if u&^0x7F == 0 {
			out = append(out, byte(u))
			return out
		}
		out = append(out, byte(u&0x7F)|0x80)
		u >>= 7
	}
}

func writePacket(conn net.Conn, payload []byte) {
	writeRaw(conn, varIntBytes(len(payload)))
	writeRaw(conn, payload)
}

func writeRaw(conn net.Conn, b []byte) {
	_, _ = conn.Write(b)
}
