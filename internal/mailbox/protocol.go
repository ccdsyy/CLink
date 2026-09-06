// Package mailbox 实现 CLink 信令信箱协议（两级信箱接力）。
//
// 内容信箱：Clipzy 粉贴（AES-256-GCM 密文存取，服务端只见密文）
// 寻址层：  Clipzy 短链（customCode = 房间码槽位 → 信箱 id 跳转 URL）
//
// 端点行为均于 2026-09-04/06 实测验证，详见 docs/PROTOCOL.md。
package mailbox

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	// ProtoVersion 当前协议版本
	ProtoVersion = 1

	// CodeLen 房间码长度
	CodeLen = 6

	// 槽位角色后缀（挂在房间码第 8 位）
	RoleOffer  = 'h' // host → guest：房间信息 / Offer
	RoleAnswer = 'g' // guest → host：Answer / 加入通知

	// DefaultTTL 信箱默认留存（6 小时，覆盖一整局游戏；服务端上限约 30 天）
	DefaultTTL = 21600

	// RoomMaxAge 房间有效期窗口（Signal.Ts 超出视为过期房间）
	RoomMaxAge = 24 * time.Hour

	// MaxEpoch 最大轮转次数（槽位 8 字符 = 房码 6 + 轮次 1 + 角色 1）
	MaxEpoch = 31

	// GitHubPagesRedirect 短链指向的中转页（Clipzy 要求合法 http(s) URL）
	GitHubPagesRedirect = "https://ccdsyy.github.io/r/"
)

// Alphabet 防混淆字符表：小写字母+数字，去掉 0/1/i/l/o，
// 与 Clipzy customCode 校验规则严格对齐（实测：含这些字符直接 400）。
const Alphabet = "abcdefghjkmnpqrstuvwxyz23456789" // 31 个字符

// ErrInvalidRoomCode 房间码格式错误
var ErrInvalidRoomCode = errors.New("房间码格式不正确（6 位，不含 0/1/i/l/o）")

// Signal 信令载荷：信箱中传输的明文结构（上链前加密）。
type Signal struct {
	V       int    `json:"v"`                // 协议版本
	Type    string `json:"type"`             // "offer" | "answer"
	Epoch   int    `json:"epoch"`            // 房间轮次（每完成一人加入 +1，兼容重连）
	Tier    string `json:"tier"`             // "ipv6" | "webrtc" | "dual"
	Nonce   string `json:"nonce"`            // 随机数，防重放
	Ts      int64  `json:"ts"`               // 签发时间（Unix 秒）
	SDP     string `json:"sdp,omitempty"`    // WebRTC SDP
	SDPType string `json:"sdpType,omitempty"`
	V6      string `json:"v6,omitempty"`     // 房主公网 IPv6
	V6Port  int    `json:"v6Port,omitempty"` // 房主 IPv6 监听端口
	MCPort  int    `json:"mcPort"`           // 房主 MC 局域网端口
	Name    string `json:"name,omitempty"`   // 玩家昵称（UI 显示）
}

// GenRoomCode 生成 6 位防混淆房间码（拒绝采样消除模偏差）。
func GenRoomCode() string {
	max := 256 - (256 % len(Alphabet))
	out := make([]byte, CodeLen)
	for i := 0; i < CodeLen; i++ {
		for {
			b := make([]byte, 1)
			if _, err := rand.Read(b); err != nil {
				// crypto/rand 失败极少见；退化为时间熵
				out[i] = Alphabet[int(time.Now().UnixNano())%len(Alphabet)]
				break
			}
			if int(b[0]) < max {
				out[i] = Alphabet[int(b[0])%len(Alphabet)]
				break
			}
		}
	}
	return string(out)
}

// ValidateRoomCode 校验房间码：6 位、全部字符在防混淆字母表内。
func ValidateRoomCode(code string) bool {
	if len(code) != CodeLen {
		return false
	}
	for i := 0; i < len(code); i++ {
		if !strings.ContainsRune(Alphabet, rune(code[i])) {
			return false
		}
	}
	return true
}

// SlotCode 由房间码、轮次与角色计算 8 位信箱槽位码。
func SlotCode(room string, epoch int, role byte) string {
	if epoch < 0 || epoch >= len(Alphabet) {
		epoch = len(Alphabet) - 1
	}
	return room + string(Alphabet[epoch]) + string(role)
}

// Nonce 生成随机 nonce（base64，12 字节熵）。
func Nonce() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return base64.RawStdEncoding.EncodeToString(b)
}

// IDFromRedirectURL 从短链目标 URL 中解析信箱 id（形如 https://host/r/?id=xxx）。
func IDFromRedirectURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("槽位指向的 URL 无法解析: %w", err)
	}
	id := u.Query().Get("id")
	if id == "" {
		return "", fmt.Errorf("槽位 URL 中缺少信箱 id")
	}
	return id, nil
}

// Validate 校验信令的时间窗与类型。
func (s *Signal) Validate(wantType string) error {
	if s.V > ProtoVersion {
		return fmt.Errorf("信令版本过新（v%d），请升级 CLink", s.V)
	}
	if s.Type != wantType {
		return fmt.Errorf("信令类型不符（期望 %s，实为 %s）", wantType, s.Type)
	}
	if time.Since(time.Unix(s.Ts, 0)) > RoomMaxAge {
		return errors.New("房间已过期（超过 24 小时）")
	}
	if s.Ts > time.Now().Add(10*time.Minute).Unix() {
		return errors.New("信令时间异常（时钟偏差过大）")
	}
	return nil
}
