package relay

import (
	"crypto/sha256"
	"io"
	"net"
	"testing"
	"time"

	"github.com/ccdsyy/clink/internal/rudp"
)

// TestRelayLoopback 中继全链路：两端经中继 rudp 会话互通 + echo。
func TestRelayLoopback(t *testing.T) {
	rudp.RUDPDebugForTest()
	psk := "test-psk"
	room := "abc234" // 防混淆字母表内

	// 起中继服务端
	sconn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer sconn.Close()
	srv := NewServer(psk)
	go srv.ServeUDP(sconn)
	serverAddr := sconn.LocalAddr().(*net.UDPAddr)

	// 选址（同时完成两端注册，#38）
	guestAddr, guestConn, _, err := SelectRelay(psk, room, RoleGuest, []*net.UDPAddr{serverAddr})
	if err != nil {
		t.Fatalf("guest 选址失败: %v", err)
	}
	_ = guestAddr
	hostAddr, hostConn, peerIntro, err := SelectRelay(psk, room, RoleHost, []*net.UDPAddr{serverAddr})
	if err != nil {
		t.Fatalf("host 选址失败: %v", err)
	}
	_ = hostAddr
	if peerIntro == nil {
		t.Log("引路：host ping 时对端地址未登记（时序），可忽略——后续 PONG 会带上")
	} else {
		t.Logf("引路 ✓：host 观察到 guest 地址 %s", peerIntro)
	}

	tag := DeriveTag(psk, room)
	key := sha256.Sum256([]byte("room-key-" + room)) // 端到端密钥（#51 简化版）

	// 双方 rudp 直接包住中继 socket（#52 单 socket 多路复用：
	// PING 注册的地址 = 数据帧来源地址，中继按源地址配对才一致）
	guestEp := rudp.Wrap(guestConn)
	defer guestEp.Close()
	hostEp := rudp.Wrap(hostConn)
	defer hostEp.Close()

	// 双方各自把 rudp socket 的 UDP 连接句柄指到中继：
	// rudp.Endpoint 自带 socket，这里直接用 SelectRelay 返回的 conn 做中继保活
	stop := make(chan struct{})
	defer close(stop)
	go KeepaliveLoop(hostConn, hostAddr, tag, psk, room, RoleHost, stop)
	go KeepaliveLoop(guestConn, guestAddr, tag, psk, room, RoleGuest, stop)

	go func() {
		stream, err := hostEp.AcceptStream(tag, key[:], 8*time.Second)
		if err != nil {
			t.Logf("[host] AcceptStream 失败: %v", err)
			return
		}
		t.Log("[host] 流已建立，开始 echo")
		n, err := io.Copy(stream, stream)
		t.Logf("[host] echo 结束: n=%d err=%v", n, err)
	}()

	// 客机：经中继打开流（rudp socket 发往中继地址，tag 路由到房主）
	stream, err := guestEp.Open(serverAddr, tag, key[:])
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	t.Log("[guest] 流已打开")

	msg := []byte("hello-over-relay")
	done := make(chan []byte, 1)
	go func() {
		got := make([]byte, len(msg))
		_, _ = io.ReadFull(stream, got)
		done <- got
	}()
	if _, err := stream.Write(msg); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if string(got) != string(msg) {
			t.Fatalf("echo 不一致")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("中继 echo 超时")
	}
}

// TestRelayAuthReject 错误 PSK（tag 不匹配）必须被静默拒绝（#33）。
func TestRelayAuthReject(t *testing.T) {
	psk := "right-psk"
	room := "abc234"

	sconn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer sconn.Close()
	go NewServer(psk).ServeUDP(sconn)
	serverAddr := sconn.LocalAddr().(*net.UDPAddr)

	// 攻击者用错误 psk 计算 tag → ping 应无响应
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	badTag := DeriveTag("wrong-psk", room)
	start := time.Now()
	_, err = Ping(conn, serverAddr, badTag, "wrong-psk", room, RoleGuest)
	if err == nil {
		t.Fatal("错误 psk 竟然收到了 PONG（鉴权失效）")
	}
	if elapsed := time.Since(start); elapsed > pingTimeout+2*time.Second {
		t.Fatalf("拒绝过慢: %v", elapsed)
	}
}
