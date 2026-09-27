// Package updater 在线更新检查。
//
// 更新源：website/update.json（部署在 clink.541887.xyz，raw.githubusercontent 备用）。
// 发布编号（build）单调递增；客户端比较 build 判断是否有新版。
// 通知去重：仅当 远端 build > 本地 build 且 > 已通知过的 build 时弹通知，
// 记录持久化在 config（LastNotifiedBuild），同一发布只通知一次（重启也不重复）。
package updater

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Update update.json 载荷。
type Update struct {
	Version  string `json:"version"`  // "1.2.0"
	Build    int    `json:"build"`    // 发布编号（1,2,3…单调递增）
	Notes    string `json:"notes"`    // 一句话更新说明
	Date     string `json:"date"`     // 发布日期
	Download string `json:"download"` // exe 直链
	Page     string `json:"page"`     // 发布页
}

// UpdateInfo 推给 UI 的检查结果。
type UpdateInfo struct {
	CurrentVersion string  `json:"currentVersion"`
	LatestVersion  string  `json:"latestVersion"`
	CurrentBuild   int     `json:"currentBuild"`
	LatestBuild    int     `json:"latestBuild"`
	Notes          string  `json:"notes"`
	Download       string  `json:"download"`
	Page           string  `json:"page"`
	HasUpdate      bool    `json:"hasUpdate"`
	FetchedAt      string  `json:"fetchedAt"`
}

// DefaultURLs 更新源：主域名 + GitHub raw 备份。
func DefaultURLs() []string {
	return []string{
		"https://clink.541887.xyz/update.json",
		"https://raw.githubusercontent.com/ccdsyy/CLink/main/website/update.json",
	}
}

var client = &http.Client{Timeout: 12 * time.Second}

// Check 依次尝试更新源，返回最新 Update。
func Check(urls []string) (*Update, error) {
	if len(urls) == 0 {
		urls = DefaultURLs()
	}
	var lastErr error
	for _, u := range urls {
		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", "CLink-Updater")
		req.Header.Set("Cache-Control", "no-cache")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != 200 {
			lastErr = fmt.Errorf("%s → HTTP %d", u, resp.StatusCode)
			continue
		}
		var up Update
		if err := json.Unmarshal(b, &up); err != nil {
			lastErr = fmt.Errorf("%s → JSON 解析失败: %w", u, err)
			continue
		}
		if up.Version == "" || up.Build <= 0 {
			lastErr = fmt.Errorf("%s → 缺少 version/build", u)
			continue
		}
		return &up, nil
	}
	if lastErr == nil {
		lastErr = errors.New("无可用更新源")
	}
	return nil, lastErr
}
