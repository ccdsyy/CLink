// Package proxyconn HTTP CONNECT 出站（ROADMAP #37）：
// 校园网/公司网强制 HTTP 代理的环境下，信令与 WS 中继经 CONNECT 隧道出站
// ——这种环境别家工具全死，CLink 还能活。
package proxyconn

import (
	"bufio"
	"encoding/base64"
	"net"
	"net/url"
	"time"
)

// Dial 经 HTTP 代理建 CONNECT 隧道到 target（host:port）。
// proxyURL 形如 http://user:pass@1.2.3.4:8080。
func Dial(proxyURL, target string, timeout time.Duration) (net.Conn, error) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, err
	}
	proxyHost := u.Host
	if _, _, e := net.SplitHostPort(proxyHost); e != nil {
		proxyHost += ":8080"
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.Dial("tcp", proxyHost)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if u.User != nil {
		pass, _ := u.User.Password()
		auth := basicAuth(u.User.Username(), pass)
		req += "Proxy-Authorization: Basic " + auth + "\r\n"
	}
	req += "\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, err
	}
	// 期望 "HTTP/1.1 200"
	ok := len(line) >= 12 && line[9:12] == "200"
	if !ok {
		conn.Close()
		return nil, errConnect(line)
	}
	// 跳过剩余响应头
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, err
		}
		if l == "\r\n" || l == "\n" {
			break
		}
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func basicAuth(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

type errConnect string

func (e errConnect) Error() string { return "代理 CONNECT 失败：" + string(e) }
