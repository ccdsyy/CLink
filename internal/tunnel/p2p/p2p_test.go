package p2p

import (
	"io"
	"net"
	"testing"
	"time"
)

// TestP2PLoopback 本机回环全链路：
// 房主 Offer → 客机 Answer → ctrl 打通 → 客机本地连接 → 数据经 DC 到达假 MC 服务器。
// 不配置 STUN（仅 host candidates），无需公网即可验证整个桥接栈。
func TestP2PLoopback(t *testing.T) {
	// 假 MC 服务器：echo
	mc, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer mc.Close()
	go func() {
		for {
			c, err := mc.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	mcPort := mc.Addr().(*net.TCPAddr).Port

	h, err := NewHost(Config{MCPort: mcPort})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop()

	oT, oS, err := h.Offer()
	if err != nil {
		t.Fatal(err)
	}

	g, err := NewGuest(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Stop()
	if err := g.AcceptOffer(oT, oS); err != nil {
		t.Fatal(err)
	}
	aT, aS, err := g.Answer()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.AcceptAnswer(aT, aS); err != nil {
		t.Fatal(err)
	}

	// 等待 ctrl 通道就绪
	select {
	case <-g.Established():
	case <-time.After(20 * time.Second):
		t.Fatal("客机 ctrl 通道超时未打通")
	}
	select {
	case <-h.Established():
	case <-time.After(20 * time.Second):
		t.Fatal("房主 ctrl 通道超时未打通")
	}

	// #56 路径抽象：调度器 accept 本地连接后调 Open() 取对端流再桥接。
	// 这里模拟三条本地连接，Open() 三次，全部走同一套 open/ack 握手。
	testOpen := func(payload []byte) {
		remote, err := g.Open()
		if err != nil {
			t.Fatalf("Open 失败: %v", err)
		}
		defer remote.Close()
		go func() { _, _ = remote.Write(payload) }()
		got := make([]byte, len(payload))
		_ = remote.SetReadDeadline(time.Now().Add(15 * time.Second))
		if _, err := io.ReadFull(remote, got); err != nil {
			t.Fatalf("回读失败: %v", err)
		}
		for i := range payload {
			if payload[i] != got[i] {
				t.Fatalf("数据不一致 @%d", i)
			}
		}
	}

	testOpen([]byte("hello-clink"))               // MC ping
	testOpen([]byte("second-connection-payload")) // 多通道并发
	big := make([]byte, chunkSize*3)              // >16KB 跨分片
	for i := range big {
		big[i] = byte(i % 251)
	}
	testOpen(big)
}
