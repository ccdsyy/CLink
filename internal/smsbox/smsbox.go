// Package smsbox 接入用户自部署的短信盒子（安卓 APK 短信网关）。
//
// 盒子接口（sms-box-apk BoxServer.kt）：
//   - GET  /api/ping  免认证健康检查
//   - POST /api/send  JSON {"phones":["138…"],"text":"…"}，认证 X-PIN 头
//     （盒子未设置 PIN 时免认证）
//
// 防轰炸内置（CLink 侧）：
//   - 同 key（如 join-小明）60 秒去抖
//   - 全局每小时最多 20 条，超限静默丢弃（HourlyLimited 可查）
package smsbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	pingTimeout   = 3 * time.Second
	sendTimeout   = 8 * time.Second
	debounce      = 60 * time.Second // 同 key 去抖窗口
	hourlyMax     = 20               // 每小时全局上限
)

// Client 短信盒子客户端（线程安全）。
type Client struct {
	mu   sync.RWMutex
	url  string // 形如 http://192.168.1.5:8080（不带尾部斜杠）
	pin  string
	hc   *http.Client
}

// New 创建客户端。url 为盒子根地址（含端口），pin 可为空。
func New(url, pin string) *Client {
	return &Client{
		url: strings.TrimRight(strings.TrimSpace(url), "/"),
		pin: strings.TrimSpace(pin),
		hc:  &http.Client{Timeout: sendTimeout},
	}
}

// SetConfig 运行时更新配置。
func (c *Client) SetConfig(url, pin string) {
	c.mu.Lock()
	c.url = strings.TrimRight(strings.TrimSpace(url), "/")
	c.pin = strings.TrimSpace(pin)
	c.mu.Unlock()
}

// Ready 是否已配置。
func (c *Client) Ready() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.url != ""
}

// Ping 健康检查（免认证，快超时）。
func (c *Client) Ping() error {
	c.mu.RLock()
	url, pin := c.url, c.pin
	c.mu.RUnlock()
	if url == "" {
		return errors.New("未配置盒子地址")
	}
	hc := &http.Client{Timeout: pingTimeout}
	req, err := http.NewRequest("GET", url+"/api/ping", nil)
	if err != nil {
		return err
	}
	if pin != "" {
		req.Header.Set("X-PIN", pin)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("连不上盒子（检查地址与同一网络）: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("盒子返回 HTTP %d", resp.StatusCode)
	}
	return nil
}

// Send 发送短信（POST /api/send）。phones 1-10 个。
func (c *Client) Send(phones []string, text string) error {
	c.mu.RLock()
	url, pin := c.url, c.pin
	c.mu.RUnlock()
	if url == "" {
		return errors.New("未配置盒子地址")
	}
	if len(phones) == 0 {
		return errors.New("没有手机号")
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("内容为空")
	}
	if len(phones) > 10 {
		phones = phones[:10]
	}
	body, _ := json.Marshal(map[string]any{"phones": phones, "text": text})
	req, err := http.NewRequest("POST", url+"/api/send", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if pin != "" {
		req.Header.Set("X-PIN", pin)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("发送失败（盒子不在线？）: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode != 200 {
		return fmt.Errorf("盒子 HTTP %d: %s", resp.StatusCode, truncate(string(b), 120))
	}
	var out struct {
		OK      bool     `json:"ok"`
		Results []string `json:"results"`
	}
	if json.Unmarshal(b, &out) == nil && !out.OK {
		return fmt.Errorf("盒子拒绝: %s", truncate(strings.Join(out.Results, ";"), 120))
	}
	return nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ---- 限流器（防轰炸）----

// Limiter 事件级去抖 + 全局每小时上限。
type Limiter struct {
	mu     sync.Mutex
	last   map[string]time.Time // key → 上次放行时间
	window []time.Time          // 全局放行时间窗（清理 >1h 的）
}

// NewLimiter 创建限流器。
func NewLimiter() *Limiter {
	return &Limiter{last: make(map[string]time.Time)}
}

// Allow 判断 key 是否放行（放行则记录）。
func (l *Limiter) Allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if t, ok := l.last[key]; ok && now.Sub(t) < debounce {
		return false
	}
	// 清理 1 小时前的窗口
	kept := l.window[:0]
	for _, t := range l.window {
		if now.Sub(t) < time.Hour {
			kept = append(kept, t)
		}
	}
	l.window = kept
	if len(l.window) >= hourlyMax {
		return false
	}

	l.last[key] = now
	l.window = append(l.window, now)
	// last 防膨胀
	if len(l.last) > 128 {
		l.last = make(map[string]time.Time)
		l.last[key] = now
	}
	return true
}
