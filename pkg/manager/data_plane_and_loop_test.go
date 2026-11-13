package manager

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kimoju/quectel-qmi-go/pkg/netcfg"
	"github.com/kimoju/quectel-qmi-go/pkg/qmi"
)

type recordingNetConfigurator struct {
	mu               sync.Mutex
	flushedAddresses []string
	flushedRoutes    []string
	broughtDown      []string
	broughtUp        []string
	setIPv4Err       error
	setIPv6Err       error
	currentIP        net.IP
	currentIPs       []net.IP
	dnsServers       []string
	dnsRestoreCalls  int
	defaultRoutes    []net.IP
	directRoutesV4   int
	directRoutesV6   int
	setMTUs          []int
	rawIPEnabled     []string
}

func (r *recordingNetConfigurator) SetIPAddress(ifname string, ip net.IP, prefixLen int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.setIPv4Err != nil {
		return r.setIPv4Err
	}
	cp := append(net.IP(nil), ip...)
	r.currentIP = cp
	r.replaceFamilyIPLocked(cp)
	return nil
}

func (r *recordingNetConfigurator) SetIPv6Address(ifname string, ip net.IP, prefixLen int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.setIPv6Err != nil {
		return r.setIPv6Err
	}
	cp := append(net.IP(nil), ip...)
	if r.currentIP == nil {
		r.currentIP = cp
	}
	r.replaceFamilyIPLocked(cp)
	return nil
}

func (r *recordingNetConfigurator) replaceFamilyIPLocked(ip net.IP) {
	isV4 := ip.To4() != nil
	kept := r.currentIPs[:0]
	for _, existing := range r.currentIPs {
		if (existing.To4() != nil) != isV4 {
			kept = append(kept, existing)
		}
	}
	r.currentIPs = append(kept, ip)
}

func (r *recordingNetConfigurator) FlushAddresses(ifname string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushedAddresses = append(r.flushedAddresses, ifname)
	r.currentIP = nil
	r.currentIPs = nil
	return nil
}

func (r *recordingNetConfigurator) AddDefaultRoute(ifname string, gateway net.IP) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defaultRoutes = append(r.defaultRoutes, append(net.IP(nil), gateway...))
	return nil
}

func (r *recordingNetConfigurator) AddDefaultRouteDirect(ifname string, ipv6 bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ipv6 {
		r.directRoutesV6++
	} else {
		r.directRoutesV4++
	}
	return nil
}

func (r *recordingNetConfigurator) FlushRoutes(ifname string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushedRoutes = append(r.flushedRoutes, ifname)
	return nil
}

func (r *recordingNetConfigurator) BringUp(ifname string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.broughtUp = append(r.broughtUp, ifname)
	return nil
}

func (r *recordingNetConfigurator) BringDown(ifname string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.broughtDown = append(r.broughtDown, ifname)
	return nil
}

func (r *recordingNetConfigurator) SetMTU(ifname string, mtu int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setMTUs = append(r.setMTUs, mtu)
	return nil
}

func (r *recordingNetConfigurator) GetCurrentIP(ifname string) (net.IP, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.currentIP, nil
}

func (r *recordingNetConfigurator) GetCurrentIPs(ifname string) ([]net.IP, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]net.IP, len(r.currentIPs))
	for i, ip := range r.currentIPs {
		out[i] = append(net.IP(nil), ip...)
	}
	return out, nil
}

func (r *recordingNetConfigurator) IsUp(ifname string) (bool, error) { return true, nil }

func (r *recordingNetConfigurator) UpdateDNS(dns1, dns2 string) error {
	var servers []string
	if dns1 != "" {
		servers = append(servers, dns1)
	}
	if dns2 != "" {
		servers = append(servers, dns2)
	}
	return r.UpdateDNSServers(servers...)
}

func (r *recordingNetConfigurator) UpdateDNSServers(servers ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dnsServers = append([]string(nil), servers...)
	return nil
}

func (r *recordingNetConfigurator) RestoreDNS() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dnsRestoreCalls++
	r.dnsServers = nil
	return nil
}

func (r *recordingNetConfigurator) AddQMAPMux(string, uint8) (string, error) {
	return "qmimux0", nil
}
func (r *recordingNetConfigurator) DelQMAPMux(string, uint8) error       { return nil }
func (r *recordingNetConfigurator) GetQMAPMuxIface(string, uint8) string { return "qmimux0" }
func (r *recordingNetConfigurator) EnableRawIP(ifname string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rawIPEnabled = append(r.rawIPEnabled, ifname)
	return nil
}

func installMockNetConfigurator(t *testing.T, mock *recordingNetConfigurator) {
	t.Helper()
	prev := netcfg.GetConfigurator()
	netcfg.SetConfigurator(mock)
	t.Cleanup(func() {
		netcfg.SetConfigurator(prev)
	})
}

func TestEventLoopAndStopDoNotSelfDeadlockWhenEventChannelFull(t *testing.T) {
	mockNet := &recordingNetConfigurator{}
	installMockNetConfigurator(t, mockNet)

	m := New(Config{
		Device:        ModemDevice{NetInterface: "wwan0"},
		EnableIPv4:    true,
		AutoReconnect: true,
		HealthPolicy: HealthPolicy{
			FullCheckInterval:  10 * time.Millisecond,
			IndicationDebounce: 5 * time.Millisecond,
		},
	}, NewNopLogger())

	ctx, cancel := context.WithCancel(context.Background())
	m.ctx = ctx
	m.cancel = cancel
	m.client = &qmi.Client{}
	m.wds = &qmi.WDSService{}
	m.wda = &qmi.WDAService{}
	m.nas = &qmi.NASService{}
	m.state = StateConnected
	m.desiredConnection = true

	var connectCalls atomic.Int32
	m.querySignalStrength = func(context.Context) (*qmi.SignalStrength, error) {
		return &qmi.SignalStrength{RSSI: -70}, nil
	}
	m.queryServingSystem = func(context.Context) (*qmi.ServingSystem, error) {
		return &qmi.ServingSystem{RegistrationState: qmi.RegStateRegistered}, nil
	}
	m.queryPacketServiceState = func(context.Context) (qmi.ConnectionStatus, error) {
		// Force doStatusCheck to observe disconnect and call requestStart()!
		return qmi.StatusDisconnected, nil
	}
	m.startWDSNetworkInterfaceHook = func(context.Context, *qmi.WDSService, uint8) (uint32, error) {
		connectCalls.Add(1)
		return 0x101, nil
	}
	m.stopWDSNetworkInterfaceHook = func(context.Context, *qmi.WDSService, uint32) error {
		return nil
	}
	m.configureNetworkHook = func() error {
		return nil
	}

	// Stuff eventCh completely full (16/16) BEFORE starting eventLoop, and simultaneously
	// hammer enqueueEvent / requestStart / scheduleTargetedCheck from external goroutines.
	for len(m.eventCh) < cap(m.eventCh) {
		m.eventCh <- eventCheckFull
	}

	m.wg.Add(1)
	go m.eventLoop()

	hammerDone := make(chan struct{})
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for {
			select {
			case <-hammerDone:
				return
			default:
				m.enqueueEvent(eventPacketStatusChanged)
				m.requestStart()
				m.scheduleTargetedCheck()
				time.Sleep(time.Millisecond)
			}
		}
	}()

	time.Sleep(60 * time.Millisecond)
	close(hammerDone)

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- m.Stop()
	}()

	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop() returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop() deadlocked waiting for eventLoop / external senders")
	}

	if connectCalls.Load() == 0 {
		t.Fatal("expected doStatusCheck disconnect recovery to trigger doConnect via requestStart()")
	}
}

func TestDoDisconnectFlushesMuxIfaceInsteadOfMasterIface(t *testing.T) {
	mockNet := &recordingNetConfigurator{}
	installMockNetConfigurator(t, mockNet)

	m := newRecoveryTestManager()
	m.cfg = normalizeConfig(Config{
		Device:     ModemDevice{NetInterface: "wwan1"},
		MuxID:      1,
		EnableIPv4: true,
	})
	m.state = StateConnected
	m.muxIface = "qmimux1"
	m.wds = &qmi.WDSService{}
	m.handleV4 = 0x55
	var stoppedHandles []uint32
	m.stopWDSNetworkInterfaceHook = func(_ context.Context, _ *qmi.WDSService, h uint32) error {
		stoppedHandles = append(stoppedHandles, h)
		return nil
	}

	m.doDisconnect()

	if len(stoppedHandles) != 1 || stoppedHandles[0] != 0x55 {
		t.Fatalf("stoppedHandles = %v, want [0x55]", stoppedHandles)
	}
	mockNet.mu.Lock()
	defer mockNet.mu.Unlock()
	if len(mockNet.flushedAddresses) != 1 || mockNet.flushedAddresses[0] != "qmimux1" {
		t.Fatalf("FlushAddresses called with %v, want [qmimux1] (not master wwan1)", mockNet.flushedAddresses)
	}
	if len(mockNet.flushedRoutes) != 1 || mockNet.flushedRoutes[0] != "qmimux1" {
		t.Fatalf("FlushRoutes called with %v, want [qmimux1]", mockNet.flushedRoutes)
	}
	if len(mockNet.broughtDown) != 1 || mockNet.broughtDown[0] != "qmimux1" {
		t.Fatalf("BringDown called with %v, want [qmimux1]", mockNet.broughtDown)
	}
}

func TestConnectRejectsConcurrentConnectingUnconditionally(t *testing.T) {
	m := newRecoveryTestManager()
	m.cfg = normalizeConfig(Config{EnableIPv4: true})
	m.state = StateConnecting
	m.desiredConnection = true
	m.handleV4 = 0
	m.handleV6 = 0

	if err := m.Connect(); err == nil {
		t.Fatal("Connect() while StateConnecting (with handleV4==0) succeeded, want error")
	}
	if err := m.doConnect(); err == nil {
		t.Fatal("doConnect() while StateConnecting (with handleV4==0) succeeded, want error")
	}
}

func TestDoConnectStopsActivePDPHandlesWhenConfigureNetworkFails(t *testing.T) {
	mockNet := &recordingNetConfigurator{}
	installMockNetConfigurator(t, mockNet)

	m := newRecoveryTestManager()
	m.cfg = normalizeConfig(Config{
		Device:        ModemDevice{NetInterface: "wwan0"},
		EnableIPv4:    true,
		EnableIPv6:    true,
		AutoReconnect: false,
	})
	m.client = &qmi.Client{}
	m.wds = &qmi.WDSService{}
	m.wdsV6 = &qmi.WDSService{}
	m.wda = &qmi.WDAService{}
	m.nas = &qmi.NASService{}
	m.desiredConnection = true
	m.querySignalStrength = func(context.Context) (*qmi.SignalStrength, error) { return nil, errors.New("skip") }
	m.queryServingSystem = func(context.Context) (*qmi.ServingSystem, error) { return nil, errors.New("skip") }

	m.startWDSNetworkInterfaceHook = func(_ context.Context, _ *qmi.WDSService, family uint8) (uint32, error) {
		if family == qmi.IpFamilyV4 {
			return 0xAA04, nil
		}
		return 0xBB06, nil
	}
	var stopped []uint32
	m.stopWDSNetworkInterfaceHook = func(_ context.Context, _ *qmi.WDSService, handle uint32) error {
		stopped = append(stopped, handle)
		return nil
	}
	cfgErr := errors.New("netlink address apply failed")
	m.configureNetworkHook = func() error {
		return cfgErr
	}

	err := m.doConnect()
	if !errors.Is(err, cfgErr) {
		t.Fatalf("doConnect() error = %v, want %v", err, cfgErr)
	}
	if len(stopped) != 2 || stopped[0] != 0xAA04 || stopped[1] != 0xBB06 {
		t.Fatalf("stopped PDP handles on configureNetwork failure = %#v, want [0xaa04, 0xbb06]", stopped)
	}
	if m.handleV4 != 0 || m.handleV6 != 0 {
		t.Fatalf("handles after failed doConnect = (%#x, %#x), want (0, 0)", m.handleV4, m.handleV6)
	}
	if m.State() != StateDisconnected {
		t.Fatalf("state = %s, want %s", m.State(), StateDisconnected)
	}
}

func TestOpenClientAndAllocateServicesReleasesAllocatedServicesOnMidWayFailure(t *testing.T) {
	m := newRecoveryTestManager()
	m.cfg = normalizeConfig(Config{
		Device:          ModemDevice{ControlPath: "/dev/cdc-wdm9", NetInterface: "wwan0"},
		EnableIPv4:      true,
		DataPlanePolicy: DataPlanePolicyEager,
		DisableWMSInd:   true,
		DisableVOICEInd: true,
	})

	m.newClientHook = func(context.Context, string, qmi.ClientOptions) (*qmi.Client, error) {
		return &qmi.Client{}, nil
	}
	m.newWDSService = func(context.Context, *qmi.Client) (*qmi.WDSService, error) {
		return &qmi.WDSService{}, nil
	}
	m.newWDAService = func(context.Context, *qmi.Client) (*qmi.WDAService, error) {
		return &qmi.WDAService{}, nil
	}
	m.enableRawIPHook = func(context.Context) error { return nil }
	nasReady := make(chan struct{})
	uimReady := make(chan struct{})
	m.newNASService = func(context.Context, *qmi.Client) (*qmi.NASService, error) {
		close(nasReady)
		return &qmi.NASService{}, nil
	}
	m.registerNASIndications = func(context.Context, qmi.NASIndicationRegistration) error {
		return nil
	}
	m.newUIMService = func(context.Context, *qmi.Client) (*qmi.UIMService, error) {
		close(uimReady)
		return &qmi.UIMService{}, nil
	}
	m.registerUIMIndications = func(context.Context) (uint32, error) {
		return 0, nil
	}
	// Wait until NAS and UIM have been allocated, then fail DMS with a non-timeout error!
	m.newDMSService = func(context.Context, *qmi.Client) (*qmi.DMSService, error) {
		<-nasReady
		<-uimReady
		return nil, qmi.ErrServiceNotSupported
	}

	var closedMu sync.Mutex
	closedServices := make(map[string]int)
	m.closeServiceHook = func(svc string) {
		closedMu.Lock()
		closedServices[svc]++
		closedMu.Unlock()
	}

	err := m.openClientAndAllocateServices(context.Background())
	if !errors.Is(err, qmi.ErrServiceNotSupported) {
		t.Fatalf("openClientAndAllocateServices() error = %v, want ErrServiceNotSupported", err)
	}

	for _, wantClosed := range []string{"WDS", "WDA", "NAS", "UIM"} {
		if closedServices[wantClosed] != 1 {
			t.Fatalf("service %s closed %d times on partial allocation failure, want 1 (closed=%v)", wantClosed, closedServices[wantClosed], closedServices)
		}
	}
	if m.wds != nil || m.wda != nil || m.nas != nil || m.uim != nil || m.client != nil {
		t.Fatal("expected manager service/client fields to be cleared after failed openClientAndAllocateServices")
	}
}

func TestRotateIPIPv6OnlyAndNilWDSGuards(t *testing.T) {
	mockNet := &recordingNetConfigurator{}
	installMockNetConfigurator(t, mockNet)

	t.Run("both wds and wdsV6 nil returns clean error without panic", func(t *testing.T) {
		m := newRecoveryTestManager()
		m.cfg = normalizeConfig(Config{EnableIPv4: true})
		m.state = StateConnected
		m.wds = nil
		m.wdsV6 = nil

		if err := m.RotateIP(); err == nil {
			t.Fatal("RotateIP() with nil WDS services succeeded, want error")
		}
	})

	t.Run("IPv6-only mode rotates IPv6 session without nil panic on m.wds", func(t *testing.T) {
		m := newRecoveryTestManager()
		m.cfg = normalizeConfig(Config{
			Device:     ModemDevice{NetInterface: "wwan0"},
			EnableIPv4: false,
			EnableIPv6: true,
			MuxID:      1,
		})
		m.muxIface = "qmimux0"
		m.state = StateConnected
		m.wds = nil
		m.wdsV6 = &qmi.WDSService{}
		m.handleV6 = 0x6001
		oldIPv6 := net.ParseIP("2409:8900:1::1")
		newIPv6 := net.ParseIP("2409:8900:2::2")
		m.settings = &qmi.RuntimeSettings{
			IPv6Address: oldIPv6,
			IPv6Prefix:  64,
		}

		var stoppedHandles []uint32
		m.stopWDSNetworkInterfaceHook = func(_ context.Context, _ *qmi.WDSService, h uint32) error {
			stoppedHandles = append(stoppedHandles, h)
			return nil
		}
		m.startWDSNetworkInterfaceHook = func(_ context.Context, _ *qmi.WDSService, family uint8) (uint32, error) {
			if family != qmi.IpFamilyV6 {
				t.Fatalf("unexpected dial family 0x%02x in IPv6-only RotateIP", family)
			}
			return 0x6002, nil
		}
		m.getWDSRuntimeSettingsHook = func(_ context.Context, _ *qmi.WDSService, family uint8) (*qmi.RuntimeSettings, error) {
			if family != qmi.IpFamilyV6 {
				t.Fatalf("unexpected runtime settings family 0x%02x in IPv6-only RotateIP", family)
			}
			return &qmi.RuntimeSettings{
				IPv6Address: newIPv6,
				IPv6Prefix:  64,
			}, nil
		}

		var ipChangedEvt atomic.Pointer[Event]
		m.events.On(func(ev Event) {
			if ev.Type == EventIPChanged {
				cp := ev
				ipChangedEvt.Store(&cp)
			}
		})
		defer m.events.Close()

		if err := m.RotateIP(); err != nil {
			t.Fatalf("RotateIP() IPv6-only error = %v", err)
		}
		if len(stoppedHandles) != 1 || stoppedHandles[0] != 0x6001 {
			t.Fatalf("stoppedHandles = %#v, want [0x6001]", stoppedHandles)
		}
		if m.handleV6 != 0x6002 {
			t.Fatalf("m.handleV6 = %#x, want 0x6002", m.handleV6)
		}
		gotSettings := m.Settings()
		if gotSettings == nil || !gotSettings.IPv6Address.Equal(newIPv6) {
			t.Fatalf("Settings().IPv6Address = %v, want %v", gotSettings, newIPv6)
		}

		deadline := time.Now().Add(time.Second)
		for ipChangedEvt.Load() == nil && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if ipChangedEvt.Load() == nil {
			t.Fatal("expected EventIPChanged to be emitted after IPv6 RotateIP")
		}
	})
}

func TestModemPoolRemoveDoesNotHoldLockAcrossBlockingStop(t *testing.T) {
	pool := NewPool()
	defer pool.StopAll()

	ctx, cancel := context.WithCancel(context.Background())
	slowMgr := New(Config{
		Device: ModemDevice{NetInterface: "slow0"},
	}, NewNopLogger())
	slowMgr.ctx = ctx
	slowMgr.cancel = cancel
	slowMgr.state = StateConnected

	// Hold an active wg worker on slowMgr so slowMgr.Stop() blocks until we release it!
	stopEntered := make(chan struct{})
	releaseStop := make(chan struct{})
	slowMgr.wg.Add(1)
	go func() {
		defer slowMgr.wg.Done()
		<-ctx.Done()
		close(stopEntered)
		<-releaseStop
	}()

	pool.mu.Lock()
	pool.modems["slow0"] = slowMgr
	pool.mu.Unlock()

	removeDone := make(chan error, 1)
	go func() {
		removeDone <- pool.Remove("slow0")
	}()

	select {
	case <-stopEntered:
	case <-time.After(time.Second):
		t.Fatal("slowMgr.Stop() did not start inside pool.Remove()")
	}

	// While slowMgr.Stop() is blocked, pool read/write operations MUST NOT block!
	sizeDone := make(chan int, 1)
	go func() {
		sizeDone <- pool.Size()
	}()

	select {
	case sz := <-sizeDone:
		if sz != 0 {
			t.Fatalf("pool.Size() = %d while Remove() stopping removed modem, want 0", sz)
		}
	case <-time.After(200 * time.Millisecond):
		close(releaseStop)
		t.Fatal("ModemPool.mu was held across blocking mgr.Stop()!")
	}

	close(releaseStop)
	if err := <-removeDone; err != nil {
		t.Fatalf("pool.Remove() error = %v", err)
	}
}
