package v6direct

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// TestV6Loopback 用 [::1] 回环模拟 IPv6 直连全链路：
// 房主监听 → 客机本地 127.0.0.1 监听 → 数据桥接到假 MC 服务器。
func TestV6Loopback(t *testing.T) {
	// 假 MC 服务器（v4 本地）
	mc, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer mc.Close()
	mcPort := mc.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := mc.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()

	ctx := context.Background()
	h, port, err := StartHost(ctx, mcPort)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop()
	if port <= 0 {
		t.Fatalf("监听端口异常: %d", port)
	}

	g, err := StartGuest(ctx, "::1", port, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Stop()

	local := "127.0.0.1:" + func() string {
		p := g.LocalPort()
		b := []byte{}
		if p == 0 {
			t.Fatal("客机端口异常")
		}
		for p > 0 {
			b = append([]byte{byte('0' + p%10)}, b...)
			p /= 10
		}
		return string(b)
	}()

	conn, err := net.DialTimeout("tcp", local, 5*time.Second)
	if err != nil {
		t.Fatalf("本地连接失败: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	msg := "v6-direct-tunnel-ok"
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if string(buf) != msg {
		t.Fatalf("echo 不一致: %q", buf)
	}
	_ = conn.Close()
}
