// Package toolbox 基于 UAPI（uapis.cn）的实用工具箱。
//
// 能力：IP 归属地查询、房间码二维码生成、
// Minecraft 服务器状态查询。全部走公开免费接口，无需鉴权。
package toolbox

import (
	"bytes"
	"encoding/base64"
	"fmt"
	uapi "github.com/AxT-Team/uapi-sdk-go/uapi"
)

// Client UAPI 客户端薄封装。
type Client struct {
	c *uapi.Client
}

// New 创建工具箱客户端。
func New() *Client { return &Client{c: uapi.New("https://uapis.cn", "")} }

// IPInfo IP 归属地结果。
type IPInfo struct {
	IP     string `json:"ip"`
	Region string `json:"region"`
	ISP    string `json:"isp"`
	ASN    string `json:"asn"`
}

// QueryIP 查询指定 IP（空则查本机出口 IP）的归属地。
func (t *Client) QueryIP(ip string) (*IPInfo, error) {
	args := map[string]any{}
	if ip != "" {
		args["ip"] = ip
	}
	r, err := t.c.Network().GetNetworkIpinfo(args)
	if err != nil {
		return nil, err
	}
	m, ok := r.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("返回结构异常")
	}
	get := func(k string) string {
		if v, ok := m[k].(string); ok {
			return v
		}
		return ""
	}
	return &IPInfo{IP: get("ip"), Region: get("region"), ISP: get("isp"), ASN: get("asn")}, nil
}

// QRCodePNG 生成二维码，返回 PNG 原始字节（size 为边长像素，默认 300）。
func (t *Client) QRCodePNG(text string, size int) ([]byte, error) {
	if text == "" {
		return nil, fmt.Errorf("内容为空")
	}
	if size <= 0 {
		size = 300
	}
	r, err := t.c.Image().GetImageQrcode(map[string]any{"text": text, "size": fmt.Sprint(size)})
	if err != nil {
		return nil, err
	}
	if b, ok := r.([]byte); ok && isPNG(b) {
		return b, nil
	}
	return nil, fmt.Errorf("二维码返回格式异常")
}

// QRCodeDataURL 生成 data:image/png;base64,... 便于前端 <img> 直接显示。
func (t *Client) QRCodeDataURL(text string, size int) (string, error) {
	b, err := t.QRCodePNG(text, size)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	buf.WriteString("data:image/png;base64,")
	buf.WriteString(base64.StdEncoding.EncodeToString(b))
	return buf.String(), nil
}

// MCServerStatus Minecraft 服务器状态（motd/在线人数/版本）。
type MCServerStatus struct {
	Online    bool   `json:"online"`
	Players   string `json:"players"` // "12/100"
	MOTD      string `json:"motd"`
	Version   string `json:"version"`
	LatencyMS int64  `json:"latency_ms"`
}

// QueryMCServer 查询任意 MC Java 服务器状态（如 mc.hypixel.net）。
func (t *Client) QueryMCServer(addr string) (*MCServerStatus, error) {
	if addr == "" {
		return nil, fmt.Errorf("地址为空")
	}
	r, err := t.c.Game().GetGameMinecraftServerstatus(map[string]any{"server": addr})
	if err != nil {
		return nil, err
	}
	m, ok := r.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("返回结构异常")
	}
	st := &MCServerStatus{}
	if v, ok := m["online"].(bool); ok {
		st.Online = v
	}
	if v, ok := m["motd"].(string); ok {
		st.MOTD = v
	}
	if v, ok := m["version"].(string); ok {
		st.Version = v
	}
	switch p := m["players"].(type) {
	case string:
		st.Players = p
	case map[string]any:
		online, _ := p["online"].(float64)
		max, _ := p["max"].(float64)
		st.Players = fmt.Sprintf("%d/%d", int(online), int(max))
	}
	if v, ok := m["latency"].(float64); ok {
		st.LatencyMS = int64(v)
	}
	return st, nil
}

func isPNG(b []byte) bool {
	return len(b) > 8 && b[0] == 0x89 && b[1] == 'P' && b[2] == 'N' && b[3] == 'G'
}
