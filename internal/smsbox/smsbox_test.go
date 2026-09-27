package smsbox

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestPingAndSend(t *testing.T) {
	var mu sync.Mutex
	gotBody := map[string]any{}
	var gotPIN string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ping" {
			if r.Header.Get("X-PIN") != "1234" {
				w.WriteHeader(401)
				return
			}
			w.Write([]byte(`{"ok":true}`))
			return
		}
		if r.URL.Path == "/api/send" {
			mu.Lock()
			gotPIN = r.Header.Get("X-PIN")
			json.NewDecoder(r.Body).Decode(&gotBody)
			mu.Unlock()
			w.Write([]byte(`{"ok":true,"results":["queued:13800138000"]}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	c := New(srv.URL, "1234")
	if err := c.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := c.Send([]string{"138-0013 8000"}, "测试短信"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotPIN != "1234" {
		t.Fatalf("PIN 头缺失: %q", gotPIN)
	}
	phones := gotBody["phones"].([]any)
	if len(phones) != 1 || phones[0] != "138-0013 8000" {
		t.Fatalf("phones 透传异常: %v", gotBody["phones"])
	}
}

func TestSendErrors(t *testing.T) {
	c := New("", "")
	if err := c.Send([]string{"138"}, "x"); err == nil {
		t.Fatal("未配置地址应报错")
	}
	if err := c.Ping(); err == nil {
		t.Fatal("未配置地址 Ping 应报错")
	}
}

func TestLimiterDebounce(t *testing.T) {
	l := NewLimiter()
	if !l.Allow("join-小明") {
		t.Fatal("首次应放行")
	}
	if l.Allow("join-小明") {
		t.Fatal("60s 内同 key 应拒绝")
	}
	if !l.Allow("join-小红") {
		t.Fatal("不同 key 应放行")
	}
}

func TestLimiterHourlyCap(t *testing.T) {
	l := NewLimiter()
	n := 0
	for i := 0; i < hourlyMax+5; i++ {
		if l.Allow("k" + string(rune('a'+i%26)) + string(rune('0'+i/26))) {
			n++
		}
	}
	if n != hourlyMax {
		t.Fatalf("每小时应恰好放行 %d 条，实际 %d", hourlyMax, n)
	}
}

func TestLimiterWindowCleanup(t *testing.T) {
	l := NewLimiter()
	// 人为塞满 1 小时前的窗口
	for i := 0; i < hourlyMax; i++ {
		l.window = append(l.window, time.Now().Add(-2*time.Hour))
	}
	if !l.Allow("fresh") {
		t.Fatal("过期窗口应被清理并放行")
	}
}
