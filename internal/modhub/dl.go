// Package modhub 模组仓库：Modrinth 搜索 + 多线程分片下载（断点续传）。
//
// 下载引擎设计（dl.go）：
//   - HEAD 探测 Content-Length 与 Accept-Ranges
//   - 支持 Range → 按 threads 分片，每片独立 goroutine 拉取
//   - 分片落盘为 dest.clink-part.N（写入偏移 = 片起始 + 已有长度）
//   - 任务清单 dest.clink-dl.json 记录分片布局，进程重启后精确续传
//   - 全片完成 → 顺序合并 → 删除分片与清单
package modhub

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---- 下载引擎 ----

// Progress 下载进度（推给 UI）。
type Progress struct {
	ID       string `json:"id"`       // 任务标识（dest 路径哈希）
	Name     string `json:"name"`     // 文件名
	Dest     string `json:"dest"`     // 目标路径
	Done     int64  `json:"done"`     // 已下载字节
	Total    int64  `json:"total"`    // 总字节（未知为 -1）
	Speed    int64  `json:"speed"`    // 字节/秒（滑动窗口）
	Threads  int    `json:"threads"`  // 活跃线程数
	Finished bool   `json:"finished"` // 本任务完成
	Failed   bool   `json:"failed"`   // 失败
	Err      string `json:"err,omitempty"`
}

type chunk struct {
	Start int64  `json:"start"`
	End   int64  `json:"end"` // 含
	Name  string `json:"name"`
}

type manifest struct {
	URL     string  `json:"url"`
	Dest    string  `json:"dest"`
	Total   int64   `json:"total"`
	Chunks  []chunk `json:"chunks"`
	Etag    string  `json:"etag,omitempty"`
	Created int64   `json:"created"`
}

// Downloader 多线程下载器。
type Downloader struct {
	client *http.Client

	mu     sync.Mutex
	active map[string]bool          // dest → running（防重复提交）
	stops  map[string]chan struct{} // dest → 取消信号
	live   map[string]Progress      // dest → 最近一次进度快照

	onProgress func(Progress)
}

// NewDownloader 创建下载器（并发默认 8，上限 16）。
func NewDownloader(onProgress func(Progress)) *Downloader {
	return &Downloader{
		client:     &http.Client{Timeout: 0}, // 下载不超时，靠取消信号控制
		active:     make(map[string]bool),
		stops:      make(map[string]chan struct{}),
		live:       make(map[string]Progress),
		onProgress: onProgress,
	}
}

// Download 提交下载任务（异步）。重复提交同一 dest 会被忽略。
// threads ≤1 或服务器不支持 Range 时自动退化为单线程。
func (d *Downloader) Download(id, url, dest string, threads int) error {
	if url == "" || dest == "" {
		return errors.New("参数为空")
	}
	if threads <= 0 {
		threads = 8
	}
	if threads > 16 {
		threads = 16
	}
	abs, _ := filepath.Abs(dest)
	d.mu.Lock()
	if d.active[abs] {
		d.mu.Unlock()
		return nil // 已在跑
	}
	d.active[abs] = true
	stop := make(chan struct{})
	d.stops[abs] = stop
	d.mu.Unlock()

	go func() {
		defer func() {
			d.mu.Lock()
			delete(d.active, abs)
			delete(d.stops, abs)
			d.mu.Unlock()
		}()
		d.run(id, url, abs, threads, stop)
	}()
	return nil
}

// Cancel 取消任务（分片保留，下次 Download 同一目标自动续传）。
func (d *Downloader) Cancel(id string) {
	d.mu.Lock()
	stop, ok := d.stops[id]
	d.mu.Unlock()
	if ok {
		close(stop)
	}
}

// CancelAll 取消全部任务（应用退出时调用）。
func (d *Downloader) CancelAll() {
	d.mu.Lock()
	stops := d.stops
	d.stops = make(map[string]chan struct{})
	d.mu.Unlock()
	for _, s := range stops {
		close(s)
	}
}

// Jobs 当前活跃任务进度快照。
func (d *Downloader) Jobs() []Progress {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Progress, 0, len(d.live))
	for _, p := range d.live {
		out = append(out, p)
	}
	return out
}

// ResumeAll 扫描目录下的未完成任务清单，全部续传（供启动时调用）。
func (d *Downloader) ResumeAll(dir string, threads int) (int, error) {
	entries, err := filepath.Glob(filepath.Join(dir, "*.clink-dl.json"))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, mf := range entries {
		b, err := os.ReadFile(mf)
		if err != nil {
			continue
		}
		var m manifest
		if json.Unmarshal(b, &m) != nil || m.URL == "" || m.Dest == "" {
			os.Remove(mf)
			continue
		}
		if err := d.Download(jobID(m.Dest), m.URL, m.Dest, threads); err == nil {
			n++
		}
	}
	return n, nil
}

func (d *Downloader) run(id, url, dest string, threads int, stop <-chan struct{}) {
	name := filepath.Base(dest)
	emit := func(p Progress) {
		p.ID, p.Name, p.Dest = id, name, dest
		d.mu.Lock()
		if _, running := d.stops[dest]; running { // 已取消的任务不再覆盖进度
			d.live[dest] = p
		}
		d.mu.Unlock()
		if d.onProgress != nil {
			d.onProgress(p)
		}
	}
	fail := func(err error) {
		emit(Progress{Total: -1, Failed: true, Err: err.Error(), Threads: 0})
	}

	// 1. 探测
	total, rangesOK, err := probe(d.client, url)
	if err != nil {
		fail(fmt.Errorf("连接失败: %w", err))
		return
	}
	if total < 0 {
		fail(errors.New("服务器未返回文件大小"))
		return
	}
	if !rangesOK {
		threads = 1
	}

	// 2. 续传检查：已有 manifest 且与当前 URL/大小匹配
	mfPath := dest + ".clink-dl.json"
	var m *manifest
	if b, err := os.ReadFile(mfPath); err == nil {
		var old manifest
		if json.Unmarshal(b, &old) == nil && old.URL == url && old.Total == total && len(old.Chunks) > 0 {
			m = &old
		}
	}
	if m == nil {
		m = newManifest(url, dest, total, threads)
	}

	// 3. 清理孤儿分片（布局变化时）
	keep := make(map[string]bool, len(m.Chunks))
	for _, c := range m.Chunks {
		keep[c.Name] = true
	}
	if ents, _ := os.ReadDir(filepath.Dir(dest)); ents != nil {
		base := filepath.Base(dest) + ".clink-part."
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), base) && !keep[e.Name()] {
				os.Remove(filepath.Join(filepath.Dir(dest), e.Name()))
			}
		}
	}

	// 4. 分片下载
	prog := &counter{total: total}
	prog.resumeExisting(m, dest) // 续传：统计已完成分片

	errCh := make(chan error, len(m.Chunks))
	var wg sync.WaitGroup
	speed := newSpeedWindow()

	// 进度上报协程（500ms 一次）
	stopReport := make(chan struct{})
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				dn, tt := prog.snapshot()
				emit(Progress{Done: dn, Total: tt, Speed: speed.rate(),
					Threads: len(m.Chunks) - completedChunks(m, dest)})
			case <-stopReport:
				close(done)
				return
			}
		}
	}()

	for i := range m.Chunks {
		c := &m.Chunks[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.fetchChunk(url, dest, c, prog, speed, stop); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(stopReport)
	<-done
	select {
	case <-stop: // 用户取消：分片保留供续传
		dn, tt := prog.snapshot()
		emit(Progress{Done: dn, Total: tt, Failed: true, Err: "已取消（进度已保留，重新添加即可续传）"})
		return
	case err := <-errCh:
		_ = saveManifest(mfPath, m) // 保存现场供续传
		fail(fmt.Errorf("下载中断: %w（重新添加同一文件即可续传）", err))
		return
	default:
	}

	// 5. 合并
	if err := mergeChunks(m, dest); err != nil {
		fail(fmt.Errorf("合并失败: %w", err))
		return
	}
	os.Remove(mfPath)
	emit(Progress{Done: total, Total: total, Speed: 0, Threads: 0, Finished: true})
}

// fetchChunk 下载单个分片（支持片内续传）。
// 关键：必须始终带 Range 头（从分片起始偏移开始），否则服务器返回全量数据。
func (d *Downloader) fetchChunk(url, dest string, c *chunk, prog *counter, sp *speedWindow, stop <-chan struct{}) error {
	partPath := filepath.Join(filepath.Dir(dest), c.Name)
	need := c.End - c.Start + 1

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ { // 每片最多重试 3 次
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
		have := int64(0)
		if st, err := os.Stat(partPath); err == nil {
			have = st.Size() // 重试时重新统计（此前可能已写入一部分）
		}
		if have >= need {
			return nil // 分片已完成
		}

		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", c.Start+have, c.End))
		req.Header.Set("User-Agent", "CLink/1.1 (github.com/ccdsyy/CLink)")
		resp, err := d.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		}

		var f *os.File
		if have > 0 {
			f, err = os.OpenFile(partPath, os.O_WRONLY|os.O_APPEND, 0o644)
		} else {
			f, err = os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		}
		if err != nil {
			resp.Body.Close()
			return err
		}

		buf := make([]byte, 64*1024)
		for {
			select {
			case <-stop: // 取消：保留已写分片供续传
				f.Close()
				resp.Body.Close()
				return errors.New("已取消")
			default:
			}
			n, rErr := resp.Body.Read(buf)
			if n > 0 {
				if _, wErr := f.Write(buf[:n]); wErr != nil {
					f.Close()
					resp.Body.Close()
					return wErr
				}
				have += int64(n)
				prog.add(int64(n))
				sp.add(int64(n))
			}
			if rErr == io.EOF {
				f.Close()
				resp.Body.Close()
				return nil
			}
			if rErr != nil {
				f.Close()
				resp.Body.Close()
				lastErr = rErr
				break
			}
		}
	}
	return lastErr
}

// ---- 辅助 ----

func probe(client *http.Client, url string) (total int64, ranges bool, err error) {
	req, _ := http.NewRequest("HEAD", url, nil)
	req.Header.Set("User-Agent", "CLink/1.1 (github.com/ccdsyy/CLink)")
	resp, err := client.Do(req)
	if err != nil {
		return -1, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return -1, false, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		fmt.Sscanf(cl, "%d", &total)
	}
	ranges = strings.Contains(strings.ToLower(resp.Header.Get("Accept-Ranges")), "bytes")
	return total, ranges, nil
}

func newManifest(url, dest string, total int64, threads int) *manifest {
	m := &manifest{URL: url, Dest: dest, Total: total, Created: time.Now().Unix()}
	n := threads
	if total > 0 && int64(n) > total { // 极小文件不超过字节数
		n = int(total)
	}
	if n < 1 {
		n = 1
	}
	step := (total + int64(n) - 1) / int64(n)
	if step <= 0 {
		step = total
		n = 1
	}
	for i := 0; i < n; i++ {
		start := int64(i) * step
		end := start + step - 1
		if end >= total {
			end = total - 1
		}
		if start > end {
			break
		}
		m.Chunks = append(m.Chunks, chunk{Start: start, End: end,
			Name: fmt.Sprintf("%s.clink-part.%d", filepath.Base(dest), i)})
	}
	return m
}

func saveManifest(path string, m *manifest) error {
	b, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func mergeChunks(m *manifest, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	for _, c := range m.Chunks {
		p, err := os.Open(filepath.Join(filepath.Dir(dest), c.Name))
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, p); err != nil {
			p.Close()
			return err
		}
		p.Close()
		os.Remove(filepath.Join(filepath.Dir(dest), c.Name))
	}
	return nil
}

// counter 进度计数（并发安全）。
type counter struct {
	mu    sync.Mutex
	done  int64
	total int64
}

func (c *counter) add(n int64) {
	c.mu.Lock()
	c.done += n
	c.mu.Unlock()
}
func (c *counter) snapshot() (done, total int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.done, c.total
}
func (c *counter) resumeExisting(m *manifest, dest string) {
	var done int64
	for _, ch := range m.Chunks {
		if st, err := os.Stat(filepath.Join(filepath.Dir(dest), ch.Name)); err == nil {
			need := ch.End - ch.Start + 1
			if st.Size() >= need {
				done += need
			} else {
				done += st.Size()
			}
		}
	}
	c.mu.Lock()
	c.done = done
	c.mu.Unlock()
}

// speedWindow 2 秒滑动窗口速度估算。
type speedWindow struct {
	mu    sync.Mutex
	start time.Time
	base  int64
	last  int64
}

func newSpeedWindow() *speedWindow {
	return &speedWindow{start: time.Now()}
}
func (s *speedWindow) add(n int64) {
	s.mu.Lock()
	s.last += n
	s.mu.Unlock()
}
func (s *speedWindow) rate() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	sec := time.Since(s.start).Seconds()
	if sec < 0.5 {
		return 0
	}
	return int64(float64(s.last) / sec)
}

func jobID(dest string) string {
	abs, _ := filepath.Abs(dest)
	h := uint64(1469598103934665603)
	for _, b := range []byte(abs) {
		h = (h ^ uint64(b)) * 1099511628211
	}
	return fmt.Sprintf("dl-%016x", h)
}

// completedChunks 统计已写满的分片数（用于展示活跃线程数）。
func completedChunks(m *manifest, dest string) int {
	n := 0
	for _, c := range m.Chunks {
		if st, err := os.Stat(filepath.Join(filepath.Dir(dest), c.Name)); err == nil &&
			st.Size() >= c.End-c.Start+1 {
			n++
		}
	}
	return n
}
