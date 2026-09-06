// Package mclog 监听 Minecraft 日志，自动捕获"对局域网开放"的端口。
package mclog

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"time"
)

var (
	// 客户端"对局域网开放"（1.13+ 日志）
	lanPortRe = regexp.MustCompile(`Local game hosted on port (\d{1,5})`)
	// 服务端启动日志（dedicated server）
	serverPortRe = regexp.MustCompile(`Starting Minecraft server on (?:\*|[0-9.]+|\[[0-9a-fA-F:]+\]):(\d{1,5})`)
)

// DefaultCandidates 按优先级列出可能的 latest.log 位置。
func DefaultCandidates() []string {
	var out []string
	if ad := os.Getenv("APPDATA"); ad != "" { // Windows 官方启动器
		out = append(out, filepath.Join(ad, ".minecraft", "logs", "latest.log"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".minecraft", "logs", "latest.log"))
	}
	if runtime.GOOS == "windows" {
		// 整合包/便携启动器常见：游戏目录 = 工作目录
		out = append(out, filepath.Join(".", "logs", "latest.log"))
	}
	return out
}

// FindLatest 返回第一个存在的日志路径（空串 = 未找到）。
func FindLatest(candidates []string) string {
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// ScanPort 全量读取日志，返回最后记录的 MC 端口（0 = 未找到）。
func ScanPort(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	port := 0
	for _, m := range lanPortRe.FindAllSubmatch(data, -1) {
		if p, e := strconv.Atoi(string(m[1])); e == nil {
			port = p
		}
	}
	for _, m := range serverPortRe.FindAllSubmatch(data, -1) {
		if p, e := strconv.Atoi(string(m[1])); e == nil {
			port = p
		}
	}
	return port
}

// Watch 增量轮询日志（1 秒），发现新端口即回调 onPort。
// 日志被截断/重建（游戏重启）时自动从头读。
func Watch(ctx context.Context, path string, onPort func(port int)) {
	lastSize := int64(0)
	lastPort := ScanPort(path)
	if st, err := os.Stat(path); err == nil {
		lastSize = st.Size()
	}

	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		st, err := os.Stat(path)
		if err != nil || st.IsDir() {
			continue
		}
		if st.Size() < lastSize {
			lastSize = 0 // 日志轮转，重新读
		}
		if st.Size() == lastSize {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		_, _ = f.Seek(lastSize, io.SeekStart)
		chunk, err := io.ReadAll(f)
		_ = f.Close()
		if err != nil {
			continue
		}
		lastSize += int64(len(chunk))

		for _, m := range lanPortRe.FindAllSubmatch(chunk, -1) {
			if p, e := strconv.Atoi(string(m[1])); e == nil && p != lastPort {
				lastPort = p
				onPort(p)
			}
		}
		for _, m := range serverPortRe.FindAllSubmatch(chunk, -1) {
			if p, e := strconv.Atoi(string(m[1])); e == nil && p != lastPort {
				lastPort = p
				onPort(p)
			}
		}
	}
}
