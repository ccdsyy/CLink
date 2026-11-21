package session

// Event 推送给 UI 的事件（Wails EventsEmit 载荷）。
type Event struct {
	Stage  string `json:"stage"`            // created|waiting|guest-joined|guest-left|guest-kicked|guest-rejected|connected|reconnecting|error|info
	Msg    string `json:"msg"`              // 人话描述（面向小白）
	Room   string `json:"room,omitempty"`   // 房间码
	Name   string `json:"name,omitempty"`   // 玩家昵称（成员管理/通知用）
	Addr   string `json:"addr,omitempty"`   // 客机：本地连接地址 127.0.0.1:xxxx
	Tier   string `json:"tier,omitempty"`   // ipv6|webrtc
	Guests int    `json:"guests,omitempty"` // 房主：已加入人数
	RTT    int64  `json:"rtt,omitempty"`    // 客机：连接往返延迟（毫秒）
}
