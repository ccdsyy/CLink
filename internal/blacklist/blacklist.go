// Package blacklist 房间黑名单：持久化到 %APPDATA%/CLink/blacklist.json。
//
// 防攻击模型：
//   - 拉黑 = 按玩家昵称拒绝（信令层 ans.Name 精确匹配，大小写不敏感）
//   - 房主踢人时可选"同时拉黑"，被拉黑者此后任何 epoch 都无法加入
//   - 数据跨会话持久，重开房也生效
package blacklist

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store 黑名单存储（线程安全）。
type Store struct {
	mu   sync.RWMutex
	path string
	m    map[string]Entry // key = strings.ToLower(name)
}

// Entry 单条黑名单记录。
type Entry struct {
	Name    string `json:"name"`
	Reason  string `json:"reason,omitempty"`
	AddedAt int64  `json:"added_at"`
}

// Load 从默认路径（或 path 覆盖）加载黑名单；文件不存在则返回空表。
func Load(path string) (*Store, error) {
	if path == "" {
		path = DefaultPath()
	}
	s := &Store{path: path, m: make(map[string]Entry)}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	var entries []Entry
	if err := json.Unmarshal(b, &entries); err != nil {
		return s, nil // 文件损坏则从空表开始，不影响使用
	}
	for _, e := range entries {
		s.m[strings.ToLower(e.Name)] = e
	}
	return s, nil
}

// DefaultPath %APPDATA%/CLink/blacklist.json（非 Windows 用 ~/.config/CLink/）。
func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "CLink", "blacklist.json")
}

// Has 判断昵称是否被拉黑（大小写不敏感）。
func (s *Store) Has(name string) bool {
	if strings.TrimSpace(name) == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.m[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// Add 拉黑（幂等；重复添加刷新时间与理由）。
func (s *Store) Add(name, reason string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	s.mu.Lock()
	s.m[strings.ToLower(name)] = Entry{Name: name, Reason: reason, AddedAt: time.Now().Unix()}
	s.mu.Unlock()
	return s.save()
}

// Remove 解除拉黑。
func (s *Store) Remove(name string) error {
	s.mu.Lock()
	delete(s.m, strings.ToLower(strings.TrimSpace(name)))
	s.mu.Unlock()
	return s.save()
}

// List 返回全部记录（按加入时间倒序）。
func (s *Store) List() []Entry {
	s.mu.RLock()
	out := make([]Entry, 0, len(s.m))
	for _, e := range s.m {
		out = append(out, e)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].AddedAt > out[j].AddedAt })
	return out
}

func (s *Store) save() error {
	s.mu.RLock()
	entries := make([]Entry, 0, len(s.m))
	for _, e := range s.m {
		entries = append(entries, e)
	}
	path := s.path
	s.mu.RUnlock()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
