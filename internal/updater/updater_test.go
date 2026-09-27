package updater

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckFallback(t *testing.T) {
	// 主源 500，备源正常 → 应取备源数据
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"version":"1.2.0","build":3,"notes":"测试版本","date":"2026-09-21",
			"download":"https://example.com/CLink.exe","page":"https://clink.541887.xyz"}`))
	}))
	defer good.Close()

	up, err := Check([]string{bad.URL + "/u.json", good.URL + "/u.json"})
	if err != nil {
		t.Fatal(err)
	}
	if up.Version != "1.2.0" || up.Build != 3 || up.Download != "https://example.com/CLink.exe" {
		t.Fatalf("解析结果异常: %+v", up)
	}
}

func TestCheckRejectInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":"x"}`)) // 缺 build
	}))
	defer srv.Close()
	if _, err := Check([]string{srv.URL}); err == nil {
		t.Fatal("缺 build 的载荷应被拒绝")
	}
}

func TestCheckAllFail(t *testing.T) {
	if _, err := Check([]string{"http://127.0.0.1:1/u.json"}); err == nil {
		t.Fatal("全部失败应返回错误")
	}
}

func TestDefaultURLs(t *testing.T) {
	urls := DefaultURLs()
	if len(urls) != 2 || urls[0] != "https://clink.541887.xyz/update.json" {
		t.Fatalf("默认源异常: %v", urls)
	}
}
