package netcfg

import (
	"net"
	"strings"
)

// ============================================================================
// Network Interface Configuration (Delegated to Configurator)
// 网络接口配置 (委托给配置器)
// ============================================================================

// SetIPAddress configures an IPv4 address on the interface / SetIPAddress在接口上配置IPv4地址
func SetIPAddress(ifname string, ip net.IP, prefixLen int) error {
	return GetConfigurator().SetIPAddress(ifname, ip, prefixLen)
}

// SetIPv6Address configures an IPv6 address on the interface / SetIPv6Address在接口上配置IPv6地址
func SetIPv6Address(ifname string, ip net.IP, prefixLen int) error {
	return GetConfigurator().SetIPv6Address(ifname, ip, prefixLen)
}

// FlushAddresses removes all IP addresses from the interface / FlushAddresses移除接口上的所有IP地址
func FlushAddresses(ifname string) error {
	return GetConfigurator().FlushAddresses(ifname)
}

// AddDefaultRoute adds a default route via the given gateway / AddDefaultRoute添加通过给定网关的默认路由
func AddDefaultRoute(ifname string, gateway net.IP) error {
	return GetConfigurator().AddDefaultRoute(ifname, gateway)
}

// AddDefaultRouteDirect adds a default route directly to the interface (no gateway) / AddDefaultRouteDirect直接向接口添加默认路由(无网关)
func AddDefaultRouteDirect(ifname string, ipv6 bool) error {
	return GetConfigurator().AddDefaultRouteDirect(ifname, ipv6)
}

// FlushRoutes removes all routes for the interface / FlushRoutes移除接口的所有路由
func FlushRoutes(ifname string) error {
	return GetConfigurator().FlushRoutes(ifname)
}

// BringUp brings the interface up / BringUp启动接口
func BringUp(ifname string) error {
	return GetConfigurator().BringUp(ifname)
}

// BringDown brings the interface down / BringDown关闭接口
func BringDown(ifname string) error {
	return GetConfigurator().BringDown(ifname)
}

// SetMTU sets the MTU for the interface / SetMTU设置接口的MTU
func SetMTU(ifname string, mtu int) error {
	return GetConfigurator().SetMTU(ifname, mtu)
}

// GetCurrentIP returns the current global IP address of the interface / GetCurrentIP返回接口当前的全局IP地址
func GetCurrentIP(ifname string) (net.IP, error) {
	return GetConfigurator().GetCurrentIP(ifname)
}

// GetCurrentIPs returns all global IPv4 and IPv6 addresses currently assigned to the interface.
func GetCurrentIPs(ifname string) ([]net.IP, error) {
	cfg := GetConfigurator()
	if multi, ok := cfg.(MultiIPConfigurator); ok {
		return multi.GetCurrentIPs(ifname)
	}
	ip, err := cfg.GetCurrentIP(ifname)
	if err != nil || ip == nil {
		return nil, err
	}
	return []net.IP{ip}, nil
}

// IsUp checks if the interface is up / IsUp检查接口是否已启动
func IsUp(ifname string) (bool, error) {
	return GetConfigurator().IsUp(ifname)
}

// ============================================================================
// DNS Configuration / DNS配置
// ============================================================================

// UpdateResolvConf updates /etc/resolv.conf with the given DNS servers / UpdateResolvConf使用给定的DNS服务器更新/etc/resolv.conf
func UpdateResolvConf(dns1, dns2 string) error {
	return GetConfigurator().UpdateDNS(dns1, dns2)
}

// UpdateResolvConfServers updates system DNS with an arbitrary list of IPv4/IPv6 DNS servers.
func UpdateResolvConfServers(servers ...string) error {
	normalized := normalizeDNSServerList(servers...)
	if len(normalized) == 0 {
		return nil
	}
	cfg := GetConfigurator()
	if multi, ok := cfg.(MultiDNSConfigurator); ok {
		return multi.UpdateDNSServers(normalized...)
	}
	var dns1, dns2 string
	if len(normalized) >= 1 {
		dns1 = normalized[0]
	}
	if len(normalized) >= 2 {
		dns2 = normalized[1]
	}
	return cfg.UpdateDNS(dns1, dns2)
}

func normalizeDNSServerList(servers ...string) []string {
	seen := make(map[string]struct{}, len(servers))
	out := make([]string, 0, len(servers))
	for _, raw := range servers {
		srv := strings.TrimSpace(raw)
		if srv == "" {
			continue
		}
		if ip := net.ParseIP(srv); ip != nil && ip.IsUnspecified() {
			continue
		}
		if _, exists := seen[srv]; exists {
			continue
		}
		seen[srv] = struct{}{}
		out = append(out, srv)
	}
	return out
}

// RestoreResolvConf removes nameserver entries (for cleanup) / RestoreResolvConf移除nameserver条目 (用于清理)
func RestoreResolvConf() error {
	return GetConfigurator().RestoreDNS()
}

// ============================================================================
// QMAP 多路复用 / QMAP Multiplexing
// ============================================================================

// AddQMAPMux 创建 QMAP 虚拟子网卡，返回新创建的虚拟网卡名 (如 qmimux0)
func AddQMAPMux(masterIface string, muxID uint8) (string, error) {
	return GetConfigurator().AddQMAPMux(masterIface, muxID)
}

// DelQMAPMux 销毁 QMAP 虚拟子网卡
func DelQMAPMux(masterIface string, muxID uint8) error {
	return GetConfigurator().DelQMAPMux(masterIface, muxID)
}

// GetQMAPMuxIface 查询 MuxID 对应的虚拟网卡名
func GetQMAPMuxIface(masterIface string, muxID uint8) string {
	return GetConfigurator().GetQMAPMuxIface(masterIface, muxID)
}

// EnableRawIP 在物理网卡上开启 Raw IP 模式（QMAP 前置条件）
func EnableRawIP(ifname string) error {
	return GetConfigurator().EnableRawIP(ifname)
}
