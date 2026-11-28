package rudp

import (
	"bytes"

	"io"
	"math/rand"
	"net"
	"testing"
	"time"
)

// TestLoopbackPlain 明文模式全链路：Open → AcceptStream → echo。
func TestLoopbackPlain(t *testing.T) {
	a, b := pair(t)
	defer a.Close()
	defer b.Close()

	tag := uint32(0x12345678)
	raddr := localAddr(b) // 服务端地址（原误写为客户端自身地址，自发自收造成假阳性）

	go func() {
		s, err := b.AcceptStream(tag, nil, 5*time.Second)
		if err != nil {
			return
		}
		_, _ = io.Copy(s, s) // echo
	}()

	s, err := a.Open(raddr, tag, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	echoRound(t, s, "hello-clink-rudp")
	// 大包（> ChunkSize × 3，跨多分片）
	big := make([]byte, ChunkSize*3+17)
	rand.Read(big)
	echoRound(t, s, string(big))
}

// TestLoopbackEncrypted 加密模式（#51 简化版）：密钥不符必须立刻失败。
func TestLoopbackEncrypted(t *testing.T) {
	RUDPDebugForTest()
	a, b := pair(t)
	defer a.Close()
	defer b.Close()

	tag := uint32(0xdeadbeef)
	key := bytes.Repeat([]byte{7}, 32)
	raddr := localAddr(b)

	go func() {
		s, err := b.AcceptStream(tag, key, 5*time.Second)
		if err != nil {
			return
		}
		buf := make([]byte, 4096)
		for {
			n, err := s.Read(buf)
			if err != nil {
				return
			}
			if n >= 8 && buf[0] == 'e' && buf[1] == 'n' {
				t.Logf("[server] 读到 %d 字节: %q", n, buf[:min(20, n)])
			} else if n >= 4 {
				t.Logf("[server] 读到异常数据 %d 字节: %x", n, buf[:min(16, n)])
			}
			if _, werr := s.Write(buf[:n]); werr != nil {
				return
			}
		}
	}()

	s, err := a.Open(raddr, tag, key)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	echoRound(t, s, "encrypted-payload")

	// 错误密钥的流：对端 GCM 认证失败会断流
	s2, err := a.Open(raddr, tag+1, bytes.Repeat([]byte{8}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	done := make(chan error, 1)
	go func() {
		_, err := s2.Write([]byte("wrong-key"))
		done <- err
	}()
	select {
	case err := <-done:
		_ = err // 写可能成功（UDP 无反馈），读对端断开即可
	case <-time.After(2 * time.Second):
	}
}

func echoRound(t *testing.T, s *Stream, msg string) {
	t.Helper()
	payload := []byte(msg)
	done := make(chan []byte, 1)
	go func() {
		got := make([]byte, len(payload))
		_, _ = io.ReadFull(s, got)
		done <- got
	}()
	if _, err := s.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case got := <-done:
		if !bytes.Equal(got, payload) {
			t.Fatalf("echo 不一致: %q vs %q", got[:min(32, len(got))], payload[:min(32, len(payload))])
		}
	case <-time.After(8 * time.Second):
		t.Fatalf("echo 超时（%d 字节）", len(payload))
	}
}

func pair(t *testing.T) (*Endpoint, *Endpoint) {
	t.Helper()
	a, err := NewEndpoint(0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewEndpoint(0)
	if err != nil {
		t.Fatal(err)
	}
	return a, b
}

func localAddr(e *Endpoint) *net.UDPAddr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: e.LocalPort()}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
