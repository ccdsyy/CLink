// clink-relay：CLink 自建中继服务端（ROADMAP #31/32/33 + #36 WS 双传输）。
//
// 单文件部署到任意有公网的 VPS：
//
//	clink-relay -port 34780 -psk 你的共享口令 [-wsport 34781]
//
// 客户端在 CLink「设置 → 网络与信令」填：
//   udp://服务器IP:34780          （UDP 传输，低延迟首选）
//   ws://服务器IP:34781           （WebSocket 传输，UDP 被 QoS 杀死时的备胎，
//                                  且可挂在任意反代/CDN 后面）
// 多个逗号分隔，客户端自动选址。仅转发密文，看不到房间内容；
// 鉴权失败的流量静默丢弃，不会被当开放代理白嫖。
package main

import (
	"flag"
	"strconv"
	"log"
	"net"
	"net/http"

	"github.com/ccdsyy/clink/internal/relay"
	"github.com/ccdsyy/clink/internal/ws"
)

func main() {
	port := flag.Int("port", 34780, "UDP 监听端口")
	wsPort := flag.Int("wsport", 0, "WebSocket 监听端口（可选，0=关闭）")
	psk := flag.String("psk", "clink-default-psk", "共享口令（与客户端一致）")
	flag.Parse()

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: *port})
	if err != nil {
		log.Fatal(err)
	}
	srv := relay.NewServer(*psk)
	go srv.ServeUDP(conn)
	log.Printf("clink-relay UDP 已启动：udp/%d（psk 已设置：%v）", *port, *psk != "")

	if *wsPort > 0 {
		go func() {
			mux := http.NewServeMux()
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				c, err := ws.Serve(w, r)
				if err != nil {
					return
				}
				log.Printf("WS peer 接入：%s", r.RemoteAddr)
				for {
					msg, err := c.ReadMessage()
					if err != nil {
						return
					}
					srv.HandleDatagram(relay.WSPeer(c), msg)
				}
			})
			log.Printf("clink-relay WS 已启动：ws/%d", *wsPort)
			if err := http.ListenAndServe(":"+strconv.Itoa(*wsPort), mux); err != nil {
				log.Printf("WS 服务退出：%v", err)
			}
		}()
	}

	select {} // 常驻
}
