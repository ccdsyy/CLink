package ws

import (
	"bytes"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestLoopback 客户端 ↔ 服务端 二进制消息回环（含 >64KB 跨帧）。
func TestLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			c, err := Serve(w, r)
			if err != nil {
				return
			}
			for {
				msg, err := c.ReadMessage()
				if err != nil {
					return
				}
				if err := c.WriteMessage(msg); err != nil {
					return
				}
			}
		})
		_ = http.Serve(ln, mux)
	}()

	time.Sleep(100 * time.Millisecond)
	c, err := Dial("ws://"+addr+"/", nil)
	if err != nil {
		t.Fatalf("拨号: %v", err)
	}
	defer c.Close()

	small := []byte("hello-clink-ws")
	if err := c.WriteMessage(small); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len(small))
	if err := readFull(c, got); err != nil {
		t.Fatalf("小包: %v", err)
	}
	if !bytes.Equal(got, small) {
		t.Fatalf("小包不一致")
	}

	big := make([]byte, 200*1024) // >64KB 跨多帧长度扩展
	for i := range big {
		big[i] = byte(i % 251)
	}
	go func() { _ = c.WriteMessage(big) }()
	got2 := make([]byte, len(big))
	if err := readFull(c, got2); err != nil {
		t.Fatalf("大包: %v", err)
	}
	if !bytes.Equal(got2, big) {
		t.Fatalf("大包不一致")
	}
}

func readFull(c *Conn, buf []byte) error {
	total := 0
	for total < len(buf) {
		msg, err := c.ReadMessage()
		if err != nil {
			return err
		}
		n := copy(buf[total:], msg)
		total += n
	}
	return nil
}
