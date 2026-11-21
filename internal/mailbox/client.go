package mailbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// 信箱服务错误（语义化，供上层重试/换槽决策）
var (
	ErrCodeTaken = errors.New("槽位已被占用")    // shorten：自定义码冲突（并发加入/槽位已用）
	ErrNotFound  = errors.New("信箱不存在或已过期") // redirect-info 404 / get 404
)

// DefaultBases 默认信令节点列表（自动容灾模式）：
// 主用「剪贴板服务1」直连后端，备用 UAPI 网关（恢复后自动接管兜底）。
var DefaultBases = []string{EndpointClip1, EndpointUAPI}

// Client 信箱 HTTP 客户端：多节点容灾 + 超时 + 重试 + 退避。
// 请求失败（网络错误 / 429 / 5xx）时自动轮换到下一个信令节点。
type Client struct {
	mu    sync.Mutex
	bases []string
	idx   int // 当前首选节点下标（记住最近可用的节点，减少无效切换）

	http *http.Client
}

// NewClient 创建客户端；baseURL 为空时使用默认端点（单节点，向后兼容）。
func NewClient(baseURL string) *Client {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = EndpointClip1
	}
	return NewClientMulti(baseURL)
}

// NewClientMulti 创建多节点容灾客户端（按序轮换；全空时使用默认节点）。
func NewClientMulti(bases ...string) *Client {
	var clean []string
	for _, b := range bases {
		if b = strings.TrimSpace(b); b != "" {
			clean = append(clean, strings.TrimRight(b, "/"))
		}
	}
	if len(clean) == 0 {
		clean = append(clean, EndpointClip1)
	}
	return &Client{
		bases: clean,
		http:  &http.Client{Timeout: 8 * time.Second},
	}
}

// Base 当前使用的信令节点（诊断展示用）。
func (c *Client) Base() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bases[c.idx%len(c.bases)]
}

// rotate 失败后轮换到下一个节点。返回 true 表示已切换。
func (c *Client) rotate() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bases) < 2 {
		return false
	}
	c.idx = (c.idx + 1) % len(c.bases)
	return true
}

// Store 上传密文，返回信箱 id。
func (c *Client) Store(compressedData string, ttl int) (string, error) {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	var resp struct {
		ID string `json:"id"`
	}
	body := map[string]any{"compressedData": compressedData, "ttl": ttl}
	if err := c.doRetry("POST", "/api/store", body, &resp); err != nil {
		return "", err
	}
	if resp.ID == "" {
		return "", fmt.Errorf("信箱未返回 id")
	}
	return resp.ID, nil
}

// Get 取回密文。
func (c *Client) Get(id string) (string, error) {
	var resp struct {
		CompressedData string `json:"compressedData"`
	}
	q := "/api/get?id=" + url.QueryEscape(id)
	if err := c.doRetry("GET", q, nil, &resp); err != nil {
		return "", err
	}
	if resp.CompressedData == "" {
		return "", fmt.Errorf("信箱内容为空")
	}
	return resp.CompressedData, nil
}

// Shorten 注册短链：customCode 即信箱槽位。槽位被占用返回 ErrCodeTaken。
func (c *Client) Shorten(targetURL, customCode string) error {
	var resp struct {
		Code string `json:"code"`
	}
	body := map[string]any{"url": targetURL, "customCode": customCode}
	return c.doRetry("POST", "/api/shorten", body, &resp)
}

// RedirectInfo 查询槽位指向的目标 URL（寻址层核心）。
func (c *Client) RedirectInfo(code string) (string, error) {
	var resp struct {
		URL       string `json:"url"`
		CreatedAt string `json:"createdAt"`
	}
	if err := c.doRetry("GET", "/api/redirect-info/"+url.PathEscape(code), nil, &resp); err != nil {
		return "", err
	}
	if resp.URL == "" {
		return "", ErrNotFound
	}
	return resp.URL, nil
}

// ---- 内部：带重试与多节点轮换的 HTTP 封装 ----

type errResp struct {
	Error string `json:"error"`
}

// doRetry 整体重试预算 4 次：每次先在当前节点尝试，遇到网络错误 / 429 / 5xx
// 且存在备用节点时立即轮换（退避仍生效，防止节点被打挂）。
func (c *Client) doRetry(method, path string, body any, out any) error {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(500<<attempt) * time.Millisecond) // 1s, 2s, 4s
		}
		err := c.doOnce(method, path, body, out)
		if err == nil {
			return nil
		}
		// 语义错误不重试不换节点（冲突/不存在），直接上抛
		if errors.Is(err, ErrCodeTaken) || errors.Is(err, ErrNotFound) {
			return err
		}
		// 可恢复错误：尝试换节点（单节点配置时轮换是 no-op）
		c.rotate()
		lastErr = err
	}
	return fmt.Errorf("信箱请求失败（已在 %d 个节点上重试）: %w", len(c.bases), lastErr)
}

func (c *Client) doOnce(method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.Base()+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", "CLink/1.3 (+https://ccdsyy.github.io)")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("网络错误: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("请求过于频繁（429）")
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("信箱服务异常（HTTP %d）", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}

	if resp.StatusCode >= 400 {
		var er errResp
		_ = json.Unmarshal(data, &er)
		msg := er.Error
		if msg == "" {
			msg = string(data)
		}
		switch {
		case resp.StatusCode == http.StatusNotFound && strings.Contains(strings.ToLower(msg), "endpoint not found"):
			// 网关级 404（节点未路由该端点，如 UAPI 网关暂时下线 Clipzy）：
			// 属于节点故障而非槽位为空，作为可恢复错误上抛以触发节点轮换
			return fmt.Errorf("信令节点不可用（网关未路由该端点）")
		case resp.StatusCode == http.StatusNotFound:
			return fmt.Errorf("%w: %s", ErrNotFound, msg)
		case strings.Contains(msg, "已被使用") || strings.Contains(msg, "already"):
			return fmt.Errorf("%w: %s", ErrCodeTaken, msg)
		default:
			return fmt.Errorf("信箱拒绝请求（HTTP %d）: %s", resp.StatusCode, msg)
		}
	}

	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("响应解析失败: %w", err)
		}
	}
	return nil
}
