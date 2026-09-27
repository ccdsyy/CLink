package blacklist

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bl.json")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Has("Griefer") {
		t.Fatal("空表不应命中")
	}
	if err := s.Add("Griefer", "拆家"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("  griefer ", "重复添加应幂等"); err != nil {
		t.Fatal(err)
	}
	if !s.Has("griefer") || !s.Has("GRIEFER") {
		t.Fatal("大小写不敏感匹配失败")
	}

	// 重新加载应恢复
	s2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.Has("Griefer") {
		t.Fatal("持久化丢失")
	}
	list := s2.List()
	if len(list) != 1 || list[0].Reason != "重复添加应幂等" {
		t.Fatalf("List 结果异常: %+v", list)
	}

	if err := s2.Remove("griefer"); err != nil {
		t.Fatal(err)
	}
	if s2.Has("Griefer") {
		t.Fatal("解除拉黑失败")
	}
	s3, _ := Load(path)
	if s3.Has("Griefer") || len(s3.List()) != 0 {
		t.Fatal("删除未持久化")
	}
}

func TestStoreEmptyName(t *testing.T) {
	s, _ := Load(filepath.Join(t.TempDir(), "bl.json"))
	if s.Has("") || s.Has("  ") {
		t.Fatal("空名不应命中")
	}
	if err := s.Add("", "x"); err != nil {
		t.Fatal(err)
	}
	if len(s.List()) != 0 {
		t.Fatal("空名不应入库")
	}
	if !strings.HasSuffix(DefaultPath(), "blacklist.json") {
		t.Fatal("默认路径异常")
	}
}
