// Package tunnel 统一路径抽象（ROADMAP #56）。
//
// LAN 直连 / 公网映射直连 / IPv6 直连 / WebRTC 打洞 / 自建中继
// 全部实现同一 Path 接口：上层（session 调度器）只认接口，
// 时间预算竞速（#21）与滞回切换（#57）对路径类型无感。
package tunnel

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Path 一条到房主的传输路径。
type Path interface {
	// Name 路径类型：lan | pubtcp | v6 | webrtc | relay
	Name() string
	// Establish 建立控制通道（阻塞，ctx 控制预算超时）。
	Establish(ctx context.Context) error
	// Open 为一条本地 MC 连接打开对端流（Establish 成功后调用）。
	Open() (net.Conn, error)
	// RTT 最近平滑往返延迟（0=未知）。
	RTT() time.Duration
	// Dead 路径死亡信号。
	Dead() <-chan struct{}
	// Close 关闭路径。
	Close()
}

// Plan 调度计划中的一项。
type Plan struct {
	Name   string
	Budget time.Duration // 建立时间预算（#21 时间预算调度器）
	Make   func(ctx context.Context) (Path, error)
}

// ---- TCP 直连类路径（LAN / PubTCP / v6 共用）----

// DialPath 直连路径：Establish = 预拨验证，Open = 实际拨号。
type DialPath struct {
	name    string
	addrs   []string // 多地址候选（#28：并发拨取最快）
	budget  time.Duration
	chosen  atomic.Value // string
	dead    chan struct{}
	deadOne sync.Once
}

// NewDialPath 构造直连路径。
func NewDialPath(name string, addrs []string, budget time.Duration) *DialPath {
	return &DialPath{name: name, addrs: addrs, budget: budget, dead: make(chan struct{})}
}

func (p *DialPath) Name() string          { return p.name }
func (p *DialPath) Dead() <-chan struct{} { return p.dead }
func (p *DialPath) RTT() time.Duration    { return 0 } // 直连 RTT 由会话心跳测量
func (p *DialPath) markDead()             { p.deadOne.Do(func() { close(p.dead) }) }
func (p *DialPath) Close()                { p.markDead() }

// Establish 并发预拨全部地址（#28 多候选），最快的留下作为后续 Open 目标。
func (p *DialPath) Establish(ctx context.Context) error {
	type res struct {
		addr string
		rtt  time.Duration
	}
	ch := make(chan res, len(p.addrs))
	for _, a := range p.addrs {
		go func(a string) {
			start := time.Now()
			c, err := net.DialTimeout("tcp", a, p.budget)
			if err != nil {
				return
			}
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetNoDelay(true)
			}
			_ = c.Close()
			ch <- res{addr: a, rtt: time.Since(start)}
		}(a)
	}
	timer := time.NewTimer(p.budget + 500*time.Millisecond)
	defer timer.Stop()
	best := ""
	bestRTT := time.Duration(1 << 62)
	for i := 0; i < len(p.addrs); i++ {
		select {
		case r := <-ch:
			if r.rtt < bestRTT {
				best, bestRTT = r.addr, r.rtt
			}
		case <-ctx.Done():
			i = len(p.addrs)
		case <-timer.C:
			i = len(p.addrs)
		}
	}
	if best == "" {
		return errors.New("所有候选地址拨号失败")
	}
	p.chosen.Store(best)
	return nil
}

// Open 拨号一条新连接（NoDelay 已开，#53）。
func (p *DialPath) Open() (net.Conn, error) {
	addr, _ := p.chosen.Load().(string)
	if addr == "" {
		return nil, errors.New("path not established")
	}
	c, err := net.DialTimeout("tcp", addr, 8*time.Second)
	if err != nil {
		p.markDead()
		return nil, err
	}
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true) // #53
	}
	return c, nil
}
