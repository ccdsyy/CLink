// Package ipc 本地 IPC 接口（ROADMAP #69）。
//
// 127.0.0.1:42656 上的 JSON 行协议：启动器 / 第三方 Mod 可以调用 CLink
// 的穿透栈能力（创建房间 / 加入 / 状态 / 退出），无需 GUI 交互。
// 仅监听回环地址，本机进程间通信。
package ipc

import (
	"bufio"
	"encoding/json"
	"net"
	"strings"
)

// Handler 命令处理回调集合。
type Handler struct {
	Status func() map[string]any
	Create func(mcPort int) (string, error)
	Join   func(code, name string) error
	Leave  func()
	Recent func() []string
}

// Serve 启动 IPC 监听（失败静默，端口冲突不影响主功能）。
func Serve(h Handler) {
	ln, err := net.Listen("tcp", "127.0.0.1:42656")
	if err != nil {
		return
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleConn(conn, h)
		}
	}()
}

func handleConn(conn net.Conn, h Handler) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 4096), 64*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req struct {
			Cmd    string `json:"cmd"`
			MCPort int    `json:"mcPort"`
			Code   string `json:"code"`
			Name   string `json:"name"`
		}
		if json.Unmarshal([]byte(line), &req) != nil {
			writeErr(conn, "bad json")
			continue
		}
		switch req.Cmd {
		case "status":
			if h.Status != nil {
				writeJSON(conn, h.Status())
			} else {
				writeErr(conn, "not available")
			}
		case "create":
			if h.Create == nil {
				writeErr(conn, "not available")
				break
			}
			code, err := h.Create(req.MCPort)
			if err != nil {
				writeErr(conn, err.Error())
				break
			}
			writeJSON(conn, map[string]any{"ok": true, "code": code})
		case "join":
			if h.Join == nil {
				writeErr(conn, "not available")
				break
			}
			if err := h.Join(req.Code, req.Name); err != nil {
				writeErr(conn, err.Error())
				break
			}
			writeJSON(conn, map[string]any{"ok": true})
		case "leave":
			if h.Leave != nil {
				h.Leave()
			}
			writeJSON(conn, map[string]any{"ok": true})
		case "recent":
			if h.Recent != nil {
				writeJSON(conn, map[string]any{"rooms": h.Recent()})
			} else {
				writeErr(conn, "not available")
			}
		default:
			writeErr(conn, "unknown cmd")
		}
	}
}

func writeJSON(conn net.Conn, v any) {
	b, _ := json.Marshal(v)
	conn.Write(append(b, '\n'))
}

func writeErr(conn net.Conn, msg string) {
	b, _ := json.Marshal(map[string]any{"ok": false, "error": msg})
	conn.Write(append(b, '\n'))
}
