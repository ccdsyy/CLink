package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ccdsyy/clink/internal/lanscan"
	"github.com/ccdsyy/clink/internal/mailbox"
	"github.com/ccdsyy/clink/internal/mcproto"
	"github.com/ccdsyy/clink/internal/netprobe"
	"github.com/ccdsyy/clink/internal/relay"
	"github.com/ccdsyy/clink/internal/rudp"
	"github.com/ccdsyy/clink/internal/tunnel"
	"github.com/ccdsyy/clink/internal/tunnel/p2p"
)

// GuestConfig 客机配置。
type GuestConfig struct {
	Code       string
	STUN       []string
	TURN       []string
	Mailbox    *mailbox.Client
	LocalPort  int // 默认 25565
	Name       string
	RelayAddrs []*net.UDPAddr // 自建中继节点（ROADMAP #31，空=不启用）
	RelayPSK   string         // 中继共享口令
	LastTier   string         // 路径记忆（ROADMAP #58）：上次成功的路径类型
	Scanner    *lanscan.Scanner
}

// Guest 客机会话状态机（ROADMAP #21 时间预算调度器）：
//
//	快速层顺序尝试：LAN 组播直连(#66) → 公网映射直连(#22) → IPv6 直连
//	竞速层并行赛跑：WebRTC 打洞 ∥ 自建中继，谁先通用谁
//	全程事件推送 + 自动重连（epoch 重扫）。
type Guest struct {
	ctx    context.Context
	cancel context.CancelFunc
	cfg    GuestConfig
	emit   func(Event)

	natCls string // 本机 NAT 映射类别（矩阵输入，#19）

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
		// 多节点探测选优，ICE 只挂最优单节点（多候选在 CGNAT 环境实测易失灵）
		cfg.STUN = []string{netprobe.PickSTUN()}
	}
	if cfg.LocalPort <= 0 {
		cfg.LocalPort = 25565
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c, cancel := context.WithCancel(ctx)
	g := &Guest{ctx: c, cancel: cancel, cfg: cfg, emit: emit, natCls: netprobe.MappingClass()}
	go g.loop()
	return g, nil
}

// Tier 实际使用的连接层。
func (g *Guest) Tier() string { g.mu.Lock(); defer g.mu.Unlock(); return g.tier }

// Addr 本地连接地址。
func (g *Guest) Addr() string { g.mu.Lock(); defer g.mu.Unlock(); return g.addr }

// NATClass 本机 NAT 映射类别（诊断面板 #62）。
func (g *Guest) NATClass() string { return g.natCls }

// Stop 退出加入。
func (g *Guest) Stop() { g.cancel() }

func (g *Guest) loop() {
	hint := ""
	for attempt := 0; ; attempt++ {
		if g.ctx.Err() != nil {
			return
		}
		tier, err := g.attempt(hint)
		if err == nil || g.ctx.Err() != nil {
			return
		}
		hint = err.Error()
		if tier != "" {
			g.mu.Lock()
			g.tier = tier
			g.mu.Unlock()
		}
		if errors.Is(err, mailbox.ErrInvalidRoomCode) || errors.Is(err, errRoomNotFound) {
			g.emit(Event{Stage: "error", Msg: err.Error()})
			return
		}
		g.emit(Event{Stage: "reconnecting", Msg: "网络波动，正在自动重连…（" + err.Error() + "）"})
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

// attempt 一次完整加入；返回成功时使用的 tier（nil 表示连接建立且随后自然结束）。
// 网络断开时返回 tier + 错误（外层重连，#58 记忆会指导下次优先路径）。
func (g *Guest) attempt(lastErr string) (string, error) {
	// 1. 扫描最新有效 epoch
	epoch, sig, err := g.findLatestOffer()
	if err != nil {
		if errors.Is(err, mailbox.ErrInvalidRoomCode) {
			return "", err
		}
		return "", fmt.Errorf("%w（%v）", errRoomNotFound, err)
	}

	localPort := g.cfg.LocalPort

	// 2. 构建路径计划（ROADMAP #19 矩阵 + #58 路径记忆 + #66 LAN 发现）
	fast, race := g.buildPlan(sig, epoch)

	// 3. 快速层：顺序尝试，各给固定预算（#21）
	for _, p := range fast {
		if g.ctx.Err() != nil {
			return "", nil
		}
		ectx, ecancel := context.WithTimeout(g.ctx, p.Budget)
		path, err := p.Make(ectx)
		ecancel()
		if err != nil {
			continue
		}
		tier := g.serve(path, epoch, localPort)
		return tier, nil
	}

	// 4. 竞速层：并行赛跑，先通先用（#21）；#18 预检——出站 UDP 受限
	//（无 srflx 且无 v6）时 WebRTC 预算压短，直接把中继放前面。
	var wg sync.WaitGroup
	winner := make(chan tunnel.Path, 2)
	loser := make(chan tunnel.Path, 2)

	startRace := func(p tunnel.Plan) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ectx, ecancel := context.WithTimeout(g.ctx, p.Budget)
			path, err := p.Make(ectx)
			ecancel()
			if err != nil {
				return
			}
			select {
			case winner <- path:
			default:
				loser <- path // 迟到的赢家：立刻让位并回收
				path.Close()
			}
		}()
	}
	for _, p := range race {
		startRace(p)
	}
	if len(race) == 0 {
		return "", errors.New("无可用连接路径（配置了自建中继节点可提升成功率）")
	}

	// 等第一个赢家（总预算封顶）
	deadline := time.After(60 * time.Second)
	var win tunnel.Path
	for win == nil {
		select {
		case p := <-winner:
			win = p
		case <-deadline:
			return "", errors.New("连接超时：网络环境复杂，建议切换手机热点或配置自建中继节点")
		case <-g.ctx.Done():
			return "", nil
		}
	}
	// 清理迟到者
	go func() {
		for {
			select {
			case p := <-loser:
				p.Close()
			case <-time.After(500 * time.Millisecond):
				return
			}
		}
	}()
	wg.Wait()

	tier := g.serve(win, epoch, localPort)
	return tier, nil
}

// buildPlan 构建路径计划。
// 快速层：LAN → 公网映射 → IPv6（顺序，秒级预算）
// 竞速层：WebRTC ∥ 中继（#19 矩阵按 NAT 类别调整预算）
func (g *Guest) buildPlan(sig *mailbox.Signal, epoch int) (fast []tunnel.Plan, race []tunnel.Plan) {
	code := g.cfg.Code

	// LAN 组播发现（#66）
	if addr, ok := g.cfg.Scanner.Lookup(code); ok {
		fast = append(fast, tunnel.Plan{
			Name: "lan", Budget: 1500 * time.Millisecond,
			Make: func(ctx context.Context) (tunnel.Path, error) {
				return tunnel.NewDialPath("lan", []string{addr}, 1200*time.Millisecond), nil
			},
		})
	}

	// 公网映射直连（#22/#25）
	if sig.PubTCP != "" {
		fast = append(fast, tunnel.Plan{
			Name: "pubtcp", Budget: 2500 * time.Millisecond,
			Make: func(ctx context.Context) (tunnel.Path, error) {
				return tunnel.NewDialPath("pubtcp", []string{sig.PubTCP}, 2*time.Second), nil
			},
		})
	}

	// IPv6 直连（#28 多地址候选并发拨）
	var v6addrs []string
	if len(sig.V6List) > 0 {
		v6addrs = sig.V6List
	} else if sig.V6 != "" {
		v6addrs = []string{net.JoinHostPort(sig.V6, strconv.Itoa(sig.V6Port))}
	}
	if len(v6addrs) > 0 && netprobe.HasIPv6Route() {
		fast = append(fast, tunnel.Plan{
			Name: "v6", Budget: 3500 * time.Millisecond,
			Make: func(ctx context.Context) (tunnel.Path, error) {
				return tunnel.NewDialPath("v6", v6addrs, 3*time.Second), nil
			},
		})
	}

	// 竞速层预算（#19 矩阵简化版）：
	//   EIM×EIM → 打洞必成率高，WebRTC 45s
	//   一侧对称（nonEIM）→ 打洞难，WebRTC 压 20s，中继前置意义不大但并行
	//   unknown → 45s
	webRTCBoot := 45 * time.Second
	if g.natCls == "nonEIM" || sig.NAT == "nonEIM" {
		webRTCBoot = 20 * time.Second
	}
	// #18 双向预检：STUN 全不可达（出站 UDP 受限）→ 打洞没戏，压到 8s
	if g.natCls == "unknown" {
		webRTCBoot = 8 * time.Second
	}

	race = append(race, tunnel.Plan{
		Name: "webrtc", Budget: webRTCBoot,
		Make: func(ctx context.Context) (tunnel.Path, error) {
			return g.makeWebRTCPath(ctx, sig, epoch)
		},
	})

	// 自建中继（#31）
	if len(g.cfg.RelayAddrs) > 0 {
		race = append(race, tunnel.Plan{
			Name: "relay", Budget: 10 * time.Second,
			Make: func(ctx context.Context) (tunnel.Path, error) {
				return g.makeRelayPath(ctx)
			},
		})
	}
	return fast, race
}

// makeWebRTCPath WebRTC 打洞路径（Tier 2）。
func (g *Guest) makeWebRTCPath(ctx context.Context, sig *mailbox.Signal, epoch int) (tunnel.Path, error) {
	pc, err := p2p.NewGuest(p2p.Config{STUN: g.cfg.STUN, TURN: g.cfg.TURN})
	if err != nil {
		return nil, fmt.Errorf("WebRTC 初始化失败: %w", err)
	}
	p := &webrtcPath{g: g, pc: pc, sig: sig, epoch: epoch, dead: make(chan struct{}), deadOne: sync.Once{}}
	if err := p.Establish(ctx); err != nil {
		pc.Stop()
		return nil, err
	}
	return p, nil
}

// webrtcPath tunnel.Path 实现（Tier 2）。
type webrtcPath struct {
	g       *Guest
	pc      *p2p.Guest
	sig     *mailbox.Signal
	epoch   int
	dead    chan struct{}
	deadOne sync.Once
}

func (p *webrtcPath) Name() string          { return "webrtc" }
func (p *webrtcPath) Dead() <-chan struct{} { return p.dead }
func (p *webrtcPath) Close()                { p.deadOne.Do(func() { close(p.dead) }); p.pc.Stop() }

func (p *webrtcPath) RTT() time.Duration {
	return time.Duration(p.pc.RTT()) * time.Millisecond
}

// Establish 收 Offer → 回 Answer → 注册信箱 → 等 ctrl 打通。
func (p *webrtcPath) Establish(ctx context.Context) error {
	if err := p.pc.AcceptOffer(p.sig.SDPType, p.sig.SDP); err != nil {
		return fmt.Errorf("Offer 无效: %w", err)
	}
	aType, aSDP, err := p.pc.Answer()
	if err != nil {
		return fmt.Errorf("Answer 生成失败: %w", err)
	}
	if err := p.g.registerAnswer(p.epoch, "webrtc", aType, aSDP); err != nil {
		if errors.Is(err, mailbox.ErrCodeTaken) {
			return errors.New("加入排队中（有其他小伙伴正在加入），即将自动重试")
		}
		return err
	}
	select {
	case <-p.pc.Established():
		// 监听断线 → Dead
		go func() {
			select {
			case <-p.pc.Dead():
				p.Close()
			case <-p.dead:
			}
		}()
		return nil
	case <-time.After(45 * time.Second):
		return errors.New("打洞超时：网络环境复杂，建议切换手机热点或配置自建中继节点")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Open 为一条本地 MC 连接开 DataChannel。
func (p *webrtcPath) Open() (net.Conn, error) {
	return p.pc.Open()
}

// makeRelayPath 自建中继路径（Tier 3，ROADMAP #31-33）。
func (g *Guest) makeRelayPath(ctx context.Context) (tunnel.Path, error) {
	// #38 多节点选址（并发 ping 取 RTT 中位数最小者）
	server, conn, err := relay.SelectRelay(g.cfg.RelayPSK, g.cfg.Code, relay.RoleGuest, g.cfg.RelayAddrs)
	if err != nil {
		return nil, err
	}
	ep := rudp.Wrap(conn)
	tag := relay.DeriveTag(g.cfg.RelayPSK, g.cfg.Code)
	key := relayKey(g.cfg.RelayPSK, g.cfg.Code)
	stream, err := ep.Open(server, tag, key)
	if err != nil {
		ep.Close()
		return nil, err
	}
	stop := make(chan struct{})
	go relay.KeepaliveLoop(conn, server, tag, g.cfg.RelayPSK, g.cfg.Code, relay.RoleGuest, stop)
	p := &relayPath{
		g: g, ep: ep, stream: stream, server: server, tag: tag,
		mux:  tunnel.NewMuxer(stream),
		stop: stop,
		dead: make(chan struct{}), deadOne: sync.Once{},
	}
	// 中继流死亡（解密失败/超时）→ Dead → 调度器重赛
	go func() {
		<-stream.Dead()
		p.Close()
	}()
	return p, nil
}

// relayPath tunnel.Path 实现（Tier 3）：单 rudp 流 + 内层 2B 通道 ID 多路复用
// （ROADMAP #52 统一 socket：一条流承载全部 MC 连接）。
type relayPath struct {
	g       *Guest
	ep      *rudp.Endpoint
	stream  *rudp.Stream
	server  *net.UDPAddr
	tag     uint32
	mux     *tunnel.Muxer
	stop    chan struct{}
	dead    chan struct{}
	deadOne sync.Once

	mu         sync.Mutex
	nextConnID uint16
}

func (p *relayPath) Name() string          { return "relay" }
func (p *relayPath) Dead() <-chan struct{} { return p.dead }
func (p *relayPath) RTT() time.Duration    { return p.stream.RTT() }
func (p *relayPath) Close() {
	p.deadOne.Do(func() { close(p.dead) })
	p.mux.Close()
	p.ep.Close()
	close(p.stop)
}

// Establish 中继路径在 Make 里已完成（选节点+开流）。
func (p *relayPath) Establish(ctx context.Context) error { return nil }

// Open 内层多路复用开新通道（ROADMAP #52）。
func (p *relayPath) Open() (net.Conn, error) {
	p.mu.Lock()
	id := p.nextConnID
	if p.nextConnID < 65500 {
		p.nextConnID++
	}
	p.mu.Unlock()
	return p.mux.Channel(id), nil
}

// ---- 会话服务层 ----

// serve 路径就绪后：注册 Answer（如需要）→ 本地监听 → 逐连接桥接。
// 返回使用的 tier。
func (g *Guest) serve(path tunnel.Path, epoch int, localPort int) string {
	tier := path.Name()

	// 非 WebRTC 层也要注册 Answer（通知房主 + 占槽防并发）
	if tier != "webrtc" {
		if rerr := g.registerAnswer(epoch, tier, "", ""); rerr != nil && !errors.Is(rerr, mailbox.ErrCodeTaken) {
			path.Close()
			return ""
		}
	}

	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(localPort))
	if err != nil {
		g.emit(Event{Stage: "error", Msg: fmt.Sprintf("本地端口 %d 被占用？%v", localPort, err)})
		path.Close()
		return ""
	}

	g.mu.Lock()
	g.tier = tier
	g.addr = "127.0.0.1:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	g.mu.Unlock()

	ready := int32(1) // mcproto 门控（#65）：路径就绪 → 透传
	tierName := map[string]string{
		"lan": "局域网直连", "pubtcp": "公网直连（UPnP）", "v6": "IPv6 极速直连",
		"webrtc": "P2P 打洞", "relay": "中继",
	}[tier]
	rtt := path.RTT()
	g.emit(Event{Stage: "connected", Tier: tier, Addr: g.addr, RTT: rtt.Milliseconds(),
		Msg: "连接成功（" + tierName + "）！"})
	g.emit(Event{Stage: "info", Msg: "打开游戏 → 多人游戏 → 直接连接 → 粘贴 " + g.addr})

	// 掉线感知：路径死亡 → 断开本地监听 → 外层重连
	pathDead := path.Dead()
	go func() {
		select {
		case <-pathDead:
			_ = ln.Close()
		case <-g.ctx.Done():
			_ = ln.Close()
		}
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			path.Close()
			return tier // 外层 loop 重连（#58 记忆在下次生效）
		}
		go g.handleLocalConn(conn, path, &ready)
	}
}

// handleLocalConn 单条 MC 连接：门控（#65）→ 路径 Open → 双向泵。
func (g *Guest) handleLocalConn(conn net.Conn, path tunnel.Path, ready *int32) {
	defer conn.Close()

	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true) // #53
	}

	// #65 协议感知门控：未就绪时对 server list ping 代答 MOTD
	decision, consumed, err := mcproto.Gate(conn, atomic.LoadInt32(ready) == 1)
	if err == nil {
		switch decision {
		case mcproto.AnswerStatus:
			mcproto.ReplyStatus(conn, "CLink 隧道建立中… 请稍候重试")
			return
		case mcproto.AnswerReject:
			mcproto.ReplyReject(conn, "CLink 隧道建立中… 请稍候重试")
			return
		}
		// Passthrough：把已消费的首包先回灌对端
		if len(consumed) > 0 {
			remote, oerr := path.Open()
			if oerr != nil {
				return
			}
			defer remote.Close()
			if tc, ok := remote.(*net.TCPConn); ok {
				_ = tc.SetNoDelay(true) // #53
			}
			if _, werr := remote.Write(consumed); werr != nil {
				return
			}
			bridgeBoth(conn, remote)
			return
		}
	}

	remote, oerr := path.Open()
	if oerr != nil {
		return
	}
	defer remote.Close()
	if tc, ok := remote.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true) // #53
	}
	bridgeBoth(conn, remote)
}

// bridgeBoth 双向泵。
func bridgeBoth(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
}

// ---- 信箱交互（原逻辑保留）----

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
		NAT:     g.natCls, // #62/#19：NAT 类别随信令交换
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

// ---- 信箱交互（原逻辑保留）----

// registerAnswer 写 Answer 信箱 + 注册槽位。
