package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/ccdsyy/clink/internal/mailbox"
	"github.com/ccdsyy/clink/internal/netprobe"
	"github.com/ccdsyy/clink/internal/tunnel/p2p"
	"github.com/ccdsyy/clink/internal/tunnel/v6direct"
)

// GuestConfig 客机配置。
type GuestConfig struct {
	Code      string
	STUN      []string
	TURN      []string
	Mailbox   *mailbox.Client
	LocalPort int // 默认 25565
	Name      string
}

// Guest 客机会话状态机：扫槽 → 取 Offer → 选层连接 → 注册 Answer → 本地监听。
// WebRTC 层断线后自动重连（epoch 重扫）。
type Guest struct {
	ctx    context.Context
	cancel context.CancelFunc
	cfg    GuestConfig
	emit   func(Event)

	tier string
	addr string
	mu   sync.Mutex
}

// StartGuest 启动客机会话（后台 goroutine）。
func StartGuest(ctx context.Context, cfg GuestConfig, emit func(Event)) (*Guest, error) {
	if !mailbox.ValidateRoomCode(cfg.Code) {
		return nil, mailbox.ErrInvalidRoomCode
	}
	if cfg.Mailbox == nil {
		cfg.Mailbox = mailbox.NewClient("")
	}
	if len(cfg.STUN) == 0 {
		cfg.STUN = []string{"stun:stun.l.google.com:19302"}
	}
	if cfg.LocalPort <= 0 {
		cfg.LocalPort = 25565
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c, cancel := context.WithCancel(ctx)
	g := &Guest{ctx: c, cancel: cancel, cfg: cfg, emit: emit}
	go g.loop()
	return g, nil
}

// Tier 实际使用的连接层。
func (g *Guest) Tier() string { g.mu.Lock(); defer g.mu.Unlock(); return g.tier }

// Addr 本地连接地址。
func (g *Guest) Addr() string { g.mu.Lock(); defer g.mu.Unlock(); return g.addr }

// Stop 退出加入。
func (g *Guest) Stop() { g.cancel() }

func (g *Guest) loop() {
	hint := ""
	for attempt := 0; ; attempt++ {
		if g.ctx.Err() != nil {
			return
		}
		err := g.attempt(hint)
		if err == nil || g.ctx.Err() != nil {
			return
		}
		hint = err.Error()
		stage, msg := "reconnecting", "网络波动，正在自动重连…"
		if errors.Is(err, mailbox.ErrInvalidRoomCode) || errors.Is(err, errRoomNotFound) {
			g.emit(Event{Stage: "error", Msg: err.Error()})
			return
		}
		g.emit(Event{Stage: stage, Msg: msg + "（" + err.Error() + "）"})
		backoff := time.Duration(2+attempt) * time.Second
		if backoff > 10*time.Second {
			backoff = 10 * time.Second
		}
		select {
		case <-g.ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

var errRoomNotFound = errors.New("房间不存在或已过期，请核对房间码")

// attempt 一次完整加入尝试；返回 nil 表示连接建立且随后自然结束（用户退出/断开重连）。
func (g *Guest) attempt(lastErr string) error {
	// 1. 扫描最新有效 epoch
	epoch, sig, err := g.findLatestOffer()
	if err != nil {
		if errors.Is(err, mailbox.ErrInvalidRoomCode) {
			return err
		}
		return fmt.Errorf("%w（%v）", errRoomNotFound, err)
	}

	localPort := g.cfg.LocalPort

	// 2. Tier 1：IPv6 直连（双方都有公网 v6 时）
	if sig.V6 != "" && netprobe.HasIPv6Route() {
		v6addr := net.JoinHostPort(sig.V6, strconv.Itoa(sig.V6Port))
		if conn, derr := net.DialTimeout("tcp6", v6addr, 3*time.Second); derr == nil {
			_ = conn.Close()
			// 注册 Answer（通知房主 + 占槽防并发冲突）
			if rerr := g.registerAnswer(epoch, "ipv6", "", ""); rerr != nil {
				if errors.Is(rerr, mailbox.ErrCodeTaken) {
					return errors.New("加入排队中（有其他小伙伴正在加入），即将自动重试")
				}
				return rerr
			}
			v6g, serr := v6direct.StartGuest(g.ctx, sig.V6, sig.V6Port, localPort)
			if serr != nil {
				return fmt.Errorf("本地端口 25565 被占用？%v", serr)
			}
			g.mu.Lock()
			g.tier, g.addr = "ipv6", "127.0.0.1:"+strconv.Itoa(v6g.LocalPort())
			g.mu.Unlock()
			g.emit(Event{Stage: "connected", Tier: "ipv6", Addr: g.addr,
				Msg: "连接成功（IPv6 极速直连）！"})
			<-g.ctx.Done()
			return nil
		}
		// v6 拨号失败 → 静默降级 WebRTC
	}

	// 3. Tier 2：WebRTC 打洞
	pc, err := p2p.NewGuest(p2p.Config{STUN: g.cfg.STUN, TURN: g.cfg.TURN})
	if err != nil {
		return fmt.Errorf("WebRTC 初始化失败: %w", err)
	}
	if err := pc.AcceptOffer(sig.SDPType, sig.SDP); err != nil {
		pc.Stop()
		return fmt.Errorf("Offer 无效: %w", err)
	}
	aType, aSDP, err := pc.Answer()
	if err != nil {
		pc.Stop()
		return fmt.Errorf("Answer 生成失败: %w", err)
	}
	if err := g.registerAnswer(epoch, "webrtc", aType, aSDP); err != nil {
		pc.Stop()
		if errors.Is(err, mailbox.ErrCodeTaken) {
			return errors.New("加入排队中（有其他小伙伴正在加入），即将自动重试")
		}
		return err
	}

	// 等 ctrl 打通
	select {
	case <-pc.Established():
	case <-time.After(45 * time.Second):
		pc.Stop()
		return errors.New("打洞超时：网络环境复杂，建议切换手机热点，或在高级设置填写中转服务器（TURN）")
	case <-g.ctx.Done():
		pc.Stop()
		return nil
	}

	if err := pc.ServeLocal(g.ctx, localPort); err != nil {
		pc.Stop()
		return fmt.Errorf("本地端口 25565 被占用？%v", err)
	}
	g.mu.Lock()
	g.tier, g.addr = "webrtc", "127.0.0.1:"+strconv.Itoa(pc.LocalPort())
	g.mu.Unlock()
	g.emit(Event{Stage: "connected", Tier: "webrtc", Addr: g.addr,
		Msg: "连接成功（P2P 打洞）！"})
	g.emit(Event{Stage: "info", Msg: "打开游戏 → 多人游戏 → 直接连接 → 粘贴 " + g.addr})

	// 4. 心跳监控：断开 → 重连
	select {
	case <-pc.Dead():
		pc.Stop()
		return errors.New("连接断开")
	case <-g.ctx.Done():
		pc.Stop()
		return nil
	}
}

// registerAnswer 写 Answer 信箱 + 注册槽位。
func (g *Guest) registerAnswer(epoch int, tier, sdpType, sdp string) error {
	key, err := mailbox.DeriveKey(g.cfg.Code)
	if err != nil {
		return err
	}
	ans := mailbox.Signal{
		V:       mailbox.ProtoVersion,
		Type:    "answer",
		Epoch:   epoch,
		Tier:    tier,
		Nonce:   mailbox.Nonce(),
		Ts:      time.Now().Unix(),
		Name:    g.cfg.Name,
		SDPType: sdpType,
		SDP:     sdp,
	}
	payload, _ := json.Marshal(ans)
	sealed, err := mailbox.Seal(key, payload)
	if err != nil {
		return err
	}
	id, err := g.cfg.Mailbox.Store(sealed, mailbox.DefaultTTL)
	if err != nil {
		return fmt.Errorf("Answer 信箱写入失败: %w", err)
	}
	slot := mailbox.SlotCode(g.cfg.Code, epoch, mailbox.RoleAnswer)
	if err := g.cfg.Mailbox.Shorten(mailbox.GitHubPagesRedirect+"?id="+id, slot); err != nil {
		if errors.Is(err, mailbox.ErrCodeTaken) {
			return err // 上层转成"排队重试"
		}
		return fmt.Errorf("Answer 槽位注册失败: %w", err)
	}
	return nil
}

// findLatestOffer 并发扫描 31 个 epoch 槽，取最新有效 Offer。
func (g *Guest) findLatestOffer() (int, *mailbox.Signal, error) {
	type slotHit struct {
		epoch int
		url   string
	}
	key, err := mailbox.DeriveKey(g.cfg.Code)
	if err != nil {
		return 0, nil, err
	}

	var wg sync.WaitGroup
	hits := make(chan slotHit, mailbox.MaxEpoch)
	sem := make(chan struct{}, 8)
	for e := 0; e < mailbox.MaxEpoch; e++ {
		wg.Add(1)
		go func(e int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			u, err := g.cfg.Mailbox.RedirectInfo(mailbox.SlotCode(g.cfg.Code, e, mailbox.RoleOffer))
			if err == nil {
				hits <- slotHit{epoch: e, url: u}
			}
		}(e)
	}
	wg.Wait()
	close(hits)

	// 从高到低找第一个"可解密且校验通过"的 Offer
	maxEpoch := -1
	valid := map[int]slotHit{}
	for h := range hits {
		valid[h.epoch] = h
		if h.epoch > maxEpoch {
			maxEpoch = h.epoch
		}
	}
	for e := maxEpoch; e >= 0; e-- {
		h, ok := valid[e]
		if !ok {
			continue
		}
		id, err := mailbox.IDFromRedirectURL(h.url)
		if err != nil {
			continue
		}
		sealed, err := g.cfg.Mailbox.Get(id)
		if err != nil {
			continue // 旧信箱可能已过期
		}
		plain, err := mailbox.Open(key, sealed)
		if err != nil {
			continue // 假房间/密钥不符
		}
		var sig mailbox.Signal
		if err := json.Unmarshal(plain, &sig); err != nil {
			continue
		}
		if err := sig.Validate("offer"); err != nil {
			continue
		}
		return e, &sig, nil
	}
	return 0, nil, errors.New("未找到有效房间")
}
