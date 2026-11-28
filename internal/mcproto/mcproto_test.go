package mcproto

import (
	"net"
	"testing"
	"time"
)

// 构造 handshake 包：packetID 0x00 + protocol + host + port + nextState
func handshakePacket(host string, port uint16, nextState byte) []byte {
	body := []byte{0x00, 0x01, byte(len(host))}
	body = append(body, host...)
	body = append(body, byte(port>>8), byte(port), nextState)
	out := append([]byte{byte(len(body))}, body...)
	return out
}

// TestGateStatusPing 隧道未就绪：server list ping → 判定代答状态。
func TestGateStatusPing(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		_, _ = client.Write(handshakePacket("localhost", 25565, 1))
		// 常驻排空（ReplyStatus 的写端在真实链路由游戏客户端读）
		for {
			buf := make([]byte, 256)
			_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := client.Read(buf); err != nil {
				return
			}
		}
	}()

	decision, consumed, err := Gate(server, false)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if decision != AnswerStatus {
		t.Fatalf("期望 AnswerStatus，实为 %d", decision)
	}
	if len(consumed) == 0 {
		t.Fatalf("首包应被消费")
	}
}

// TestGateLoginReject 未就绪 login → 判定代答断开。
func TestGateLoginReject(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		_, _ = client.Write(handshakePacket("localhost", 25565, 2))
		for {
			buf := make([]byte, 256)
			_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := client.Read(buf); err != nil {
				return
			}
		}
	}()

	decision, _, err := Gate(server, false)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if decision != AnswerReject {
		t.Fatalf("期望 AnswerReject，实为 %d", decision)
	}
}

// TestGatePassthrough 已就绪 → 透传（消费的首包原样可回灌）。
func TestGatePassthrough(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go func() {
		_, _ = client.Write(handshakePacket("localhost", 25565, 2))
	}()

	decision, consumed, err := Gate(server, true)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if decision != Passthrough {
		t.Fatalf("已就绪应透传，实为 %d", decision)
	}
	if len(consumed) == 0 {
		t.Fatalf("应消费首包供回灌")
	}
}
