// Package v6direct 实现 Tier 1：IPv6 公网直连 TCP 隧道。
//
// 房主侧：监听公网 IPv6 随机端口，把流量桥接到本机 MC 端口；
// 客机侧：本机监听 127.0.0.1:25565，把流量桥接到房主 IPv6。
// 全程点对点，无 NAT、无中转、无信令依赖。
package v6direct

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

// Bridge 双向桥接两条 TCP 连接，任一方向断开即整体回收。
// 启用 20 秒 TCP KeepAlive 防止中间设备静默掐断长连接（应用层保活的系统级等价实现）。
func Bridge(a, b net.Conn) {
	setKeepAlive(a, 20*time.Second)
	setKeepAlive(b, 20*time.Second)
	defer a.Close()
	defer b.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	<-done
}

func setKeepAlive(c net.Conn, period time.Duration) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(period)
	}
}

// ---- 房主侧 ----

// Host 房主侧直连服务：监听 [::] 随机端口 → 桥接 127.0.0.1:mcPort。
type Host struct {
	ln     net.Listener
	mcPort int
	ctx    context.Context
}

// StartHost 启动房主侧 IPv6 监听。返回实际端口（写入信令 V6Port）。
func StartHost(ctx context.Context, mcPort int) (*Host, int, error) {
	ln, err := net.Listen("tcp6", ":0")
	if err != nil {
		return nil, 0, fmt.Errorf("IPv6 监听失败: %w", err)
	}
	h := &Host{ln: ln, mcPort: mcPort, ctx: ctx}
	go h.accept()
	port := ln.Addr().(*net.TCPAddr).Port
	return h, port, nil
}

func (h *Host) accept() {
	for {
		conn, err := h.ln.Accept()
		if err != nil {
			return // listener closed or ctx done
		}
		go func(c net.Conn) {
			defer c.Close()
			target, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", h.mcPort), 5*time.Second)
			if err != nil {
				return // MC 未启动
			}
			Bridge(c, target)
		}(conn)
	}
}

// Stop 停止监听（已建立的连接自然收尾）。
func (h *Host) Stop() {
	if h.ln != nil {
		_ = h.ln.Close()
	}
}

// ---- 客机侧 ----

// Guest 客机侧直连：本机监听 127.0.0.1:localPort → 桥接房主 [v6]:hostPort。
type Guest struct {
	ln       net.Listener
	hostAddr string
	mu       sync.Mutex
}

// StartGuest 启动客机侧本地监听，返回可 dial 的本地地址。
func StartGuest(ctx context.Context, hostIP string, hostPort, localPort int) (*Guest, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(localPort))
	if err != nil {
		return nil, fmt.Errorf("本地端口 %d 被占用：%w", localPort, err)
	}
	g := &Guest{
		ln:       ln,
		hostAddr: net.JoinHostPort(hostIP, strconv.Itoa(hostPort)),
	}
	go g.accept(ctx)
	return g, nil
}

// LocalPort 返回实际监听的本地端口（localPort=0 时为随机端口）。
func (g *Guest) LocalPort() int {
	if g.ln == nil {
		return 0
	}
	return g.ln.Addr().(*net.TCPAddr).Port
}

func (g *Guest) accept(ctx context.Context) {
	for {
		conn, err := g.ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			remote, err := net.DialTimeout("tcp6", g.hostAddr, 8*time.Second)
			if err != nil {
				return
			}
			Bridge(c, remote)
		}(conn)
	}
}

// Stop 停止本地监听。
func (g *Guest) Stop() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ln != nil {
		_ = g.ln.Close()
	}
}
