//go:build linux
// +build linux

package netcfg

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/kimoju/netlink"
)

const defaultRouteMetric = 512

var resolvConfMu sync.Mutex

// LinuxConfigurator implements NetworkConfigurator for Linux using netlink
// LinuxConfigurator 使用 netlink 实现 Linux 的 NetworkConfigurator
type LinuxConfigurator struct {
	RouteMetric    int
	resolvConfPath string
	sysClassNetDir string
	parentIndexFn  func(muxIface, masterIface string) (bool, bool)
}

func NewLinuxConfigurator() *LinuxConfigurator {
	return &LinuxConfigurator{}
}

func (l *LinuxConfigurator) routeMetric() int {
	if l != nil && l.RouteMetric > 0 {
		return l.RouteMetric
	}
	return defaultRouteMetric
}

func (l *LinuxConfigurator) resolvPath() string {
	if l != nil && l.resolvConfPath != "" {
		return l.resolvConfPath
	}
	return defaultResolvConfPath
}

func (l *LinuxConfigurator) sysNetDir() string {
	if l != nil && l.sysClassNetDir != "" {
		return l.sysClassNetDir
	}
	return defaultSysClassNetDir
}

func isLinkNotFound(err error) bool {
	if err == nil {
		return false
	}
	var lnf netlink.LinkNotFoundError
	return errors.As(err, &lnf) ||
		errors.Is(err, syscall.ENODEV) ||
		errors.Is(err, syscall.ENOENT) ||
		os.IsNotExist(err)
}

func (l *LinuxConfigurator) SetIPAddress(ifname string, ip net.IP, prefixLen int) error {
	link, err := netlink.LinkByName(ifname)
	if err != nil {
		return fmt.Errorf("interface %s not found: %w", ifname, err)
	}

	ip4 := ip.To4()
	if ip4 == nil {
		return fmt.Errorf("invalid IPv4 address: %v", ip)
	}
	ipNet := &net.IPNet{
		IP:   ip4,
		Mask: net.CIDRMask(prefixLen, 32),
	}

	addr := &netlink.Addr{IPNet: ipNet}

	if err := netlink.AddrReplace(link, addr); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("failed to set IPv4 address: %w", err)
	}
	return nil
}

func (l *LinuxConfigurator) SetIPv6Address(ifname string, ip net.IP, prefixLen int) error {
	link, err := netlink.LinkByName(ifname)
	if err != nil {
		return fmt.Errorf("interface %s not found: %w", ifname, err)
	}

	ip16 := ip.To16()
	if ip16 == nil || ip.To4() != nil {
		return fmt.Errorf("invalid IPv6 address: %v", ip)
	}
	ipNet := &net.IPNet{
		IP:   ip16,
		Mask: net.CIDRMask(prefixLen, 128),
	}

	addr := &netlink.Addr{IPNet: ipNet}

	if err := netlink.AddrReplace(link, addr); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("failed to set IPv6 address: %w", err)
	}
	return nil
}

func (l *LinuxConfigurator) FlushAddresses(ifname string) error {
	link, err := netlink.LinkByName(ifname)
	if err != nil {
		return nil // Interface gone
	}

	addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return nil
	}

	for _, addr := range addrs {
		// Ignore errors during cleanup
		_ = netlink.AddrDel(link, &addr)
	}
	return nil
}

func buildGatewayDefaultRoute(linkIndex int, gateway net.IP, metric int) *netlink.Route {
	var dst *net.IPNet
	var gw net.IP
	if v4 := gateway.To4(); v4 != nil {
		dst = &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}
		gw = v4
	} else {
		dst = &net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)}
		gw = gateway.To16()
	}
	flags := 0
	if !gw.IsLinkLocalUnicast() {
		// Cellular Raw-IP / PtP PDNs frequently assign off-subnet gateways (/32 or /128).
		flags = int(netlink.FLAG_ONLINK)
	}
	return &netlink.Route{
		LinkIndex: linkIndex,
		Dst:       dst,
		Gw:        gw,
		Priority:  metric,
		Flags:     flags,
	}
}

func buildDirectDefaultRoute(linkIndex int, ipv6 bool, metric int) *netlink.Route {
	var dst *net.IPNet
	if ipv6 {
		dst = &net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)}
	} else {
		dst = &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}
	}
	return &netlink.Route{
		LinkIndex: linkIndex,
		Dst:       dst,
		Priority:  metric,
	}
}

func (l *LinuxConfigurator) AddDefaultRoute(ifname string, gateway net.IP) error {
	link, err := netlink.LinkByName(ifname)
	if err != nil {
		return fmt.Errorf("interface %s not found: %w", ifname, err)
	}

	if len(gateway) == 0 || gateway.IsUnspecified() {
		addrs, addrErr := netlink.AddrList(link, netlink.FAMILY_ALL)
		hasV4, hasV6 := false, false
		if addrErr == nil {
			for _, ip := range selectGlobalInterfaceIPs(addrs) {
				if ip.To4() != nil {
					hasV4 = true
				} else {
					hasV6 = true
				}
			}
		}
		if !hasV4 && !hasV6 {
			hasV4 = true
		}
		if hasV4 {
			if err := l.AddDefaultRouteDirect(ifname, false); err != nil {
				return err
			}
		}
		if hasV6 {
			if err := l.AddDefaultRouteDirect(ifname, true); err != nil {
				return err
			}
		}
		return nil
	}

	route := buildGatewayDefaultRoute(link.Attrs().Index, gateway, l.routeMetric())
	if err := netlink.RouteReplace(route); err != nil {
		if errors.Is(err, syscall.EINVAL) && route.Flags != 0 {
			fallback := *route
			fallback.Flags = 0
			if retryErr := netlink.RouteReplace(&fallback); retryErr == nil || errors.Is(retryErr, syscall.EEXIST) {
				return nil
			}
		}
		if !errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("failed to replace default route: %w", err)
		}
	}
	return nil
}

func (l *LinuxConfigurator) AddDefaultRouteDirect(ifname string, ipv6 bool) error {
	link, err := netlink.LinkByName(ifname)
	if err != nil {
		return fmt.Errorf("interface %s not found: %w", ifname, err)
	}

	route := buildDirectDefaultRoute(link.Attrs().Index, ipv6, l.routeMetric())
	if err := netlink.RouteReplace(route); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("failed to replace direct default route: %w", err)
	}
	return nil
}

func (l *LinuxConfigurator) FlushRoutes(ifname string) error {
	link, err := netlink.LinkByName(ifname)
	if err != nil {
		if isLinkNotFound(err) {
			return nil
		}
		return fmt.Errorf("interface %s not found: %w", ifname, err)
	}

	routes, err := netlink.RouteList(link, netlink.FAMILY_ALL)
	if err != nil {
		if isLinkNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to list routes: %w", err)
	}

	for _, route := range routes {
		_ = netlink.RouteDel(&route)
	}
	return nil
}

func (l *LinuxConfigurator) BringUp(ifname string) error {
	link, err := netlink.LinkByName(ifname)
	if err != nil {
		return fmt.Errorf("interface %s not found: %w", ifname, err)
	}
	return netlink.LinkSetUp(link)
}

func (l *LinuxConfigurator) BringDown(ifname string) error {
	link, err := netlink.LinkByName(ifname)
	if err != nil {
		return nil // Interface gone
	}
	return netlink.LinkSetDown(link)
}

func (l *LinuxConfigurator) SetMTU(ifname string, mtu int) error {
	link, err := netlink.LinkByName(ifname)
	if err != nil {
		return fmt.Errorf("interface %s not found: %w", ifname, err)
	}
	return netlink.LinkSetMTU(link, mtu)
}

func selectGlobalInterfaceIPs(addrs []netlink.Addr) []net.IP {
	var v4s []net.IP
	var v6s []net.IP
	for _, addr := range addrs {
		ip := addr.IP
		if len(ip) == 0 || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			if v4.IsLinkLocalUnicast() {
				continue
			}
			v4s = append(v4s, v4)
			continue
		}
		if v6 := ip.To16(); v6 != nil {
			if v6.IsLinkLocalUnicast() || v6.IsLinkLocalMulticast() {
				continue
			}
			v6s = append(v6s, v6)
		}
	}
	out := make([]net.IP, 0, len(v4s)+len(v6s))
	out = append(out, v4s...)
	out = append(out, v6s...)
	return out
}

func (l *LinuxConfigurator) GetCurrentIPs(ifname string) ([]net.IP, error) {
	link, err := netlink.LinkByName(ifname)
	if err != nil {
		return nil, fmt.Errorf("interface %s not found: %w", ifname, err)
	}

	addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return nil, fmt.Errorf("failed to list addresses: %w", err)
	}

	return selectGlobalInterfaceIPs(addrs), nil
}

func (l *LinuxConfigurator) GetCurrentIP(ifname string) (net.IP, error) {
	ips, err := l.GetCurrentIPs(ifname)
	if err != nil || len(ips) == 0 {
		return nil, err
	}
	return ips[0], nil
}

func (l *LinuxConfigurator) IsUp(ifname string) (bool, error) {
	link, err := netlink.LinkByName(ifname)
	if err != nil {
		return false, fmt.Errorf("interface %s not found: %w", ifname, err)
	}
	return link.Attrs().Flags&net.FlagUp != 0, nil
}

const (
	defaultResolvConfPath = "/etc/resolv.conf"
	defaultSysClassNetDir = "/sys/class/net"
	managedDNSBeginMarker = "# BEGIN quectel-qmi-go-dns"
	managedDNSEndMarker   = "# END quectel-qmi-go-dns"
	managedDNSBackupTag   = "# quectel-qmi-go-backup: "
)

func collectDNSServers(dns1, dns2 string) []string {
	var servers []string
	d1 := strings.TrimSpace(dns1)
	d2 := strings.TrimSpace(dns2)
	if d1 != "" {
		servers = append(servers, d1)
	}
	if d2 != "" && d2 != d1 {
		servers = append(servers, d2)
	}
	return servers
}

func splitResolvLines(data []byte) []string {
	raw := strings.ReplaceAll(string(data), "\r\n", "\n")
	raw = strings.TrimSuffix(raw, "\n")
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\n")
}

func resolveWritableResolvPath(path string) (string, error) {
	current := path
	for depth := 0; depth < 16; depth++ {
		fi, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				return current, nil
			}
			return "", err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			return current, nil
		}
		target, err := os.Readlink(current)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(current), target)
		}
		current = filepath.Clean(target)
	}
	return "", fmt.Errorf("too many symlink levels resolving %s", path)
}

func writeResolvFileSafe(path string, content []byte) error {
	realPath, err := resolveWritableResolvPath(path)
	if err != nil {
		return err
	}
	mode := os.FileMode(0644)
	if fi, statErr := os.Stat(realPath); statErr == nil {
		mode = fi.Mode().Perm()
	}
	dir := filepath.Dir(realPath)
	tmpFile, tmpErr := os.CreateTemp(dir, ".resolv.conf.qmi-*")
	if tmpErr != nil {
		// Fallback to direct write if directory does not permit temporary files (e.g. bind-mounted single file).
		return os.WriteFile(realPath, content, mode)
	}
	tmpName := tmpFile.Name()
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmpFile.Write(content); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Chmod(mode); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, realPath); err != nil {
		// Cross-device or bind-mount fallback: write directly to resolved target without clobbering symlink.
		return os.WriteFile(realPath, content, mode)
	}
	cleanupTmp = false
	return nil
}

func (l *LinuxConfigurator) UpdateDNS(dns1, dns2 string) error {
	return l.UpdateDNSServers(dns1, dns2)
}

func (l *LinuxConfigurator) UpdateDNSServers(rawServers ...string) error {
	servers := normalizeDNSServerList(rawServers...)
	if len(servers) == 0 {
		return nil
	}

	resolvConfMu.Lock()
	defer resolvConfMu.Unlock()

	path := l.resolvPath()
	var existingLines []string
	if data, err := os.ReadFile(path); err == nil {
		existingLines = splitResolvLines(data)
	} else if !os.IsNotExist(err) {
		return err
	}

	var kept []string
	inManagedBlock := false
	for _, line := range existingLines {
		trimmed := strings.TrimSpace(line)
		if trimmed == managedDNSBeginMarker {
			inManagedBlock = true
			continue
		}
		if trimmed == managedDNSEndMarker {
			inManagedBlock = false
			continue
		}
		if inManagedBlock {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) >= 2 && fields[0] == "nameserver" {
			kept = append(kept, managedDNSBackupTag+trimmed)
			continue
		}
		kept = append(kept, line)
	}

	out := make([]string, 0, len(kept)+len(servers)+2)
	out = append(out, kept...)
	out = append(out, managedDNSBeginMarker)
	for _, srv := range servers {
		out = append(out, "nameserver "+srv)
	}
	out = append(out, managedDNSEndMarker)

	content := strings.Join(out, "\n") + "\n"
	return writeResolvFileSafe(path, []byte(content))
}

func (l *LinuxConfigurator) RestoreDNS() error {
	resolvConfMu.Lock()
	defer resolvConfMu.Unlock()

	path := l.resolvPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	existingLines := splitResolvLines(data)
	var out []string
	inManagedBlock := false
	changed := false
	for _, line := range existingLines {
		trimmed := strings.TrimSpace(line)
		if trimmed == managedDNSBeginMarker {
			inManagedBlock = true
			changed = true
			continue
		}
		if trimmed == managedDNSEndMarker {
			inManagedBlock = false
			changed = true
			continue
		}
		if inManagedBlock {
			changed = true
			continue
		}
		if strings.HasPrefix(trimmed, managedDNSBackupTag) {
			restored := strings.TrimSpace(strings.TrimPrefix(trimmed, managedDNSBackupTag))
			if restored != "" {
				out = append(out, restored)
			}
			changed = true
			continue
		}
		out = append(out, line)
	}

	if !changed {
		return nil
	}

	content := ""
	if len(out) > 0 {
		content = strings.Join(out, "\n") + "\n"
	}
	return writeResolvFileSafe(path, []byte(content))
}

// ============================================================================
// QMAP 多路复用 sysfs 操作 (Linux 专属)
// ============================================================================

// AddQMAPMux 向内核 qmi_wwan 驱动申请创建一个 MUX ID 对应的虚拟网卡
// 等同于: echo {muxID} > /sys/class/net/{masterIface}/qmi/add_mux
// 返回值为创建出的虚拟网卡名，例如 "qmimux0"
func (l *LinuxConfigurator) AddQMAPMux(masterIface string, muxID uint8) (string, error) {
	sysNet := l.sysNetDir()
	addMuxPath := filepath.Join(sysNet, masterIface, "qmi", "add_mux")

	// 检查 sysfs 节点是否存在
	if _, err := os.Stat(addMuxPath); os.IsNotExist(err) {
		return "", fmt.Errorf("sysfs 节点 %s 不存在，内核驱动可能不支持 QMAP", addMuxPath)
	}

	// 写入 MuxID 触发内核创建虚拟网卡
	data := fmt.Sprintf("%d\n", muxID)
	if err := os.WriteFile(addMuxPath, []byte(data), 0200); err != nil {
		// 如果已经存在，可能返回 "device or resource busy" 之类的错误
		// 尝试检查对应的虚拟网卡是否已存在
		ifname := l.GetQMAPMuxIface(masterIface, muxID)
		if ifname != "" {
			return ifname, nil // 虚拟网卡已存在，直接使用
		}
		return "", fmt.Errorf("写入 %s 失败: %w", addMuxPath, err)
	}

	// 推导并验证虚拟网卡名
	ifname := l.GetQMAPMuxIface(masterIface, muxID)
	if ifname == "" {
		return "", fmt.Errorf("MuxID %d 的虚拟网卡创建后未找到", muxID)
	}

	return ifname, nil
}

// DelQMAPMux 销毁指定 MuxID 对应的虚拟网卡
// 等同于: echo {muxID} > /sys/class/net/{masterIface}/qmi/del_mux
func (l *LinuxConfigurator) DelQMAPMux(masterIface string, muxID uint8) error {
	delMuxPath := filepath.Join(l.sysNetDir(), masterIface, "qmi", "del_mux")

	if _, err := os.Stat(delMuxPath); os.IsNotExist(err) {
		return nil // 节点不存在就认为无需清理
	}

	data := fmt.Sprintf("%d\n", muxID)
	if err := os.WriteFile(delMuxPath, []byte(data), 0200); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", delMuxPath, err)
	}
	return nil
}

func matchesQMAPMuxID(raw string, muxID uint8) bool {
	val := strings.TrimSpace(raw)
	if val == "" {
		return false
	}
	parsed, err := strconv.ParseUint(val, 0, 8)
	if err == nil {
		return uint8(parsed) == muxID
	}
	return strings.EqualFold(val, fmt.Sprintf("0x%x", muxID)) ||
		strings.EqualFold(val, fmt.Sprintf("0x%02x", muxID)) ||
		val == fmt.Sprintf("%d", muxID)
}

func (l *LinuxConfigurator) isQMAPMuxBoundToMaster(muxIface, masterIface string) bool {
	masterIface = strings.TrimSpace(masterIface)
	muxIface = strings.TrimSpace(muxIface)
	if muxIface == "" || muxIface == masterIface {
		return false
	}
	if masterIface == "" {
		return true
	}

	sysNet := l.sysNetDir()
	muxDir := filepath.Join(sysNet, muxIface)
	masterDir := filepath.Join(sysNet, masterIface)

	// 1. Direct sysfs netdev upper/lower relationship:
	//    /sys/class/net/<muxIface>/lower_<masterIface> or /sys/class/net/<masterIface>/upper_<muxIface>
	if _, err := os.Lstat(filepath.Join(muxDir, "lower_"+masterIface)); err == nil {
		return true
	}
	if _, err := os.Lstat(filepath.Join(masterDir, "upper_"+muxIface)); err == nil {
		return true
	}

	// If <muxIface> explicitly points to a lower_<otherMaster>, it belongs to another modem.
	if muxEntries, err := os.ReadDir(muxDir); err == nil {
		for _, e := range muxEntries {
			if strings.HasPrefix(e.Name(), "lower_") {
				return e.Name() == "lower_"+masterIface
			}
		}
	}

	// 2. Netlink IFLA_LINK (ParentIndex == master.Index)
	if l != nil && l.parentIndexFn != nil {
		if matches, known := l.parentIndexFn(muxIface, masterIface); known {
			return matches
		}
	} else if sysNet == defaultSysClassNetDir {
		masterLink, mErr := netlink.LinkByName(masterIface)
		muxLink, xErr := netlink.LinkByName(muxIface)
		if mErr == nil && xErr == nil && masterLink != nil && muxLink != nil {
			mAttrs := masterLink.Attrs()
			xAttrs := muxLink.Attrs()
			if mAttrs != nil && xAttrs != nil && xAttrs.ParentIndex > 0 && mAttrs.Index > 0 {
				return xAttrs.ParentIndex == mAttrs.Index
			}
		}
	}

	// 3. Shared underlying USB interface device in sysfs (/sys/class/net/<iface>/device)
	muxDev, err1 := filepath.EvalSymlinks(filepath.Join(muxDir, "device"))
	masterDev, err2 := filepath.EvalSymlinks(filepath.Join(masterDir, "device"))
	if err1 == nil && err2 == nil && muxDev != "" && muxDev == masterDev {
		return true
	}
	if err1 == nil {
		if masterReal, errM := filepath.EvalSymlinks(masterDir); errM == nil && muxDev == masterReal {
			return true
		}
	}

	return false
}

// GetQMAPMuxIface 根据 masterIface 和 MuxID 查找归属于该物理网卡的 QMAP 虚拟网卡接口名
func (l *LinuxConfigurator) GetQMAPMuxIface(masterIface string, muxID uint8) string {
	if muxID == 0 {
		return ""
	}
	sysNet := l.sysNetDir()
	entries, err := os.ReadDir(sysNet)
	if err != nil {
		return ""
	}

	masterIface = strings.TrimSpace(masterIface)

	// 优先扫描绑定到 masterIface 且存在 qmap/mux_id 匹配的接口
	var boundWithoutMuxID []string
	for _, entry := range entries {
		name := entry.Name()
		if name == masterIface {
			continue
		}
		if !strings.HasPrefix(name, "qmimux") && masterIface == "" {
			continue
		}
		if !l.isQMAPMuxBoundToMaster(name, masterIface) {
			continue
		}
		muxIDPath := filepath.Join(sysNet, name, "qmap", "mux_id")
		if data, err := os.ReadFile(muxIDPath); err == nil {
			if matchesQMAPMuxID(string(data), muxID) {
				return name
			}
			continue
		} else if os.IsNotExist(err) && strings.HasPrefix(name, "qmimux") {
			boundWithoutMuxID = append(boundWithoutMuxID, name)
		}
	}

	// 兼容旧版内核未暴露 qmap/mux_id 属性的情况：
	// 仅在不存在 qmap/mux_id 且明确归属于 masterIface（或未指定 masterIface）的候选接口中选择。
	candidate := fmt.Sprintf("qmimux%d", muxID-1)
	for _, name := range boundWithoutMuxID {
		if name == candidate {
			return name
		}
	}
	if masterIface != "" && len(boundWithoutMuxID) == 1 {
		return boundWithoutMuxID[0]
	}

	return ""
}

// EnableRawIP 在物理网卡上开启 Raw IP 模式（QMAP 前置条件）
// 等同于: echo Y > /sys/class/net/{ifname}/qmi/raw_ip
func (l *LinuxConfigurator) EnableRawIP(ifname string) error {
	rawIPPath := filepath.Join(l.sysNetDir(), ifname, "qmi", "raw_ip")

	if _, err := os.Stat(rawIPPath); os.IsNotExist(err) {
		return nil // 内核驱动不支持，跳过
	}

	// 检查是否已经开启
	if content, err := os.ReadFile(rawIPPath); err == nil {
		s := strings.TrimSpace(string(content))
		if s == "Y" || s == "y" || s == "1" {
			return nil // 已开启
		}
	}

	// 开启前需要先关闭网卡
	_ = l.BringDown(ifname)

	if err := os.WriteFile(rawIPPath, []byte("Y\n"), 0644); err != nil {
		return fmt.Errorf("开启 raw_ip 失败: %w", err)
	}

	return nil
}
