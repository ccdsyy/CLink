// clink-relay：CLink 自建中继服务端（ROADMAP #31/32/33）。
//
// 单文件部署到任意有公网的 VPS：
//
//	clink-relay -port 34780 -psk 你的共享口令
//
// 客户端在 CLink「设置 → 网络与信令 → 自建中继节点」填 udp://服务器IP:端口
// （多个逗号分隔，客户端自动选址）。仅转发密文，看不到房间内容；
// 鉴权失败的流量静默丢弃，不会被当开放代理白嫖。
package main

import (
	"flag"
	"log"
	"net"

	"github.com/ccdsyy/clink/internal/relay"
)

func main() {
	port := flag.Int("port", 34780, "UDP 监听端口")
	psk := flag.String("psk", "clink-default-psk", "共享口令（与客户端一致）")
	flag.Parse()

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: *port})
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("clink-relay 已启动：udp/%d（psk 已设置：%v）", *port, *psk != "")
	srv := relay.NewServer(*psk)
	srv.ServeUDP(conn)
}
