//go:build !windows && !linux

package sysguard

func isAdmin() bool { return true } // 非 Windows/Linux 无此概念

func ensureFirewall() error { return nil } // macOS 由系统弹本地网络权限
