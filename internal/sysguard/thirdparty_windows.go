//go:build windows

// 第三方防火墙/安全软件检测（ROADMAP #26）：netsh 规则可能被 360/火绒等
// 拦截而静默失效，检测到时给用户一键引导提示。
package sysguard

import (
	"os/exec"
	"strings"
)

var thirdPartyNames = map[string]string{
	"360tray":         "360 安全卫士",
	"360safe":         "360 安全卫士",
	"zhuangdongfangyu": "360 实时防护",
	"hipsdaemon":      "火绒安全",
	"wsctrl":          "火绒安全",
	"usysdiag":        "火绒安全",
	"qqpctray":        "腾讯电脑管家",
	"kxetray":         "金山毒霸",
}

// DetectThirdPartyFirewall 返回检测到的第三方安全软件名（空 = 未检测到）。
func DetectThirdPartyFirewall() string {
	out, err := exec.Command("tasklist", "/fo", "csv", "/nh").Output()
	if err != nil {
		return ""
	}
	lower := strings.ToLower(string(out))
	for proc, name := range thirdPartyNames {
		if strings.Contains(lower, proc+".exe") {
			return name
		}
	}
	return ""
}
