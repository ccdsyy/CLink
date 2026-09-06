//go:build !windows

package sysguard

func isAdmin() bool { return true } // 非 Windows 无此概念

func ensureFirewall() error { return nil } // Linux/macOS 由系统弹本地网络权限
