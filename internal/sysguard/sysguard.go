package sysguard

// EnsureFirewall 添加 Windows 防火墙入站放行（CLink 程序规则 + 25565 端口）。
// 非管理员时弹一次 UAC 授权框；非 Windows 平台为空操作。
// 幂等：重复调用先删旧规则再添加。
func EnsureFirewall() error { return ensureFirewall() }

// IsAdmin 当前进程是否具有管理员权限。
func IsAdmin() bool { return isAdmin() }
