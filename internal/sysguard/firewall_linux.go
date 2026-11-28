//go:build linux

// Linux 防火墙放行（ROADMAP #27）：探测 ufw/firewalld/iptables 并加规则。
package sysguard

import (
	"os/exec"
	"strings"
)

func isAdmin() bool {
	out, err := exec.Command("id", "-u").Output()
	return err == nil && strings.TrimSpace(string(out)) == "0"
}

func ensureFirewall() error {
	mc := "25565"
	// ufw
	if _, err := exec.LookPath("ufw"); err == nil {
		if b, _ := exec.Command("ufw", "status").Output(); strings.Contains(strings.ToLower(string(b)), "active") {
			if err := exec.Command("ufw", "allow", mc+"/tcp").Run(); err != nil {
				return err
			}
			return nil
		}
	}
	// firewalld
	if _, err := exec.LookPath("firewall-cmd"); err == nil {
		if b, _ := exec.Command("firewall-cmd", "--state").Output(); strings.TrimSpace(string(b)) == "running" {
			if err := exec.Command("firewall-cmd", "--add-port="+mc+"/tcp").Run(); err != nil {
				return err
			}
			return nil
		}
	}
	// iptables 兜底
	if _, err := exec.LookPath("iptables"); err == nil {
		return exec.Command("iptables", "-I", "INPUT", "-p", "tcp", "--dport", mc, "-j", "ACCEPT").Run()
	}
	return nil
}
