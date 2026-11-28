//go:build !windows

// 非 Windows：无第三方墙检测概念（ROADMAP #26 Windows 专属）。
package sysguard

// DetectThirdPartyFirewall 恒返回空。
func DetectThirdPartyFirewall() string { return "" }
