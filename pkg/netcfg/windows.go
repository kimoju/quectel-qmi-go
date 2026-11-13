package netcfg

import (
	"fmt"
	"net"
	"os/exec"
	"strconv"
)

// WindowsConfigurator implements NetworkConfigurator for Windows using netsh
// WindowsConfigurator 使用 netsh 实现 Windows 的 NetworkConfigurator
type WindowsConfigurator struct{}

func NewWindowsConfigurator() *WindowsConfigurator {
	return &WindowsConfigurator{}
}

func (w *WindowsConfigurator) run(args ...string) error {
	cmd := exec.Command("netsh", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("command failed: %s, output: %s", err, string(out))
	}
	return nil
}

func (w *WindowsConfigurator) SetIPAddress(ifname string, ip net.IP, prefixLen int) error {
	mask := net.IP(net.CIDRMask(prefixLen, 32)).String()
	return w.run("interface", "ipv4", "set", "address", "name="+ifname, "source=static", "addr="+ip.String(), "mask="+mask)
}

func (w *WindowsConfigurator) SetIPv6Address(ifname string, ip net.IP, prefixLen int) error {
	return w.run("interface", "ipv6", "add", "address", ifname, fmt.Sprintf("%s/%d", ip.String(), prefixLen))
}

func (w *WindowsConfigurator) FlushAddresses(ifname string) error {
	_ = w.run("interface", "ipv4", "set", "address", "name="+ifname, "source=dhcp")
	_ = w.run("interface", "ipv6", "set", "address", ifname, "dhcp")
	return nil
}

func (w *WindowsConfigurator) AddDefaultRoute(ifname string, gateway net.IP) error {
	if len(gateway) == 0 || gateway.IsUnspecified() {
		return w.AddDefaultRouteDirect(ifname, false)
	}
	if gateway.To4() != nil {
		return w.run("interface", "ipv4", "add", "route", "0.0.0.0/0", ifname, "nexthop="+gateway.String())
	}
	return w.run("interface", "ipv6", "add", "route", "::/0", ifname, "nexthop="+gateway.String())
}

func (w *WindowsConfigurator) AddDefaultRouteDirect(ifname string, ipv6 bool) error {
	if ipv6 {
		return w.run("interface", "ipv6", "add", "route", "::/0", ifname)
	}
	return w.run("interface", "ipv4", "add", "route", "0.0.0.0/0", ifname)
}

func (w *WindowsConfigurator) FlushRoutes(ifname string) error {
	_ = w.run("interface", "ipv4", "delete", "route", "0.0.0.0/0", ifname)
	_ = w.run("interface", "ipv6", "delete", "route", "::/0", ifname)
	return nil
}

func (w *WindowsConfigurator) BringUp(ifname string) error {
	return w.run("interface", "set", "interface", ifname, "admin=enable")
}

func (w *WindowsConfigurator) BringDown(ifname string) error {
	return w.run("interface", "set", "interface", ifname, "admin=disable")
}

func (w *WindowsConfigurator) SetMTU(ifname string, mtu int) error {
	if err := w.run("interface", "ipv4", "set", "subinterface", ifname, "mtu="+strconv.Itoa(mtu), "store=persistent"); err != nil {
		return err
	}
	return w.run("interface", "ipv6", "set", "subinterface", ifname, "mtu="+strconv.Itoa(mtu), "store=persistent")
}

func (w *WindowsConfigurator) GetCurrentIPs(ifname string) ([]net.IP, error) {
	iface, err := net.InterfaceByName(ifname)
	if err != nil {
		return nil, err
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, err
	}
	var v4s, v6s []net.IP
	for _, addr := range addrs {
		ipnet, ok := addr.(*net.IPNet)
		if !ok || len(ipnet.IP) == 0 || ipnet.IP.IsLoopback() || ipnet.IP.IsUnspecified() || ipnet.IP.IsMulticast() {
			continue
		}
		if v4 := ipnet.IP.To4(); v4 != nil {
			if !v4.IsLinkLocalUnicast() {
				v4s = append(v4s, v4)
			}
			continue
		}
		if v6 := ipnet.IP.To16(); v6 != nil && !v6.IsLinkLocalUnicast() && !v6.IsLinkLocalMulticast() {
			v6s = append(v6s, v6)
		}
	}
	return append(v4s, v6s...), nil
}

func (w *WindowsConfigurator) GetCurrentIP(ifname string) (net.IP, error) {
	ips, err := w.GetCurrentIPs(ifname)
	if err != nil || len(ips) == 0 {
		return nil, err
	}
	return ips[0], nil
}

func (w *WindowsConfigurator) IsUp(ifname string) (bool, error) {
	iface, err := net.InterfaceByName(ifname)
	if err != nil {
		return false, err
	}
	return iface.Flags&net.FlagUp != 0, nil
}

func (w *WindowsConfigurator) UpdateDNS(dns1, dns2 string) error {
	// DNS is per-interface in Windows. We don't have ifname here, which is a design flaw in the interface for Windows.
	// Windows 中 DNS 是基于接口的。我们这里没有 ifname，这是接口设计在 Windows 上的缺陷。
	// We'll assume a global variable or modify interface later. For now, skipping.
	// 我们稍后将假设一个全局变量或修改接口。目前跳过。
	return fmt.Errorf("UpdateDNS not supported on Windows directly without interface name")
}

func (w *WindowsConfigurator) RestoreDNS() error {
	return nil
}

// QMAP 多路复用在 Windows 上不支持
func (w *WindowsConfigurator) AddQMAPMux(masterIface string, muxID uint8) (string, error) {
	return "", fmt.Errorf("QMAP 多路复用在 Windows 上不可用")
}
func (w *WindowsConfigurator) DelQMAPMux(masterIface string, muxID uint8) error       { return nil }
func (w *WindowsConfigurator) GetQMAPMuxIface(masterIface string, muxID uint8) string { return "" }
func (w *WindowsConfigurator) EnableRawIP(ifname string) error                        { return nil }
