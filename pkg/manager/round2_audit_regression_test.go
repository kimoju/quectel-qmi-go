package manager

import (
	"bytes"
	"context"
	"errors"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kimoju/quectel-qmi-go/pkg/qmi"
)

// CRITICAL-4: handleIndication(EventNewMessage) with nil Packet must never panic and emits EventNewSMS sentinel.
func TestCritical4HandleIndicationEventNewMessageNilPacketDoesNotPanic(t *testing.T) {
	m := newRecoveryTestManager()
	gotCh := make(chan Event, 1)
	m.OnEvent(func(e Event) {
		if e.Type == EventNewSMS {
			select {
			case gotCh <- e:
			default:
			}
		}
	})

	m.handleIndication(qmi.Event{
		Type:      qmi.EventNewMessage,
		ServiceID: qmi.ServiceWMS,
		MessageID: qmi.WMSEventReportInd,
		Packet:    nil,
	})

	select {
	case evt := <-gotCh:
		if evt.SMSIndex != 0xFFFFFFFF {
			t.Fatalf("SMSIndex = 0x%x, want 0xFFFFFFFF", evt.SMSIndex)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for EventNewSMS")
	}
}

// HIGH-2: normalizeConfig defaults EnableIPv4=true when dial is enabled and both families are false,
// and never enables SyncOnOpen when UseProxy=true.
func TestHigh2NormalizeConfigDefaultsAndClientOptions(t *testing.T) {
	cfg := normalizeConfig(Config{
		APN:           "internet",
		ClientOptions: qmi.ClientOptions{UseProxy: true, SyncOnOpen: true},
	})
	if !cfg.EnableIPv4 {
		t.Fatal("normalizeConfig(Config{APN:\"internet\"}) left EnableIPv4=false; must default EnableIPv4=true when dial is enabled")
	}
	if cfg.ClientOptions.SyncOnOpen {
		t.Fatal("normalizeConfig kept ClientOptions.SyncOnOpen=true when UseProxy=true")
	}
	if cfg.ClientOptions.ReadDeadline <= 0 || cfg.ClientOptions.DefaultRequestTimeout <= 0 {
		t.Fatalf("normalizeConfig left zero ClientOptions timeouts: %+v", cfg.ClientOptions)
	}
}

// HIGH-2: Dual-stack doConnect and RotateIP succeed on IPv6-only carriers when IPv4 fails,
// configure IPv6 address + route + MTU + IPv6 DNS, and restore DNS on disconnect.
func TestHigh2SymmetricDualStackIPv6OnlyCarrierConnectRotateAndDNSRestore(t *testing.T) {
	mockNet := &recordingNetConfigurator{}
	installMockNetConfigurator(t, mockNet)

	m := New(Config{
		Device:     ModemDevice{NetInterface: "wwan0"},
		APN:        "cmnet",
		EnableIPv4: true,
		EnableIPv6: true,
	}, NewNopLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.ctx = ctx
	m.cancel = cancel
	m.client = &qmi.Client{}
	m.wds = &qmi.WDSService{}
	m.wdsV6 = &qmi.WDSService{}
	m.wda = &qmi.WDAService{}
	m.desiredConnection = true

	var v6Gen atomic.Int32
	m.startWDSNetworkInterfaceHook = func(_ context.Context, _ *qmi.WDSService, family uint8) (uint32, error) {
		if family == qmi.IpFamilyV4 {
			return 0, errors.New("IPv4 PDN unsupported on IPv6-only carrier")
		}
		n := v6Gen.Add(1)
		return uint32(0x600 + n), nil
	}
	var stoppedHandles []uint32
	m.stopWDSNetworkInterfaceHook = func(_ context.Context, _ *qmi.WDSService, handle uint32) error {
		stoppedHandles = append(stoppedHandles, handle)
		return nil
	}
	m.getWDSRuntimeSettingsHook = func(_ context.Context, _ *qmi.WDSService, family uint8) (*qmi.RuntimeSettings, error) {
		if family != qmi.IpFamilyV6 {
			return nil, errors.New("out of call")
		}
		ipStr := "2409:8088::1001"
		if v6Gen.Load() >= 2 {
			ipStr = "2409:8088::2002"
		}
		return &qmi.RuntimeSettings{
			IPv6Address: net.ParseIP(ipStr),
			IPv6Prefix:  64,
			IPv6Gateway: net.ParseIP("fe80::1"),
			IPv6DNS1:    net.ParseIP("2409:8088::8"),
			IPv6DNS2:    net.ParseIP("2409:8088::9"),
			MTU:         1480,
		}, nil
	}

	if err := m.doConnect(); err != nil {
		t.Fatalf("doConnect() on IPv6-only carrier with dual-stack config failed: %v", err)
	}
	if m.State() != StateConnected {
		t.Fatalf("State() = %v, want StateConnected", m.State())
	}
	if m.handleV4 != 0 || m.handleV6 != 0x601 {
		t.Fatalf("handles = (%#x, %#x), want (0, 0x601)", m.handleV4, m.handleV6)
	}
	wantDNS := []string{"2409:8088::8", "2409:8088::9"}
	if !reflect.DeepEqual(mockNet.dnsServers, wantDNS) {
		t.Fatalf("configured DNS = %v, want %v", mockNet.dnsServers, wantDNS)
	}
	if len(mockNet.setMTUs) == 0 || mockNet.setMTUs[0] != 1480 {
		t.Fatalf("configured MTUs = %v, want [1480]", mockNet.setMTUs)
	}

	// RotateIP must also succeed symmetrically when IPv4 fails and IPv6 succeeds!
	if err := m.RotateIP(); err != nil {
		t.Fatalf("RotateIP() on IPv6-only carrier failed: %v", err)
	}
	st := m.Settings()
	if st == nil || !st.IPv6Address.Equal(net.ParseIP("2409:8088::2002")) {
		t.Fatalf("Settings() after RotateIP = %+v, want IPv6 2409:8088::2002", st)
	}

	// doDisconnect must stop active handle and call RestoreDNS()!
	m.doDisconnect()
	if mockNet.dnsRestoreCalls == 0 {
		t.Fatal("doDisconnect() did not call RestoreDNS()")
	}
}

// HIGH-2: Dual-stack partial leg drop preserves live IPv6 without wiping addresses,
// full drop stops active handles + restores DNS, and verifyIPConsistency detects IPv6 drift & OS IP wipe.
func TestHigh2DualStackStatusCheckPartialDropFullDropAndVerifyIPConsistency(t *testing.T) {
	mockNet := &recordingNetConfigurator{}
	installMockNetConfigurator(t, mockNet)

	m := New(Config{
		Device:        ModemDevice{NetInterface: "wwan0"},
		EnableIPv4:    true,
		EnableIPv6:    true,
		AutoReconnect: false,
	}, NewNopLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.ctx = ctx
	m.cancel = cancel
	m.client = &qmi.Client{}
	m.wds = &qmi.WDSService{}
	m.wdsV6 = &qmi.WDSService{}
	m.state = StateConnected
	m.desiredConnection = true
	m.handleV4 = 0x111
	m.handleV6 = 0x222
	m.dnsConfigured = true
	_ = mockNet.SetIPAddress("wwan0", net.ParseIP("10.10.10.2"), 32)
	_ = mockNet.SetIPv6Address("wwan0", net.ParseIP("2409:8088::77"), 64)
	m.settings = &qmi.RuntimeSettings{
		IPv4Address: net.ParseIP("10.10.10.2"),
		IPv6Address: net.ParseIP("2409:8088::77"),
		IPv6Prefix:  64,
	}

	v4Status := qmi.StatusDisconnected // IPv4 leg dropped!
	v6Status := qmi.StatusConnected    // IPv6 leg still alive!
	m.queryWDSPacketServiceStateHook = func(_ context.Context, _ *qmi.WDSService, family uint8) (qmi.ConnectionStatus, error) {
		if family == qmi.IpFamilyV4 {
			return v4Status, nil
		}
		return v6Status, nil
	}
	var stopped []uint32
	m.stopWDSNetworkInterfaceHook = func(_ context.Context, _ *qmi.WDSService, handle uint32) error {
		stopped = append(stopped, handle)
		return nil
	}
	modemIPv6 := net.ParseIP("2409:8088::77")
	m.getWDSRuntimeSettingsHook = func(_ context.Context, _ *qmi.WDSService, family uint8) (*qmi.RuntimeSettings, error) {
		if family == qmi.IpFamilyV6 {
			return &qmi.RuntimeSettings{IPv6Address: modemIPv6, IPv6Prefix: 64}, nil
		}
		return nil, errors.New("out of call")
	}

	// 1. Partial drop (IPv4 down, IPv6 up): must stop handleV4 ONLY, keep StateConnected & handleV6, and NOT FlushAddresses!
	m.doStatusCheck(false)
	if m.State() != StateConnected || m.handleV4 != 0 || m.handleV6 != 0x222 {
		t.Fatalf("after IPv4 partial drop: state=%v handles=(%#x,%#x), want Connected (0, 0x222)", m.State(), m.handleV4, m.handleV6)
	}
	if !reflect.DeepEqual(stopped, []uint32{0x111}) {
		t.Fatalf("stopped handles on partial drop = %v, want [0x111]", stopped)
	}
	if len(mockNet.flushedAddresses) != 0 {
		t.Fatalf("partial IPv4 drop unexpectedly flushed interface addresses (wiping live IPv6!): %v", mockNet.flushedAddresses)
	}

	// 2. verifyIPConsistency passes while modem & OS interface match IPv6, fails on modem IPv6 drift, and fails on OS IP wipe.
	if err := m.verifyIPConsistency(); err != nil {
		t.Fatalf("verifyIPConsistency() error = %v, want nil", err)
	}
	modemIPv6 = net.ParseIP("2409:8088::9999")
	if err := m.verifyIPConsistency(); err == nil {
		t.Fatal("verifyIPConsistency() did not detect modem IPv6 address drift")
	}
	modemIPv6 = net.ParseIP("2409:8088::77")
	_ = mockNet.FlushAddresses("wwan0")
	if err := m.verifyIPConsistency(); err == nil {
		t.Fatal("verifyIPConsistency() did not detect wiped OS interface IPv6 address")
	}

	// 3. Full drop (IPv6 also disconnects): must stop handleV6 and restore DNS.
	v6Status = qmi.StatusDisconnected
	m.doStatusCheck(false)
	if m.State() != StateDisconnected || m.handleV6 != 0 {
		t.Fatalf("after full drop: state=%v handleV6=%#x, want Disconnected, 0", m.State(), m.handleV6)
	}
	if !reflect.DeepEqual(stopped, []uint32{0x111, 0x222}) {
		t.Fatalf("stopped handles after full drop = %v, want [0x111, 0x222]", stopped)
	}
	if mockNet.dnsRestoreCalls == 0 {
		t.Fatal("full connection drop did not call RestoreDNS()")
	}
}

// HIGH-3: Stop() cancels doRecoverFromModemReset via m.ctx immediately, StartCoreContext vs Stop() has no race,
// and Settings() returns a deep clone.
func TestHigh3RecoveryRespectsManagerContextStartStopRaceAndSettingsClone(t *testing.T) {
	t.Run("Stop cancels in-flight doRecoverFromModemReset immediately", func(t *testing.T) {
		m := New(Config{
			Device: ModemDevice{ControlPath: "/dev/cdc-wdm0"},
		}, NewNopLogger())
		ctx, cancel := context.WithCancel(context.Background())
		m.ctx = ctx
		m.cancel = cancel

		enteredOpen := make(chan struct{})
		m.openClientAndAllocateServicesHook = func(rctx context.Context) error {
			close(enteredOpen)
			<-rctx.Done()
			return rctx.Err()
		}

		recoverDone := make(chan bool, 1)
		go func() {
			recoverDone <- m.doRecoverFromModemReset()
		}()

		select {
		case <-enteredOpen:
		case <-time.After(time.Second):
			t.Fatal("doRecoverFromModemReset did not enter openClientAndAllocateServicesHook")
		}

		start := time.Now()
		if err := m.Stop(); err != nil {
			t.Fatalf("Stop() error = %v", err)
		}
		select {
		case ok := <-recoverDone:
			if ok {
				t.Fatal("doRecoverFromModemReset returned true after Stop()")
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatal("doRecoverFromModemReset ignored Stop() cancellation")
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("Stop() took %v, want fast cancellation", elapsed)
		}
		if m.IsCoreReady() {
			t.Fatal("IsCoreReady() = true after Stop()")
		}
	})

	t.Run("Concurrent StartCoreContext vs Stop race safety", func(t *testing.T) {
		for i := 0; i < 10; i++ {
			m := New(Config{Device: ModemDevice{ControlPath: "/dev/cdc-wdm0"}}, NewNopLogger())
			m.openClientAndAllocateServicesHook = func(ctx context.Context) error {
				time.Sleep(2 * time.Millisecond)
				return ctx.Err()
			}
			m.checkSIMHook = func() error { return nil }
			m.getICCIDStrictHook = func(context.Context) (string, error) { return "898600", nil }

			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				_ = m.StartCoreContext(context.Background())
			}()
			go func() {
				defer wg.Done()
				time.Sleep(time.Millisecond)
				_ = m.Stop()
			}()
			wg.Wait()
			_ = m.Stop()
		}
	})

	t.Run("Settings returns deep copy of IP slices", func(t *testing.T) {
		m := newRecoveryTestManager()
		m.settings = &qmi.RuntimeSettings{
			IPv4Address: net.IP{10, 1, 2, 3},
			IPv6Address: net.ParseIP("2409:8088::1"),
		}
		got := m.Settings()
		got.IPv4Address[3] = 99
		got.IPv6Address[15] = 0xEE
		again := m.Settings()
		if !again.IPv4Address.Equal(net.IP{10, 1, 2, 3}) || !again.IPv6Address.Equal(net.ParseIP("2409:8088::1")) {
			t.Fatalf("mutating Settings() corrupted manager internal state: %+v", again)
		}
	})
}

// HIGH-8: EF_SMSP TS-SCA offset (len-15 vs TP-DA at len-27), ISO 7816-4 logical channel CLA encoding,
// and Case-3 SELECT 6Cxx APDU retry appending Le instead of overwriting FileID[1].
func TestHigh8SMSPRecordOffsetLogicalChannelCLAAndCase36CxxRetry(t *testing.T) {
	// Build 40-byte (Alpha-ID = 12B) and 42-byte (Alpha-ID = 14B) EF_SMSP records
	// where TP-DestinationAddress (base+1..12) holds recipient +8613900009999
	// and TS-ServiceCentreAddress (base+13..24) holds SMSC +8613800100500!
	tpDA := []byte{0x08, 0x91, 0x68, 0x31, 0x09, 0x00, 0x90, 0x99, 0xF9, 0xFF, 0xFF, 0xFF}  // +8613900009999
	tsSCA := []byte{0x08, 0x91, 0x68, 0x31, 0x08, 0x10, 0x00, 0x05, 0xF0, 0xFF, 0xFF, 0xFF} // +8613800100500

	for _, alphaLen := range []int{12, 14} {
		rec := bytes.Repeat([]byte{0xFF}, alphaLen+28)
		base := alphaLen
		rec[base] = 0xFD // bit 1 == 0 -> TS-SCA present; bit 0 == 1
		copy(rec[base+1:base+13], tpDA)
		copy(rec[base+13:base+25], tsSCA)
		got := parseSMSCFromSMSPRecord(rec)
		if got != "+8613800100500" {
			t.Fatalf("parseSMSCFromSMSPRecord(alphaLen=%d) = %q, want \"+8613800100500\" (must never return TP-DA +8613900009999)", alphaLen, got)
		}

		// Parameter Indicators bit 1 == 1 means TS-SCA absent -> must return "".
		recAbsent := append([]byte(nil), rec...)
		recAbsent[base] = 0x02
		if gotAbsent := parseSMSCFromSMSPRecord(recAbsent); gotAbsent != "" {
			t.Fatalf("parseSMSCFromSMSPRecord(TS-SCA absent) = %q, want empty", gotAbsent)
		}
	}

	// ISO 7816-4 logical channel CLA encoding: ch 1..3 -> 0x01..0x03; ch 4..19 -> 0x40..0x4F.
	if got := encodeISO7816LogicalChannelCLA(0x00, 2); got != 0x02 {
		t.Fatalf("encodeISO7816LogicalChannelCLA(0x00, 2) = 0x%02X, want 0x02", got)
	}
	if got := encodeISO7816LogicalChannelCLA(0x00, 5); got != 0x41 {
		t.Fatalf("encodeISO7816LogicalChannelCLA(0x00, 5) = 0x%02X, want 0x41", got)
	}

	// Case-3 SELECT (00 A4 00 04 02 6F 42) receiving 6C 1E must retry with 8-byte 00 A4 00 04 02 6F 42 1E.
	var sentAPDUs [][]byte
	selectCmd := []byte{0x00, 0xA4, 0x00, 0x04, 0x02, 0x6F, 0x42}
	rsp, err := transmitBasicAPDUWithFollowUp(func(apdu []byte) ([]byte, error) {
		sentAPDUs = append(sentAPDUs, append([]byte(nil), apdu...))
		if len(sentAPDUs) == 1 {
			return []byte{0x6C, 0x1E}, nil
		}
		return []byte{0x62, 0x00, 0x90, 0x00}, nil
	}, selectCmd)
	if err != nil {
		t.Fatalf("transmitBasicAPDUWithFollowUp() error = %v", err)
	}
	if len(rsp) < 2 || len(sentAPDUs) != 2 {
		t.Fatalf("unexpected APDU exchange: sent=%X rsp=%X", sentAPDUs, rsp)
	}
	wantRetry := []byte{0x00, 0xA4, 0x00, 0x04, 0x02, 0x6F, 0x42, 0x1E}
	if !bytes.Equal(sentAPDUs[1], wantRetry) {
		t.Fatalf("Case-3 6C1E retry APDU = % X, want % X (must append Le, not overwrite FileID 0x42)", sentAPDUs[1], wantRetry)
	}
}

// HIGH-9: QMAP BindMuxDataPort uses discovered BusType/InterfaceNumber (including InterfaceNumber=0 and PCIe EpType=0x03),
// fails dial fatally when BindMuxDataPort errors, and openClientAndAllocateServices skips raw_ip in lazy/disabled data-plane policy.
func TestHigh9QMAPMuxBindingFatalErrorAndGuardedRawIP(t *testing.T) {
	mockNet := &recordingNetConfigurator{}
	installMockNetConfigurator(t, mockNet)

	m := New(Config{
		Device: ModemDevice{
			NetInterface:       "rmnet_mhi0",
			InterfaceNumber:    0,
			HasInterfaceNumber: true,
			BusType:            "pcie",
		},
		MuxID:      2,
		EnableIPv4: true,
	}, NewNopLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.ctx = ctx
	m.cancel = cancel
	m.client = &qmi.Client{}
	m.wds = &qmi.WDSService{}
	m.wda = &qmi.WDAService{}
	m.desiredConnection = true

	var gotBinding qmi.MuxBinding
	bindErr := errors.New("QMI_ERR_INVALID_ARG")
	m.bindMuxDataPortHook = func(_ context.Context, _ *qmi.WDSService, b qmi.MuxBinding) error {
		gotBinding = b
		return bindErr
	}
	dialCalled := false
	m.startWDSNetworkInterfaceHook = func(context.Context, *qmi.WDSService, uint8) (uint32, error) {
		dialCalled = true
		return 1, nil
	}

	err := m.doConnect()
	if !errors.Is(err, bindErr) {
		t.Fatalf("doConnect() error = %v, want wrapped %v", err, bindErr)
	}
	if gotBinding.EpType != 0x03 || gotBinding.EpIfID != 0 || gotBinding.MuxID != 2 {
		t.Fatalf("MuxBinding = %+v, want EpType=3 (PCIe), EpIfID=0, MuxID=2", gotBinding)
	}
	if dialCalled {
		t.Fatal("doConnect() dialed on unbound default port after BindMuxDataPort failed!")
	}

	// openClientAndAllocateServices with DataPlanePolicyLazy (StartCore default) must NOT touch raw_ip!
	mockNet.rawIPEnabled = nil
	coreMgr := New(Config{
		Device:          ModemDevice{NetInterface: "wwan0", ControlPath: "/dev/cdc-wdm0"},
		DataPlanePolicy: DataPlanePolicyLazy,
	}, NewNopLogger())
	coreMgr.newClientHook = func(context.Context, string, qmi.ClientOptions) (*qmi.Client, error) {
		return nil, context.Canceled
	}
	cancelCtx, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	_ = coreMgr.openClientAndAllocateServices(cancelCtx)
	if len(mockNet.rawIPEnabled) != 0 {
		t.Fatalf("openClientAndAllocateServices(DataPlanePolicyLazy) enabled raw_ip unexpectedly: %v", mockNet.rawIPEnabled)
	}
}

// MEDIUM-6: ModemPool SetLogger concurrency, control-only modem keying, negative-dBm SignalStrengthSelector,
// LeastUsedSelector pruning, StopAll stopping loops, and hotplug PoolEvents.
func TestMedium6ModemPoolConcurrencySelectorsKeyingAndHotplugEvents(t *testing.T) {
	pool := NewPool()

	// 1. Concurrent SetLogger + logger() reads must be race-free.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				pool.SetLogger(NewNopLogger())
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				pool.logger().Debug("ping")
			}
		}()
	}
	wg.Wait()

	// 2. Control-only modems (NetInterface == "") must have distinct pool keys by ControlPath.
	k1 := modemPoolKey(ModemDevice{ControlPath: "/dev/cdc-wdm0"})
	k2 := modemPoolKey(ModemDevice{ControlPath: "/dev/cdc-wdm1"})
	if k1 == "" || k2 == "" || k1 == k2 {
		t.Fatalf("control-only modemPoolKey collision: k1=%q k2=%q", k1, k2)
	}

	// 3. SignalStrengthSelector must reject RSSI=0 (unmeasured) and RSSI=+125 (0x7D unknown), picking -65 dBm!
	mZero := newRecoveryTestManager()
	mZero.querySignalStrength = func(context.Context) (*qmi.SignalStrength, error) {
		return &qmi.SignalStrength{RSSI: 0}, nil
	}
	mUnknown := newRecoveryTestManager()
	mUnknown.querySignalStrength = func(context.Context) (*qmi.SignalStrength, error) {
		return &qmi.SignalStrength{RSSI: 125}, nil
	}
	mGood := newRecoveryTestManager()
	mGood.querySignalStrength = func(context.Context) (*qmi.SignalStrength, error) {
		return &qmi.SignalStrength{RSSI: -65}, nil
	}
	sigSel := &SignalStrengthSelector{}
	if got := sigSel.Select([]*Manager{mZero, mUnknown, mGood}); got != mGood {
		t.Fatalf("SignalStrengthSelector picked %p, want mGood (-65 dBm) instead of 0/+125", got)
	}

	// 4. LeastUsedSelector prunes removed *Manager entries so hot-unplugged modems do not leak.
	luSel := NewLeastUsedSelector()
	_ = luSel.Select([]*Manager{mZero, mGood})
	_ = luSel.Select([]*Manager{mGood})
	if len(luSel.lastUsed) != 1 {
		t.Fatalf("LeastUsedSelector retained %d entries after pruning mZero, want 1", len(luSel.lastUsed))
	}

	// 5. scanForChanges emits PoolEventModemAdded & PoolEventModemRemoved, and StopAll stops hotplug/health loops.
	prevDiscoverer := getDeviceDiscoverer()
	defer SetDeviceDiscoverer(prevDiscoverer)

	var discMu sync.Mutex
	currentDevs := []ModemDevice{{NetInterface: "wwan9", ControlPath: "/dev/cdc-wdm9"}}
	SetDeviceDiscoverer(func() ([]ModemDevice, error) {
		discMu.Lock()
		defer discMu.Unlock()
		return append([]ModemDevice(nil), currentDevs...), nil
	})

	var poolEvents []PoolEventType
	var evMu sync.Mutex
	pool.OnPoolEvent(func(ev PoolEvent) {
		evMu.Lock()
		poolEvents = append(poolEvents, ev.Type)
		evMu.Unlock()
	})
	pool.baseCfg = Config{NoDial: true}
	pool.baseLogger = NewNopLogger()

	pool.scanForChanges()
	discMu.Lock()
	currentDevs = nil
	discMu.Unlock()
	// Note: empty discover error is ignored, whereas returning non-nil slice with 0 matching or replacing device triggers removal:
	discMu.Lock()
	currentDevs = []ModemDevice{{NetInterface: "wwan10", ControlPath: "/dev/cdc-wdm10"}}
	discMu.Unlock()
	pool.scanForChanges()

	evMu.Lock()
	gotPoolEvs := append([]PoolEventType(nil), poolEvents...)
	evMu.Unlock()
	sawAdded, sawRemoved := false, false
	for _, et := range gotPoolEvs {
		if et == PoolEventModemAdded {
			sawAdded = true
		}
		if et == PoolEventModemRemoved {
			sawRemoved = true
		}
	}
	if !sawAdded || !sawRemoved {
		t.Fatalf("scanForChanges pool events = %v, want both PoolEventModemAdded and PoolEventModemRemoved", gotPoolEvs)
	}

	pool.StartHealthMonitor(time.Hour)
	pool.WatchHotPlug(Config{NoDial: true}, NewNopLogger(), time.Hour)
	pool.StopAll()
	pool.mu.RLock()
	stillRunning := pool.healthCtx != nil || pool.hotplugCtx != nil
	pool.mu.RUnlock()
	if stillRunning {
		t.Fatal("StopAll() did not stop healthLoop / hotplugLoop contexts")
	}
}
