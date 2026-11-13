package netcfg

import (
	"net"
	"reflect"
	"sync"
	"testing"
)

type stubRegressionConfigurator struct {
	mu      sync.Mutex
	ips     []net.IP
	servers []string
	upCalls int
}

func (s *stubRegressionConfigurator) SetIPAddress(string, net.IP, int) error   { return nil }
func (s *stubRegressionConfigurator) SetIPv6Address(string, net.IP, int) error { return nil }
func (s *stubRegressionConfigurator) FlushAddresses(string) error              { return nil }
func (s *stubRegressionConfigurator) AddDefaultRoute(string, net.IP) error     { return nil }
func (s *stubRegressionConfigurator) AddDefaultRouteDirect(string, bool) error { return nil }
func (s *stubRegressionConfigurator) FlushRoutes(string) error                 { return nil }
func (s *stubRegressionConfigurator) BringUp(string) error {
	s.mu.Lock()
	s.upCalls++
	s.mu.Unlock()
	return nil
}
func (s *stubRegressionConfigurator) BringDown(string) error    { return nil }
func (s *stubRegressionConfigurator) SetMTU(string, int) error  { return nil }
func (s *stubRegressionConfigurator) IsUp(string) (bool, error) { return true, nil }
func (s *stubRegressionConfigurator) RestoreDNS() error         { return nil }
func (s *stubRegressionConfigurator) AddQMAPMux(string, uint8) (string, error) {
	return "qmimux0", nil
}
func (s *stubRegressionConfigurator) DelQMAPMux(string, uint8) error       { return nil }
func (s *stubRegressionConfigurator) GetQMAPMuxIface(string, uint8) string { return "qmimux0" }
func (s *stubRegressionConfigurator) EnableRawIP(string) error             { return nil }
func (s *stubRegressionConfigurator) GetCurrentIP(string) (net.IP, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.ips) == 0 {
		return nil, nil
	}
	return s.ips[0], nil
}
func (s *stubRegressionConfigurator) GetCurrentIPs(string) ([]net.IP, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]net.IP, len(s.ips))
	copy(out, s.ips)
	return out, nil
}
func (s *stubRegressionConfigurator) UpdateDNS(dns1, dns2 string) error {
	return s.UpdateDNSServers(dns1, dns2)
}
func (s *stubRegressionConfigurator) UpdateDNSServers(servers ...string) error {
	s.mu.Lock()
	s.servers = append([]string(nil), servers...)
	s.mu.Unlock()
	return nil
}

// MEDIUM-5 & HIGH-10: Concurrent GetConfigurator/SetConfigurator/ResetConfigurator without data race,
// plus MultiIPConfigurator & MultiDNSConfigurator dual-stack delegation.
func TestMedium5AndHigh10ConcurrentConfiguratorAndMultiIPDNS(t *testing.T) {
	orig := GetConfigurator()
	defer SetConfigurator(orig)

	stub := &stubRegressionConfigurator{
		ips: []net.IP{net.ParseIP("2409:8088::abcd")},
	}
	SetConfigurator(stub)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if id%4 == 0 {
					SetConfigurator(stub)
				} else if id%7 == 0 && j%10 == 0 {
					ResetConfigurator()
					SetConfigurator(stub)
				} else {
					_ = GetConfigurator()
					_, _ = GetCurrentIPs("wwan0")
					_ = BringUp("wwan0")
				}
			}
		}(i)
	}
	wg.Wait()

	SetConfigurator(stub)
	if err := UpdateResolvConfServers("1.1.1.1", "0.0.0.0", "::", "8.8.8.8", "2409:8088::1", "2409:8088::2", "1.1.1.1"); err != nil {
		t.Fatalf("UpdateResolvConfServers() error = %v", err)
	}
	wantServers := []string{"1.1.1.1", "8.8.8.8", "2409:8088::1", "2409:8088::2"}
	if !reflect.DeepEqual(stub.servers, wantServers) {
		t.Fatalf("UpdateResolvConfServers() delegated %v, want %v", stub.servers, wantServers)
	}

	gotIPs, err := GetCurrentIPs("wwan0")
	if err != nil || len(gotIPs) != 1 || !gotIPs[0].Equal(net.ParseIP("2409:8088::abcd")) {
		t.Fatalf("GetCurrentIPs() = (%v, %v), want [2409:8088::abcd]", gotIPs, err)
	}
}
