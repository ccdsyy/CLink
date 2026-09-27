// Package notify 封装 Windows 右下角系统通知（跨平台 fallback 静默）。
//
// 触发约定：仅当主窗口处于悬浮球模式或已最小化/隐藏时才弹系统通知，
// 避免用户正盯着窗口时重复打扰。是否弹由调用方（app 层）决定，
// 本包只负责发送与去抖。
package notify

import (
	"sync"
	"time"

	"github.com/gen2brain/beeep"
)

var (
	mu        sync.Mutex
	lastFired = make(map[string]int64) // key → unix 秒（同 key 3 秒去抖）
	enabled   = true
)

// SetEnabled 总开关（设置页可关）。
func SetEnabled(v bool) { mu.Lock(); enabled = v; mu.Unlock() }

// Enabled 当前开关状态。
func Enabled() bool { mu.Lock(); defer mu.Unlock(); return enabled }

// Send 弹出系统通知。
// 去抖：同 key 3 秒内只弹一次（防止 epoch 重扫导致重复加入事件）。
func Send(key, title, body string) {
	mu.Lock()
	if !enabled {
		mu.Unlock()
		return
	}
	now := time.Now().Unix()
	if t, ok := lastFired[key]; ok && now-t < 3 {
		mu.Unlock()
		return
	}
	lastFired[key] = now
	// map 防膨胀：超过 64 条清最旧的（简单起见直接重置）
	if len(lastFired) > 64 {
		lastFired = make(map[string]int64)
		lastFired[key] = now
	}
	mu.Unlock()

	_ = beeep.Notify(title, body, "")
}
