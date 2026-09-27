package modhub

import (
        "crypto/sha256"
        "encoding/hex"
        "fmt"
        "net/http"
        "net/http/httptest"
        "os"
        "path/filepath"
        "strings"
        "sync/atomic"
        "testing"
        "time"
)

// rangeSrv 模拟支持 Range 的文件服务器（记录 Range 请求次数）。
func rangeSrv(t *testing.T, data []byte, hits *int32) *httptest.Server {
        return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                atomic.AddInt32(hits, 1)
                w.Header().Set("Accept-Ranges", "bytes")
                w.Header().Set("Content-Length", fmt.Sprint(len(data)))
                if r.Method == http.MethodHead {
                        return
                }
                rg := r.Header.Get("Range")
                if rg == "" {
                        w.Write(data)
                        return
                }
                var start, end int64
                fmt.Sscanf(rg, "bytes=%d-%d", &start, &end)
                if end >= int64(len(data)) || end == 0 {
                        end = int64(len(data)) - 1
                }
                w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
                w.WriteHeader(http.StatusPartialContent)
                w.Write(data[start : end+1])
        }))
}

func dataHash(b []byte) string {
        h := sha256.Sum256(b)
        return hex.EncodeToString(h[:])
}

func TestMultiThreadDownloadAndResume(t *testing.T) {
        // 512KB 随机内容
        data := make([]byte, 512*1024)
        for i := range data {
                data[i] = byte(i * 7)
        }
        want := dataHash(data)

        var hits int32
        srv := rangeSrv(t, data, &hits)
        defer srv.Close()

        dest := filepath.Join(t.TempDir(), "testmod.jar")
        var prog1 atomic.Value
        dl := NewDownloader(func(p Progress) { prog1.Store(p) })

        // 全量多线程下载（8 线程分 8 片）
        if err := dl.Download("t1", srv.URL+"/mod.jar", dest, 8); err != nil {
                t.Fatal(err)
        }
        waitDone(t, &prog1, 15*time.Second)

        got, err := os.ReadFile(dest)
        if err != nil {
                t.Fatal(err)
        }
        if dataHash(got) != want {
                t.Fatalf("内容不一致：分片合并错误")
        }
        if atomic.LoadInt32(&hits) < 8 {
                t.Fatalf("未走多线程分片（Range 请求 %d 次）", hits)
        }
        if _, err := os.Stat(dest + ".clink-dl.json"); !os.IsNotExist(err) {
                t.Fatal("完成后清单应被清理")
        }

        // ---- 断点续传：删目标文件，只留一半分片，重建任务 ----
        var hits2 int32
        srv2 := rangeSrv(t, data, &hits2)
        defer srv2.Close()

        m := newManifest(srv2.URL+"/mod.jar", dest, int64(len(data)), 8)
        mfPath := dest + ".clink-dl.json"
        for i, c := range m.Chunks {
                pp := filepath.Join(t.TempDir(), c.Name) // 先写真实分片到 dest 目录
                _ = pp
                part := filepath.Join(filepath.Dir(dest), c.Name)
                if i < 4 { // 前 4 片写满
                        if err := os.WriteFile(part, data[c.Start:c.End+1], 0o644); err != nil {
                                t.Fatal(err)
                        }
                } else if i == 4 { // 第 5 片写一半
                        half := (c.End - c.Start + 1) / 2
                        if err := os.WriteFile(part, data[c.Start:c.Start+half], 0o644); err != nil {
                                t.Fatal(err)
                        }
                }
        }
        if err := saveManifest(mfPath, m); err != nil {
                t.Fatal(err)
        }
        os.Remove(dest) // 确保目标不存在

        var prog2 atomic.Value
        dl2 := NewDownloader(func(p Progress) { prog2.Store(p) })
        if err := dl2.Download("t2", srv2.URL+"/mod.jar", dest, 8); err != nil {
                t.Fatal(err)
        }
        waitDone(t, &prog2, 15*time.Second)

        got2, err := os.ReadFile(dest)
        if err != nil {
                t.Fatal(err)
        }
        if dataHash(got2) != want {
                t.Fatal("续传后内容不一致")
        }
        if _, err := os.Stat(dest + ".clink-dl.json"); !os.IsNotExist(err) {
                t.Fatal("续传完成后清单应被清理")
        }
}

func TestResumeAll(t *testing.T) {
        data := []byte(strings.Repeat("CLink断点续传", 4096))
        var hits int32
        srv := rangeSrv(t, data, &hits)
        defer srv.Close()
        dir := t.TempDir()
        dest := filepath.Join(dir, "mod.jar")

        // 造一个未完成任务（无分片，只有清单）
        m := newManifest(srv.URL+"/mod.jar", dest, int64(len(data)), 4)
        if err := saveManifest(dest+".clink-dl.json", m); err != nil {
                t.Fatal(err)
        }

        var done atomic.Value
        dl := NewDownloader(func(p Progress) {
                if p.Finished || p.Failed {
                        done.Store(p)
                }
        })
        n, err := dl.ResumeAll(dir, 4)
        if err != nil || n != 1 {
                t.Fatalf("ResumeAll 应发现 1 个任务，got %d err=%v", n, err)
        }
        deadline := time.Now().Add(10 * time.Second)
        for time.Now().Before(deadline) {
                if v := done.Load(); v != nil {
                        p := v.(Progress)
                        if p.Failed {
                                t.Fatalf("续传失败: %s", p.Err)
                        }
                        break
                }
                time.Sleep(100 * time.Millisecond)
        }
        got, err := os.ReadFile(dest)
        if err != nil {
                t.Fatal(err)
        }
        if string(got) != string(data) {
                t.Fatal("ResumeAll 内容不一致")
        }
}

func TestFormatBytes(t *testing.T) {
        cases := map[int64]string{512: "512 B", 5 << 20: "5.0 MB", 3 << 10: "3.0 KB", int64(2 << 30): "2.0 GB"}
        for in, want := range cases {
                if got := FormatBytes(in); got != want {
                        t.Fatalf("FormatBytes(%d)=%s want %s", in, got, want)
                }
        }
}

func waitDone(t *testing.T, lastProg *atomic.Value, timeout time.Duration) {
        t.Helper()
        deadline := time.Now().Add(timeout)
        for time.Now().Before(deadline) {
                if v := lastProg.Load(); v != nil {
                        p := v.(Progress)
                        if p.Finished || p.Failed {
                                if p.Failed {
                                        t.Fatalf("下载失败: %s", p.Err)
                                }
                                return
                        }
                }
                time.Sleep(100 * time.Millisecond)
        }
        t.Fatal("下载超时")
}
