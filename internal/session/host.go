package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ccdsyy/clink/internal/blacklist"
	"github.com/ccdsyy/clink/internal/lanscan"
	"github.com/ccdsyy/clink/internal/mailbox"
	"github.com/ccdsyy/clink/internal/netprobe"
	"github.com/ccdsyy/clink/internal/portmap"
	"github.com/ccdsyy/clink/internal/relay"
	"github.com/ccdsyy/clink/internal/rudp"
	"github.com/ccdsyy/clink/internal/tunnel"
	"github.com/ccdsyy/clink/internal/tunnel/p2p"
	"github.com/ccdsyy/clink/internal/tunnel/v6direct"
)

// HostConfig 房主配置。
type HostConfig struct {
	MCPort     int
	STUN       []string
	TURN       []string
	Mailbox    *mailbox.Client
	Blacklist  *blacklist.Store // 可为 nil（无黑名单）
	RelayAddrs []*net.UDPAddr   // 自建中继节点（ROADMAP #31，空=不启用）
	RelayPSK   string
}

// Host 房主会话状态机。
//
// 生命周期：StartHost → 探测 v6 → 起 v6 监听 + p2p 端点 → 写 Offer 信箱+槽位
// → 轮询 Answer 槽 → 客机接入 → epoch+1 写新 Offer（支持下一位玩家）→ 循环。
type Host struct {
	ctx    context.Context
	cancel context.CancelFunc
	cfg    HostConfig
	emit   func(Event)

	code  string
	epoch int

	v6      *v6direct.Host
	v6Addrs []string // 全部公网 v6 候选（含临时地址，ROADMAP #28）
	v6Addr  string
	v6Port  int
	pubTCP  string // UPnP/PCP 映射公网 TCP 端点（ROADMAP #22/#25）
	natCls  string // 本机 NAT 映射类别（EIM/nonEIM/unknown，ROADMAP #19）

	announce *lanscan.Announcer // LAN 组播声明（#66）

	mu      sync.Mutex
	pcs     map[string]*p2p.Host // 昵称 → 该客机的 WebRTC 端点
	members map[string]bool      // 在线成员（昵称去重）
}

// StartHost 启动房主会话（后台 goroutine 运行，事件经 emit 推送）。
func StartHost(ctx context.Context, cfg HostConfig, emit func(Event)) (*Host, error) {
	if cfg.MCPort <= 0 || cfg.MCPort > 65535 {
		return nil, fmt.Errorf("游戏端口无效（%d）", cfg.MCPort)
	}
	if cfg.Mailbox == nil {
		cfg.Mailbox = mailbox.NewClient("")
	}
	if len(cfg.STUN) == 0 {
		// 多节点探测选优，ICE 只挂最优单节点（多候选在 CGNAT 环境实测易失灵）
		cfg.STUN = []string{netprobe.PickSTUN()}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c, cancel := context.WithCancel(ctx)

	h := &Host{
		ctx:     c,
		cancel:  cancel,
		cfg:     cfg,
		emit:    emit,
		code:    mailbox.GenRoomCode(),
		pcs:     make(map[string]*p2p.Host),
		members: make(map[string]bool),
	}

	// Tier 1 准备：有公网 v6 就起直连监听（信令里同时保留 WebRTC 备用）
	if addrs := netprobe.PublicIPv6(); len(addrs) > 0 && netprobe.HasIPv6Route() {
		vh, port, err := v6direct.StartHost(c, cfg.MCPort)
		if err == nil {
			h.v6 = vh
			for _, a := range addrs { // ROADMAP #28：全候选广播（含临时隐私地址）
				h.v6Addrs = append(h.v6Addrs, a.String())
			}
			h.v6Addr, h.v6Port = h.v6Addrs[0], port
		}
	}

	// NAT 映射类别探测（ROADMAP #19 矩阵输入）+ Tier 1.5 UPnP/PCP 端口映射
	// （成功则信令携带公网 TCP 端点，客机可免穿透直连）
	go func() {
		h.natCls = netprobe.MappingClass()
		h.setupPortMapping()
	}()

	// LAN 组播声明（#66）：同一局域网的客机可直接秒连
	if h.v6 != nil {
		h.announce = lanscan.StartAnnounce(h.code, h.v6Port)
	}

	// 自建中继注册（#31/#38/#39）：常驻保活 + 接受经中继的入站流
	if len(cfg.RelayAddrs) > 0 {
		go h.relayLoop()
	}

	// 房主常驻自检（#59）：每 5 分钟刷新 NAT 类别与直连评分并广播
	go h.selfCheckLoop()

	go h.loop()
	return h, nil
}

// setupPortMapping 尝试 PCP/NAT-PMP/UPnP 把双栈隧道端口映射为公网
// （ROADMAP #22/23/24），并经公网回环拨号实测有效性（#25）。
// 成功后 Tier 1.5 生效：信令携带 PubTCP，客机免穿透直连。失败静默。
// 前置条件：v6Port 隧道监听已起（双栈 tcp），否则没有可映射的存活内网口。
func (h *Host) setupPortMapping() {
	if h.v6Port == 0 {
		return
	}
	res := portmap.Map(h.ctx, h.v6Port)
	if res == nil {
		return
	}
	ip := res.ExternalIP
	if ip == "" {
		ip, _ = netprobe.PublicV4()
	}
	if ip == "" {
		return
	}
	h.mu.Lock()
	h.pubTCP = net.JoinHostPort(ip, strconv.Itoa(res.ExternalPort))
	h.mu.Unlock()
	h.emit(Event{Stage: "info", Msg: "✨ 已通过 " + strings.ToUpper(res.Method) +
		" 建立公网直连通道" + map[bool]string{true: "（已实测可达）", false: ""}[res.Verified]})
}

// relayLoop 房主侧中继会话：注册（role=host）→ 保活 → 接受 Mux 通道桥接 MC。
// 断开后自动重选节点重连（#39 故障切换的房主侧）。
func (h *Host) relayLoop() {
	psk := h.cfg.RelayPSK
	tag := relay.DeriveTag(psk, h.code)
	key := relayKey(psk, h.code)

	for {
		if h.ctx.Err() != nil {
			return
		}
		server, conn, _, err := relay.SelectRelay(psk, h.code, relay.RoleHost, h.cfg.RelayAddrs)
		if err != nil {
			h.emit(Event{Stage: "info", Msg: "自建中继暂不可达（" + err.Error() + "），不影响直连"})
			select {
			case <-h.ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
			continue
		}
		ep := rudp.Wrap(conn)
		// tag2 接受器（ROADMAP #6 引路打洞）：客机经引路地址直接喷 KEEP
		// 过来，被动建流 → 独立 Muxer 服务 → 桥接 MC（数据不经中继）
		punchTag := relay.DeriveTagN(psk, h.code, 1)
		go func() {
			pstream, perr := ep.AcceptStream(punchTag, key, 0)
			if perr != nil {
				return
			}
			h.serveMuxStream(pstream)
		}()
		stream, err := ep.Open(server, tag, key)
		if err != nil {
			ep.Close()
			continue
		}
		stop := make(chan struct{})
		go relay.KeepaliveLoop(conn, server, tag, psk, h.code, relay.RoleHost, stop)
		h.emit(Event{Stage: "info", Msg: "📡 中继节点已就绪（" + relay.AddrString(server) + "），对称型网络的伙伴也能加入了"})

		mux := h.serveMuxStream(stream)

		// 任一侧死亡 → 重选节点
		select {
		case <-stream.Dead():
		case <-mux.Done():
		case <-h.ctx.Done():
			close(stop)
			ep.Close()
			return
		}
		close(stop)
		ep.Close()
	}
}

// serveMuxStream 在一条流上起 Muxer 服务：每条通道桥接本机 MC 端口。
func (h *Host) serveMuxStream(stream *rudp.Stream) *tunnel.Muxer {
	mux := tunnel.NewMuxerServer(stream)
	go func() {
		for {
			ch, err := mux.Accept()
			if err != nil {
				return
			}
			go func(ch *tunnel.MuxConn) {
				defer ch.Close()
				target, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", h.cfg.MCPort), 5*time.Second)
				if err != nil {
					return
				}
				if tc, ok := target.(*net.TCPConn); ok {
					_ = tc.SetNoDelay(true) // #53
				}
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(ch, target); done <- struct{}{} }()
				go func() { _, _ = io.Copy(target, ch); done <- struct{}{} }()
				<-done
			}(ch)
		}
	}()
	return mux
}

// selfCheckLoop 房主常驻自检（#59）：周期刷新直连评分并广播。
func (h *Host) selfCheckLoop() {
	score := func() int {
		s := 0
		if h.v6 != nil {
			s += 60
		}
		h.mu.Lock()
		pub := h.pubTCP
		nat := h.natCls
		h.mu.Unlock()
		if pub != "" {
			s += 30
		}
		if nat == "EIM" {
			s += 10
		}
		if s == 0 {
			s = 10 // 仅有打洞，保守估计
		}
		if s > 99 {
			s = 99
		}
		return s
	}
	h.emit(Event{Stage: "info", Msg: fmt.Sprintf("🏠 房间可直连概率约 %d%%（小伙伴加入基本零等待）", score())})
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-t.C:
		}
		h.natCls = netprobe.MappingClass()
		h.emit(Event{Stage: "info", Msg: fmt.Sprintf("🏠 房间可直连概率约 %d%%", score())})
	}
}

// relayKey 中继路径端到端加密密钥（#51 简化版）：中继只见密文。
func relayKey(psk, room string) []byte {
	key, _ := mailbox.DeriveKey(room + "|relay|" + psk)
	return key
}

// Code 房间码。
func (h *Host) Code() string { return h.code }

// Guests 已加入人数（以成员表为准，避免计数漂移）。
func (h *Host) Guests() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.members)
}

// Members 在线成员列表（昵称，去重排序）。
func (h *Host) Members() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.members))
	for n := range h.members {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Kick 踢出成员并可选拉黑（webrtc 层精确断开；被踢者会自动重连并被拒）。
func (h *Host) Kick(name string, ban bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	if ban && h.cfg.Blacklist != nil {
		_ = h.cfg.Blacklist.Add(name, "房主在房间内拉黑")
	}
	h.mu.Lock()
	pc := h.pcs[name]
	delete(h.pcs, name)
	_, was := h.members[name]
	delete(h.members, name)
	h.mu.Unlock()
	if pc != nil {
		pc.Stop() // 客机侧收到 Dead → 自动重连 → 黑名单拒绝
	}
	if !was {
		return // 本来就不在线，不重复广播
	}
	h.emit(Event{Stage: "guest-kicked", Name: name,
		Msg:    fmt.Sprintf("%s 已被%s", name, map[bool]string{true: "踢出并拉黑", false: "踢出房间"}[ban]),
		Guests: h.Guests()})
}

// HasV6 是否具备 IPv6 直连条件。
func (h *Host) HasV6() bool { return h.v6 != nil }

// Stop 结束房间。
func (h *Host) Stop() {
	h.cancel()
	h.mu.Lock()
	pcs := h.pcs
	h.pcs = make(map[string]*p2p.Host)
	h.members = make(map[string]bool)
	h.mu.Unlock()
	for _, p := range pcs {
		p.Stop()
	}
	if h.v6 != nil {
		h.v6.Stop()
	}
	h.announce.Stop()
}

func (h *Host) loop() {
	h.emit(Event{Stage: "created", Room: h.code, Msg: "房间已创建"})
	for {
		if h.ctx.Err() != nil {
			return
		}
		if err := h.runEpoch(); err != nil {
			if h.ctx.Err() != nil {
				return
			}
			h.emit(Event{Stage: "error", Msg: "信箱通信受阻：" + err.Error() + "，5 秒后自动重试"})
			select {
			case <-h.ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
}

// runEpoch 一个 epoch：挂出 Offer → 等一位客机接入 → 返回（loop 将进入下一 epoch）。
func (h *Host) runEpoch() error {
	// epoch 轮转上限
	if h.epoch >= mailbox.MaxEpoch {
		h.emit(Event{Stage: "info", Msg: "房间已达最大人数（31），如需更多请退出后重新开房"})
		<-h.ctx.Done()
		return nil
	}

	// #30 v6 地址轮换容忍：每 epoch 重新枚举公网 v6（临时隐私地址会轮换），
	// 新地址随本轮 Offer 广播，客机断线重连即拿到最新地址
	if h.v6 != nil {
		if addrs := netprobe.PublicIPv6(); len(addrs) > 0 {
			h.v6Addrs = h.v6Addrs[:0]
			for _, a := range addrs {
				h.v6Addrs = append(h.v6Addrs, a.String())
			}
			h.v6Addr = h.v6Addrs[0]
		}
	}

	// 1. WebRTC 端点（即使有 v6 也准备，双通道提高客机加入成功率）
	pc, err := p2p.NewHost(p2p.Config{STUN: h.cfg.STUN, TURN: h.cfg.TURN, MCPort: h.cfg.MCPort})
	if err != nil {
		return fmt.Errorf("WebRTC 初始化失败: %w", err)
	}
	sdpType, sdp, err := pc.Offer()
	if err != nil {
		pc.Stop()
		return fmt.Errorf("SDP 生成失败: %w", err)
	}

	// 2. 信令上链
	sig := mailbox.Signal{
		V:       mailbox.ProtoVersion,
		Type:    "offer",
		Epoch:   h.epoch,
		Tier:    "dual",
		Nonce:   mailbox.Nonce(),
		Ts:      time.Now().Unix(),
		SDP:     sdp,
		SDPType: sdpType,
		MCPort:  h.cfg.MCPort,
	}
	if h.v6 != nil {
		sig.V6, sig.V6Port = h.v6Addr, h.v6Port
		sig.V6List = h.v6Addrs // ROADMAP #28：全候选
	} else {
		sig.Tier = "webrtc"
	}
	h.mu.Lock()
	sig.PubTCP, sig.NAT = h.pubTCP, h.natCls // ROADMAP #22/#19：公网直连端点 + NAT 类别
	h.mu.Unlock()
	payload, err := json.Marshal(sig)
	if err != nil {
		pc.Stop()
		return err
	}
	key, err := mailbox.DeriveKey(h.code)
	if err != nil {
		pc.Stop()
		return err
	}
	sealed, err := mailbox.Seal(key, payload)
	if err != nil {
		pc.Stop()
		return err
	}
	id, err := h.cfg.Mailbox.Store(sealed, mailbox.DefaultTTL)
	if err != nil {
		pc.Stop()
		return fmt.Errorf("Offer 信箱写入失败: %w", err)
	}
	offerSlot := mailbox.SlotCode(h.code, h.epoch, mailbox.RoleOffer)
	if err := h.cfg.Mailbox.Shorten(mailbox.GitHubPagesRedirect+"?id="+id, offerSlot); err != nil {
		pc.Stop()
		if errors.Is(err, mailbox.ErrCodeTaken) {
			// 槽位已注册（上次重试残留/信箱恢复后重复）：
			// 该 epoch 已被毒化，轮转下一 epoch 而不是原地打转
			h.epoch++
			return nil
		}
		return fmt.Errorf("房间码注册失败: %w", err)
	}

	h.emit(Event{Stage: "waiting", Room: h.code, Guests: h.Guests(),
		Msg: "房间已就绪，把房间码发给小伙伴吧"})

	// 3. 轮询 Answer 槽（2 秒一次）
	answerSlot := mailbox.SlotCode(h.code, h.epoch, mailbox.RoleAnswer)
	var ans *mailbox.Signal
	for {
		if h.ctx.Err() != nil {
			pc.Stop()
			return nil
		}
		redirect, err := h.cfg.Mailbox.RedirectInfo(answerSlot)
		if err == nil {
			ansID, idErr := mailbox.IDFromRedirectURL(redirect)
			if idErr == nil {
				sealedAns, gErr := h.cfg.Mailbox.Get(ansID)
				if gErr == nil {
					if plain, oErr := mailbox.Open(key, sealedAns); oErr == nil {
						var s mailbox.Signal
						if jErr := json.Unmarshal(plain, &s); jErr == nil && s.Validate("answer") == nil && s.Epoch == h.epoch {
							ans = &s
							break
						}
					}
				}
			}
		}
		select {
		case <-h.ctx.Done():
			pc.Stop()
			return nil
		case <-time.After(2 * time.Second):
		}
	}

	// 4. 客机接入
	if guestName(ans.Name) != "小伙伴" && h.cfg.Blacklist != nil && h.cfg.Blacklist.Has(ans.Name) {
		// 黑名单命中：拒绝接入，直接进入下一 epoch 继续等下一位玩家
		pc.Stop()
		h.epoch++
		h.emit(Event{Stage: "guest-rejected", Name: ans.Name,
			Msg: fmt.Sprintf("已拒绝黑名单玩家 %s 的加入请求", ans.Name)})
		return nil
	}
	if ans.Tier == "ipv6" {
		// 客机走 IPv6 直连，无需 WebRTC
		pc.Stop()
		name := guestName(ans.Name)
		h.mu.Lock()
		h.members[name] = true
		h.mu.Unlock()
		h.emit(Event{Stage: "guest-joined", Name: name, Guests: h.Guests(), Tier: "ipv6",
			Msg: fmt.Sprintf("%s 已通过 IPv6 直连加入", name)})
	} else {
		if err := pc.AcceptAnswer(ans.SDPType, ans.SDP); err != nil {
			pc.Stop()
			return fmt.Errorf("Answer 接受失败: %w", err)
		}
		name := guestName(ans.Name)
		h.mu.Lock()
		old := h.pcs[name]
		h.pcs[name] = pc // 登记，供 Kick 精确断开
		h.mu.Unlock()
		if old != nil {
			old.Stop() // 同名重连：旧端点立即回收，防止双连接并存
		}

		// 等 ctrl 打通（45 秒）
		select {
		case <-pc.Established():
			h.mu.Lock()
			h.members[name] = true
			h.mu.Unlock()
			h.emit(Event{Stage: "guest-joined", Name: name, Guests: h.Guests(), Tier: "webrtc",
				Msg: fmt.Sprintf("%s 已通过 P2P 打洞加入", name)})
			go h.watchGuest(name, pc) // 掉线感知：成员列表实时准确
		case <-time.After(45 * time.Second):
			// 打洞失败：回收端点，避免僵尸连接挂着不动
			pc.Stop()
			h.mu.Lock()
			delete(h.pcs, name)
			h.mu.Unlock()
			h.emit(Event{Stage: "error", Guests: h.Guests(),
				Msg: "有小伙伴加入失败（打洞未完成）。对方网络可能较复杂，建议其切换手机热点后重新加入"})
		case <-h.ctx.Done():
			return nil
		}
	}

	// 5. 本轮完成 → 下一 epoch 挂新 Offer
	h.epoch++
	return nil
}

// watchGuest 监控已接入的 WebRTC 成员：断线即从成员表移除并广播 guest-left。
func (h *Host) watchGuest(name string, pc *p2p.Host) {
	select {
	case <-pc.Dead():
	case <-h.ctx.Done():
		return
	}
	h.mu.Lock()
	current := h.pcs[name] // 同名玩家可能已用新端点重连，只有旧端点才移除成员
	if current == pc {
		_, was := h.members[name]
		delete(h.members, name)
		delete(h.pcs, name)
		h.mu.Unlock()
		if !was {
			return
		}
		h.emit(Event{Stage: "guest-left", Name: name, Guests: h.Guests(),
			Msg: fmt.Sprintf("%s 已离开房间（连接断开）", name)})
		return
	}
	h.mu.Unlock()
}

func guestName(name string) string {
	if name == "" {
		return "小伙伴"
	}
	return name
}
