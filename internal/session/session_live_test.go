package session

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// TestLiveSessionFullFlow 会话级真网集成测试：
// 房主 StartHost（真实信箱注册）→ 客机 StartGuest（真实扫槽+Answer）
// → WebRTC 打通（本机 host candidates）→ 本地端口回显 MC 数据。
// 依赖外网（paste.sdjz.wiki），-short 时跳过。
func TestLiveSessionFullFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("需要真实网络")
	}

	// 假 MC 服务器
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

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	hostEv := make(chan Event, 16)
	h, err := StartHost(ctx, HostConfig{MCPort: mc.Addr().(*net.TCPAddr).Port}, func(e Event) {
		select {
		case hostEv <- e:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop()

	// 等房间创建完成（waiting 事件意味着槽位已注册）
	waitEvent(t, hostEv, ctx, "waiting", 30*time.Second)
	code := h.Code()
	if code == "" {
		t.Fatal("房间码为空")
	}
	t.Logf("房间码: %s (v6: %v)", code, h.HasV6())

	// 客机加入
	guestEv := make(chan Event, 16)
	g, err := StartGuest(ctx, GuestConfig{Code: code}, func(e Event) {
		select {
		case guestEv <- e:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Stop()

	ev := waitEvent(t, guestEv, ctx, "connected", 60*time.Second)
	t.Logf("客机已连接: tier=%s addr=%s", ev.Tier, ev.Addr)

	// 等待 200ms 让监听就绪
	time.Sleep(300 * time.Millisecond)

	// 连接"MC 客户端"：写入并回读
	conn, err := net.DialTimeout("tcp", ev.Addr, 5*time.Second)
	if err != nil {
		t.Fatalf("本地连接失败: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	msg := "clink-session-e2e-ok"
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if string(buf) != msg {
		t.Fatalf("回显不一致: %q", buf)
	}
	_ = conn.Close()

	// 房主应收到 guest-joined
	waitEvent(t, hostEv, ctx, "guest-joined", 20*time.Second)
	if h.Guests() < 1 {
		t.Fatalf("房主人数未增加: %d", h.Guests())
	}
}

// waitEvent 阻塞等待指定 stage 的事件（超时致命）。
func waitEvent(t *testing.T, ch chan Event, ctx context.Context, stage string, timeout time.Duration) Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case e := <-ch:
			if e.Stage == stage {
				return e
			}
			if e.Stage == "error" {
				t.Fatalf("收到错误事件: %s", e.Msg)
			}
		case <-deadline:
			t.Fatalf("等待事件 %s 超时（%v）", stage, timeout)
		case <-ctx.Done():
			t.Fatalf("上下文取消：等待 %s", stage)
		}
	}
}
