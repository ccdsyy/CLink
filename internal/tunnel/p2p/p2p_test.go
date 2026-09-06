package p2p

import (
	"context"
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

	// 客机开本地监听（随机端口）
	if err := g.ServeLocal(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	local := "127.0.0.1:" + itoa(g.LocalPort())

	// 连接 1：模拟 MC 客户端 ping
	c1, err := net.DialTimeout("tcp", local, 5*time.Second)
	if err != nil {
		t.Fatalf("本地连接失败: %v", err)
	}
	testEcho(t, c1, "hello-clink")

	// 连接 2：模拟正式游戏连接（验证多通道并发）
	c2, err := net.DialTimeout("tcp", local, 5*time.Second)
	if err != nil {
		t.Fatalf("第二条本地连接失败: %v", err)
	}
	testEcho(t, c2, "second-connection-payload")

	// 大分片验证（> 16KB，跨多个 DataChannel 消息）
	c3, err := net.DialTimeout("tcp", local, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	big := make([]byte, chunkSize*3)
	for i := range big {
		big[i] = byte(i % 251)
	}
	go func() { _, _ = c3.Write(big) }()
	got := make([]byte, len(big))
	_ = c3.SetReadDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.ReadFull(c3, got); err != nil {
		t.Fatalf("大包回读失败: %v", err)
	}
	for i := range big {
		if big[i] != got[i] {
			t.Fatalf("大包数据不一致 @%d", i)
		}
	}
	_ = c3.Close()
}

func testEcho(t *testing.T, c net.Conn, msg string) {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if string(buf) != msg {
		t.Fatalf("echo 不一致: %q != %q", buf, msg)
	}
	_ = c.Close()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
