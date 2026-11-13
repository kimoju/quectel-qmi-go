package qmi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
	"unicode/utf16"
)

type shortWriteRawMock struct {
	writeCalls int
}

func (m *shortWriteRawMock) Read(p []byte) (int, error) {
	time.Sleep(20 * time.Millisecond)
	return 0, io.EOF
}

func (m *shortWriteRawMock) Write(p []byte) (int, error) {
	m.writeCalls++
	if len(p) > 4 {
		return len(p) / 2, nil
	}
	return len(p), nil
}

func (m *shortWriteRawMock) SetReadDeadline(time.Time) error { return nil }
func (m *shortWriteRawMock) Close() error                    { return nil }

func marshalServiceResponsePacket(service uint8, clientID uint8, txID uint16, msgID uint16, tlvs []TLV) []byte {
	var tlvBytes []byte
	for i := range tlvs {
		tlvBytes = append(tlvBytes, tlvs[i].Marshal()...)
	}
	svcH := ServiceHeader{
		ControlFlags:  0x02, // Response
		TransactionID: txID,
		MessageID:     msgID,
		Length:        uint16(len(tlvBytes)),
	}
	body := append(svcH.Marshal(), tlvBytes...)
	qmuxH := QmuxHeader{
		IFType:       0x01,
		Length:       uint16(len(body) + 5),
		ControlFlags: 0x80,
		ServiceType:  service,
		ClientID:     clientID,
	}
	return append(qmuxH.Marshal(), body...)
}

func newQMIServiceTestHarness(t *testing.T, service uint8, clientID uint8, handler func(req *Packet) (*Packet, error)) *Client {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = serverConn.Close()
	})

	opts := normalizeClientOptions(ClientOptions{
		ReadDeadline:          10 * time.Millisecond,
		DefaultRequestTimeout: time.Second,
	})
	client := newClientWithTransport("/dev/cdc-wdm-test", opts, clientConn)
	client.addClientID(service, clientID)
	t.Cleanup(func() {
		_ = client.Close()
	})

	go func() {
		defer serverConn.Close()
		buf := make([]byte, 8192)
		for {
			n, err := serverConn.Read(buf)
			if err != nil {
				return
			}
			req, err := UnmarshalPacket(buf[:n])
			if err != nil {
				return
			}
			resp, err := handler(req)
			if err != nil || resp == nil {
				return
			}
			wire := marshalServiceResponsePacket(req.ServiceType, req.ClientID, req.TransactionID, req.MessageID, resp.TLVs)
			if _, err := serverConn.Write(wire); err != nil {
				return
			}
		}
	}()

	return client
}

// CRITICAL-1: normalizeClientOptions defaults & UseProxy never SyncOnOpen;
// fatal read errors fail pending + subsequent SendRequest immediately and emit EventModemReset;
// raw transport rejects partial writes without sending sliced fragments.
func TestCritical1ClientOptionsFatalDisconnectAndAtomicRawWrite(t *testing.T) {
	zeroOpts := normalizeClientOptions(ClientOptions{})
	if zeroOpts.ReadDeadline <= 0 || zeroOpts.DefaultRequestTimeout <= 0 || zeroOpts.TxQueueSize <= 0 || zeroOpts.IndicationQueueSize <= 0 {
		t.Fatalf("normalizeClientOptions(ClientOptions{}) left zero defaults: %+v", zeroOpts)
	}

	proxyOpts := normalizeClientOptions(ClientOptions{UseProxy: true, SyncOnOpen: true})
	if proxyOpts.SyncOnOpen {
		t.Fatal("normalizeClientOptions(UseProxy:true, SyncOnOpen:true) kept SyncOnOpen=true; CTLSync must NEVER run over qmi-proxy")
	}

	// Fatal disconnect wakes pending request + fails subsequent SendRequest fast + emits EventModemReset.
	clientConn, serverConn := net.Pipe()
	client := newClientWithTransport("/dev/cdc-wdm-dead", normalizeClientOptions(ClientOptions{
		ReadDeadline:          10 * time.Millisecond,
		DefaultRequestTimeout: 30 * time.Second,
	}), clientConn)
	defer client.Close()

	reqDone := make(chan error, 1)
	go func() {
		_, err := client.SendRequest(context.Background(), ServiceNAS, 1, NASGetServingSystem, nil)
		reqDone <- err
	}()

	reqBuf := make([]byte, 256)
	if _, err := serverConn.Read(reqBuf); err != nil {
		t.Fatalf("server Read() error = %v", err)
	}
	// Simulate abrupt USB unplug / qmi-proxy crash!
	_ = serverConn.Close()

	select {
	case err := <-reqDone:
		if err == nil {
			t.Fatal("pending SendRequest succeeded on dead transport, want fatal error")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("pending SendRequest hung after transport disconnect")
	}

	start := time.Now()
	_, subseqErr := client.SendRequest(context.Background(), ServiceNAS, 1, NASGetServingSystem, nil)
	if subseqErr == nil {
		t.Fatal("subsequent SendRequest succeeded on dead client, want fast failure")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("subsequent SendRequest took %v on dead client, want immediate failure", elapsed)
	}

	select {
	case evt := <-client.Events():
		if evt.Type != EventModemReset {
			t.Fatalf("emitted event type = %v, want EventModemReset", evt.Type)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected EventModemReset indication on fatal transport disconnect")
	}

	// Atomic raw write check: partial write on raw transport must return ErrShortWrite without slicing a 2nd write.
	shortMock := &shortWriteRawMock{}
	rawClient := newClientWithTransport("/dev/cdc-wdm-short", normalizeClientOptions(ClientOptions{
		UseProxy:              false,
		ReadDeadline:          50 * time.Millisecond,
		DefaultRequestTimeout: time.Second,
	}), shortMock)
	defer rawClient.Close()

	_, writeErr := rawClient.SendRequest(context.Background(), ServiceDMS, 1, DMSGetOperatingMode, nil)
	if !errors.Is(writeErr, io.ErrShortWrite) {
		t.Fatalf("raw partial write error = %v, want io.ErrShortWrite", writeErr)
	}
	if shortMock.writeCalls != 1 {
		t.Fatalf("raw partial write called Write() %d times, want 1 atomic attempt", shortMock.writeCalls)
	}
}

// CRITICAL-2: All 4 UIM PIN methods must include mandatory Session Information TLV 0x01 and PIN/PUK payload in TLV 0x02.
func TestCritical2UIMPINMethodsIncludeMandatorySessionTLV0x01AndPayloadTLV0x02(t *testing.T) {
	var seen []*Packet
	client := newQMIServiceTestHarness(t, ServiceUIM, 3, func(req *Packet) (*Packet, error) {
		seen = append(seen, req)
		return &Packet{TLVs: []TLV{successResultTLV()}}, nil
	})
	uim := &UIMService{client: client, clientID: 3}
	ctx := context.Background()
	const pinID1 uint8 = 1

	if err := uim.VerifyPIN(ctx, pinID1, "1234"); err != nil {
		t.Fatalf("VerifyPIN() error = %v", err)
	}
	if err := uim.SetPINProtection(ctx, pinID1, true, "1234"); err != nil {
		t.Fatalf("SetPINProtection() error = %v", err)
	}
	if err := uim.ChangePIN(ctx, pinID1, "1234", "5678"); err != nil {
		t.Fatalf("ChangePIN() error = %v", err)
	}
	if err := uim.UnblockPIN(ctx, pinID1, "12345678", "0000"); err != nil {
		t.Fatalf("UnblockPIN() error = %v", err)
	}

	if len(seen) != 4 {
		t.Fatalf("captured %d UIM PIN requests, want 4", len(seen))
	}
	wantMsgs := []uint16{UIMVerifyPIN, UIMSetPINProtection, UIMChangePIN, UIMUnblockPIN}
	for i, req := range seen {
		if req.MessageID != wantMsgs[i] {
			t.Fatalf("req[%d].MessageID = 0x%04x, want 0x%04x", i, req.MessageID, wantMsgs[i])
		}
		sess := FindTLV(req.TLVs, 0x01)
		if sess == nil || !bytes.Equal(sess.Value, []byte{UIMSessionTypePrimaryGWProvisioning, 0x00}) {
			t.Fatalf("req[%d] (0x%04x) missing/invalid Session TLV 0x01: %+v", i, req.MessageID, sess)
		}
		payload := FindTLV(req.TLVs, 0x02)
		if payload == nil || len(payload.Value) < 3 {
			t.Fatalf("req[%d] (0x%04x) missing/invalid PIN payload TLV 0x02: %+v", i, req.MessageID, payload)
		}
	}
}

// CRITICAL-3: 5G NR signal parsing in parseSignalInfoPacket (TLV 0x17 = 4B [RSRP, SNR], TLV 0x18 = 2B [RSRQ], -32768 unmeasured sentinel).
func TestCritical3NASSignalInfo5GNRWireLayoutAndUnmeasuredSentinel(t *testing.T) {
	tlv17 := make([]byte, 4)
	rsrp := int16(-95)
	sinr := int16(125)
	binary.LittleEndian.PutUint16(tlv17[0:2], uint16(rsrp))
	binary.LittleEndian.PutUint16(tlv17[2:4], uint16(sinr))
	tlv18 := make([]byte, 2)
	rsrq := int16(-11)
	binary.LittleEndian.PutUint16(tlv18[0:2], uint16(rsrq))

	info, err := parseSignalInfoPacket(&Packet{
		TLVs: []TLV{
			{Type: 0x17, Value: tlv17},
			{Type: 0x18, Value: tlv18},
		},
	})
	if err != nil {
		t.Fatalf("parseSignalInfoPacket() error = %v", err)
	}
	if info.NR5GRSRP != -95 || info.NR5GSINR != 125 || info.NR5GRSRQ != -11 {
		t.Fatalf("5G NR signal = %+v, want RSRP=-95 RSRQ=-11 SINR=125", info)
	}

	// Unmeasured -32768 (0x8000) sentinel must not populate bogus -32768 dBm values.
	unmeasured17 := []byte{0x00, 0x80, 0x00, 0x80}
	unmeasured18 := []byte{0x00, 0x80}
	sentinelInfo, err := parseSignalInfoPacket(&Packet{
		TLVs: []TLV{
			{Type: 0x17, Value: unmeasured17},
			{Type: 0x18, Value: unmeasured18},
		},
	})
	if err != nil {
		t.Fatalf("parseSignalInfoPacket(sentinel) error = %v", err)
	}
	if sentinelInfo.NR5GRSRP != 0 || sentinelInfo.NR5GSINR != 0 || sentinelInfo.NR5GRSRQ != 0 {
		t.Fatalf("sentinel 0x8000 leaked into NASSignalInfo: %+v", sentinelInfo)
	}
}

// HIGH-1: Multi-client IDs per service, per-ClientID/session coalescingKey, and CSVT 0x1B not misrouted as WDS.
func TestHigh1MultiClientTrackingCoalescingKeyAndCSVTRouting(t *testing.T) {
	c := &Client{}
	c.addClientID(ServiceWDS, 0x01)
	c.addClientID(ServiceWDS, 0x02)
	ids := c.GetClientIDs(ServiceWDS)
	if len(ids) != 2 || ids[0] != 0x01 || ids[1] != 0x02 {
		t.Fatalf("GetClientIDs(ServiceWDS) = %v, want [1 2]", ids)
	}
	c.removeClientID(ServiceWDS, 0x01)
	ids = c.GetClientIDs(ServiceWDS)
	if len(ids) != 1 || ids[0] != 0x02 || c.GetClientID(ServiceWDS) != 0x02 {
		t.Fatalf("after releasing cid 1, GetClientIDs(ServiceWDS) = %v, want [2]", ids)
	}

	kV4, okV4 := c.coalescingKey(Event{
		Type: EventPacketServiceStatusChanged, ServiceID: ServiceWDS, MessageID: WDSGetPktSrvcStatusInd,
		Packet: &Packet{ServiceType: ServiceWDS, ClientID: 1, MessageID: WDSGetPktSrvcStatusInd},
	})
	kV6, okV6 := c.coalescingKey(Event{
		Type: EventPacketServiceStatusChanged, ServiceID: ServiceWDS, MessageID: WDSGetPktSrvcStatusInd,
		Packet: &Packet{ServiceType: ServiceWDS, ClientID: 2, MessageID: WDSGetPktSrvcStatusInd},
	})
	if !okV4 || !okV6 || kV4 == kV6 {
		t.Fatalf("dual-stack WDS indications collided in coalescingKey: v4=(%q,%v) v6=(%q,%v)", kV4, okV4, kV6, okV6)
	}

	kRev1, okRev1 := c.coalescingKey(Event{
		Type: EventModemReset, ServiceID: ServiceControl, MessageID: CTLRevokeClientIDInd,
		Packet: &Packet{TLVs: []TLV{{Type: 0x01, Value: []byte{ServiceWDS, 1}}}},
	})
	kRev2, okRev2 := c.coalescingKey(Event{
		Type: EventModemReset, ServiceID: ServiceControl, MessageID: CTLRevokeClientIDInd,
		Packet: &Packet{TLVs: []TLV{{Type: 0x01, Value: []byte{ServiceWDS, 2}}}},
	})
	if !okRev1 || !okRev2 || kRev1 == kRev2 {
		t.Fatalf("CTLRevokeClientIDInd collided in coalescingKey: %q vs %q", kRev1, kRev2)
	}

	kUIM1, _ := c.coalescingKey(Event{
		Type: EventUIMSessionClosed, ServiceID: ServiceUIM, MessageID: UIMSessionClosedInd,
		Packet: &Packet{ClientID: 1, TLVs: []TLV{{Type: 0x01, Value: []byte{0x00}}}},
	})
	kUIM2, _ := c.coalescingKey(Event{
		Type: EventUIMSessionClosed, ServiceID: ServiceUIM, MessageID: UIMSessionClosedInd,
		Packet: &Packet{ClientID: 1, TLVs: []TLV{{Type: 0x01, Value: []byte{0x02}}}},
	})
	if kUIM1 == kUIM2 {
		t.Fatalf("UIMSessionClosed indications for distinct sessions collided: %q", kUIM1)
	}

	// ServiceCSVT (0x1B) indication with MsgID 0x0022 must NOT be dispatched as EventPacketServiceStatusChanged.
	csvtClient := &Client{
		opts:    normalizeClientOptions(ClientOptions{}),
		eventCh: make(chan Event, 4),
	}
	csvtClient.dispatchIndication(&Packet{
		IsIndication: true,
		ServiceType:  ServiceCSVT,
		ClientID:     1,
		MessageID:    WDSGetPktSrvcStatusInd,
	})
	select {
	case evt := <-csvtClient.eventCh:
		if evt.Type == EventPacketServiceStatusChanged {
			t.Fatal("ServiceCSVT (0x1B) was misrouted as EventPacketServiceStatusChanged")
		}
	default:
		t.Fatal("expected dispatched event on eventCh")
	}
}

// HIGH-4: NASService.ConfigSignalInfoV2 (0x006C) wire TLVs verified against libqmi.
func TestHigh4NASConfigSignalInfoV2WireTLVs(t *testing.T) {
	var captured *Packet
	client := newQMIServiceTestHarness(t, ServiceNAS, 2, func(req *Packet) (*Packet, error) {
		captured = req
		return &Packet{TLVs: []TLV{successResultTLV()}}, nil
	})
	nas := &NASService{client: client, clientID: 2}

	err := nas.ConfigSignalInfoV2(context.Background(), NASSignalInfoConfigV2{
		LTEEnabled:              true,
		LTERSRPDelta:            20,
		LTERSRQDelta:            15,
		LTESNRDelta:             30,
		LTEReportRate:           1,
		LTEReportAveragePeriod:  2,
		HasLTEReport:            true,
		NR5GEnabled:             true,
		NR5GRSRPDelta:           25,
		NR5GRSRQDelta:           10,
		NR5GSINRDelta:           35,
		NR5GReportRate:          3,
		NR5GReportAveragePeriod: 4,
		HasNR5GReport:           true,
	})
	if err != nil {
		t.Fatalf("ConfigSignalInfoV2() error = %v", err)
	}
	if captured == nil || captured.MessageID != NASConfigSignalInfoV2 {
		t.Fatalf("captured packet = %+v, want MsgID 0x006C", captured)
	}

	assertU16 := func(id uint8, want uint16) {
		t.Helper()
		tlv := FindTLV(captured.TLVs, id)
		if tlv == nil || len(tlv.Value) != 2 || binary.LittleEndian.Uint16(tlv.Value) != want {
			t.Fatalf("TLV 0x%02x = %+v, want uint16(%d)", id, tlv, want)
		}
	}
	assertU16(0x25, 300) // LTE SNR Delta (0.1 dB)
	assertU16(0x27, 150) // LTE RSRQ Delta (0.1 dB)
	assertU16(0x29, 200) // LTE RSRP Delta (0.1 dBm)
	assertU16(0x34, 350) // NR5G SNR Delta (0.1 dB)
	assertU16(0x36, 250) // NR5G RSRP Delta (0.1 dBm)
	assertU16(0x39, 100) // NR5G RSRQ Delta (0.1 dB)

	lteRep := FindTLV(captured.TLVs, 0x2A)
	if lteRep == nil || !bytes.Equal(lteRep.Value, []byte{1, 2}) {
		t.Fatalf("LTE SigRptConfig TLV 0x2A = %+v, want [1 2]", lteRep)
	}
	nrRep := FindTLV(captured.TLVs, 0x37)
	if nrRep == nil || !bytes.Equal(nrRep.Value, []byte{3, 4}) {
		t.Fatalf("NR5G SigRptConfig TLV 0x37 = %+v, want [3 4]", nrRep)
	}
}

// HIGH-5: ParseNetworkRejectIndication (0x0068) mandatory TLVs 0x01, 0x02, 0x03 and optional 0x10 (5B PLMN) & 0x11 (CSGID).
func TestHigh5ParseNetworkRejectIndication3DigitMNC(t *testing.T) {
	plmn := make([]byte, 5)
	binary.LittleEndian.PutUint16(plmn[0:2], 310)
	binary.LittleEndian.PutUint16(plmn[2:4], 260)
	plmn[4] = 1 // 3-digit MNC

	info, err := ParseNetworkRejectIndication(&Packet{
		TLVs: []TLV{
			{Type: 0x01, Value: []byte{0x0C}},                   // NR5G
			{Type: 0x02, Value: []byte{0x02}},                   // PS
			{Type: 0x03, Value: []byte{0x0F}},                   // No suitable cells in TA
			{Type: 0x10, Value: plmn},                           // 310260
			{Type: 0x11, Value: []byte{0x78, 0x56, 0x34, 0x12}}, // CSGID
		},
	})
	if err != nil {
		t.Fatalf("ParseNetworkRejectIndication() error = %v", err)
	}
	if info.RadioInterface != 0x0C || info.ServiceDomain != 0x02 || info.RejectCause != 0x0F || info.PLMN != "310260" || !info.HasCSGID || info.CSGID != 0x12345678 {
		t.Fatalf("unexpected NASNetworkRejectInfo: %+v", info)
	}
}

// HIGH-6: OperatorName (0x0039/0x003A) TLVs 0x10/0x13 and GetPLMNName (0x0044) compound EONS TLV 0x10 GSM-7 & UCS-2.
func TestHigh6OperatorNameAndGetPLMNNameDecoding(t *testing.T) {
	// UCS-2 big-endian for "中国联通" in 5-byte NITZ header:
	// [encoding(1), country_initials(1), long_name(1), spare_bits(1), name_len(1), bytes...]
	cuRunes := utf16.Encode([]rune("中国联通"))
	nitzUCS2 := make([]byte, 5+len(cuRunes)*2)
	nitzUCS2[0] = 0x01 // UCS-2 UTF-16BE
	nitzUCS2[1] = 0x00 // country_initials
	nitzUCS2[2] = 0x01 // long_name
	nitzUCS2[3] = 0x00 // spare_bits
	nitzUCS2[4] = uint8(len(cuRunes) * 2)
	for i, u := range cuRunes {
		binary.BigEndian.PutUint16(nitzUCS2[5+i*2:], u)
	}

	opInfo, err := ParseOperatorNameIndication(&Packet{
		TLVs: []TLV{
			{Type: 0x10, Value: append([]byte{0x01, 0x07}, []byte("CHN-CUG")...)},
			{Type: 0x13, Value: nitzUCS2},
		},
	})
	if err != nil {
		t.Fatalf("ParseOperatorNameIndication() error = %v", err)
	}
	if opInfo.ServiceProviderName != "CHN-CUG" || opInfo.NITZOperatorName != "中国联通" {
		t.Fatalf("OperatorName = %+v, want SPN=CHN-CUG NITZ=中国联通", opInfo)
	}

	// GetPLMNName (0x0044) compound EONS TLV 0x10:
	// [spn_enc, spn_len, spn..., short_enc, short_ci, short_spare, short_len, short..., long_enc, long_ci, long_spare, long_len, long...]
	packGSM7 := func(s string) ([]byte, byte) {
		var out []byte
		acc, bits := 0, 0
		for i := 0; i < len(s); i++ {
			acc |= int(s[i]&0x7F) << bits
			bits += 7
			if bits >= 8 {
				out = append(out, byte(acc&0xFF))
				acc >>= 8
				bits -= 8
			}
		}
		spare := byte(0)
		if bits > 0 {
			out = append(out, byte(acc&0xFF))
			spare = byte(8 - bits)
		}
		return out, spare
	}
	spnPacked, _ := packGSM7("CMCC")
	cmShort := utf16.Encode([]rune("移动"))
	cmLong := utf16.Encode([]rune("中国移动"))
	var eons bytes.Buffer
	eons.WriteByte(0x00) // SPN GSM7 packed
	eons.WriteByte(byte(len(spnPacked)))
	eons.Write(spnPacked)
	eons.WriteByte(0x01) // Short UCS-2 UTF-16BE
	eons.WriteByte(0x00)
	eons.WriteByte(0x00)
	eons.WriteByte(uint8(len(cmShort) * 2))
	for _, u := range cmShort {
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], u)
		eons.Write(b[:])
	}
	eons.WriteByte(0x01) // Long UCS-2 UTF-16BE
	eons.WriteByte(0x00)
	eons.WriteByte(0x00)
	eons.WriteByte(uint8(len(cmLong) * 2))
	for _, u := range cmLong {
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], u)
		eons.Write(b[:])
	}

	client := newQMIServiceTestHarness(t, ServiceNAS, 4, func(req *Packet) (*Packet, error) {
		if req.MessageID != NASGetPLMNName {
			return nil, errors.New("unexpected msgID")
		}
		return &Packet{
			TLVs: []TLV{
				successResultTLV(),
				{Type: 0x10, Value: eons.Bytes()},
			},
		}, nil
	})
	nas := &NASService{client: client, clientID: 4}
	plmnName, err := nas.GetPLMNName(context.Background(), NASPLMNNameRequest{MCC: 460, MNC: 0})
	if err != nil {
		t.Fatalf("GetPLMNName() error = %v", err)
	}
	if plmnName.ServiceProviderName != "CMCC" || plmnName.ShortName != "移动" || plmnName.LongName != "中国移动" {
		t.Fatalf("GetPLMNName() = %+v, want SPN=CMCC Short=移动 Long=中国移动", plmnName)
	}
}

// HIGH-7: WDAService GetDataFormatDetails (0x17/0x18 = UlMaxDatagrams/UlMaxSize) & SetDataFormat round-trip.
func TestHigh7WDADataFormatUlAggregationAndEndpointRoundTrip(t *testing.T) {
	var setReq *Packet
	client := newQMIServiceTestHarness(t, ServiceWDA, 1, func(req *Packet) (*Packet, error) {
		switch req.MessageID {
		case WDAGetDataFormat:
			u32 := func(v uint32) []byte {
				b := make([]byte, 4)
				binary.LittleEndian.PutUint32(b, v)
				return b
			}
			return &Packet{
				TLVs: []TLV{
					successResultTLV(),
					{Type: 0x11, Value: u32(LinkProtocolIP)},
					{Type: 0x12, Value: u32(5)},
					{Type: 0x13, Value: u32(5)},
					{Type: 0x15, Value: u32(32)},
					{Type: 0x16, Value: u32(32768)},
					{Type: 0x17, Value: u32(16)},    // UlMaxDatagrams
					{Type: 0x18, Value: u32(16384)}, // UlMaxSize
				},
			}, nil
		case WDASetDataFormat:
			setReq = req
			return &Packet{TLVs: []TLV{successResultTLV()}}, nil
		default:
			return nil, errors.New("unexpected WDA msg")
		}
	})
	wda := &WDAService{client: client, clientID: 1}

	details, err := wda.GetDataFormatDetails(context.Background())
	if err != nil {
		t.Fatalf("GetDataFormatDetails() error = %v", err)
	}
	if details.UlMaxDatagrams != 16 || details.UlMaxSize != 16384 || details.EndpointType != 0 || details.EndpointID != 0 {
		t.Fatalf("GetDataFormatDetails() = %+v, want UlMaxDatagrams=16 UlMaxSize=16384 and zero Endpoint*", details)
	}

	df, err := wda.GetDataFormat(context.Background())
	if err != nil {
		t.Fatalf("GetDataFormat() error = %v", err)
	}
	if err := wda.SetDataFormat(context.Background(), *df); err != nil {
		t.Fatalf("SetDataFormat(round-trip) error = %v", err)
	}
	if FindTLV(setReq.TLVs, 0x17) != nil {
		t.Fatalf("SetDataFormat(round-trip) unexpectedly sent bogus Peripheral Endpoint TLV 0x17: %+v", setReq.TLVs)
	}
}

// MEDIUM-1: UIM EF_SPN 1-byte display_condition, UCS-2 characters ending in 0x00/0xFF ('一' U+4E00, 'ÿ' U+00FF), and GSM-7 '@' (0x00).
func TestMedium1EFSPNAndUCS2GSM7PaddingPreservation(t *testing.T) {
	if spn, err := decodeEFSPN([]byte{'A'}); err == nil || spn != "" {
		t.Fatalf("decodeEFSPN([0x41]) = (%q, %v), want empty error (byte 0 is display_condition, not operator name)", spn, err)
	}

	// UCS-2 EF_SPN: [displayCond=0x01, 0x80, '中'(4E2D), '国'(56FD), '一'(4E00), 'ÿ'(00FF), 0xFF, 0xFF]
	ucs2Raw := []byte{0x01, 0x80, 0x4E, 0x2D, 0x56, 0xFD, 0x4E, 0x00, 0x00, 0xFF, 0xFF, 0xFF}
	gotUCS2, err := decodeEFSPN(ucs2Raw)
	if err != nil {
		t.Fatalf("decodeEFSPN(UCS-2 '中国一ÿ') error = %v", err)
	}
	if gotUCS2 != "中国一ÿ" {
		t.Fatalf("decodeEFSPN(UCS-2) = %q, want %q", gotUCS2, "中国一ÿ")
	}

	// GSM-7 EF_SPN ending with '@' (0x00) followed by 0xFF SIM padding:
	gsmRaw := []byte{0x00, 'T', 'e', 'l', 0x00, 0xFF, 0xFF}
	gotGSM, err := decodeEFSPN(gsmRaw)
	if err != nil {
		t.Fatalf("decodeEFSPN(GSM-7 '@') error = %v", err)
	}
	if gotGSM != "Tel@" {
		t.Fatalf("decodeEFSPN(GSM-7) = %q, want %q", gotGSM, "Tel@")
	}
}

// MEDIUM-2: Voice USSD payload DCS decoding (packed GSM-7 and UTF-16BE UCS-2).
func TestMedium2VoiceUSSDPayloadDCSDecoding(t *testing.T) {
	// Packed GSM-7 for "Bal:10" (6 septets -> 6 bytes):
	septets := []byte{0x42, 0x61, 0x6C, 0x3A, 0x31, 0x30}
	var packed []byte
	acc, bits := 0, 0
	for _, s := range septets {
		acc |= int(s&0x7F) << bits
		bits += 7
		if bits >= 8 {
			packed = append(packed, byte(acc&0xFF))
			acc >>= 8
			bits -= 8
		}
	}
	if bits > 0 {
		packed = append(packed, byte(acc&0xFF))
	}

	gsmInd, err := ParseVoiceUSSDIndication(&Packet{
		TLVs: []TLV{
			{Type: 0x01, Value: []byte{0x01}}, // user_action
			{Type: 0x10, Value: append([]byte{0x0F, byte(len(packed))}, packed...)},
		},
	})
	if err != nil {
		t.Fatalf("ParseVoiceUSSDIndication(GSM7) error = %v", err)
	}
	if gsmInd.USSData == nil || gsmInd.USSData.Text != "Bal:10" {
		t.Fatalf("GSM7 USSD decoded = %+v, want Text=\"Bal:10\"", gsmInd.USSData)
	}

	// UCS-2 (DCS=0x03) for "余额100元"
	u16 := utf16.Encode([]rune("余额100元"))
	ucs2Bytes := make([]byte, len(u16)*2)
	for i, v := range u16 {
		binary.BigEndian.PutUint16(ucs2Bytes[i*2:], v)
	}
	ucs2Ind, err := ParseVoiceUSSDIndication(&Packet{
		TLVs: []TLV{
			{Type: 0x01, Value: []byte{0x01}},
			{Type: 0x10, Value: append([]byte{0x03, byte(len(ucs2Bytes))}, ucs2Bytes...)},
		},
	})
	if err != nil {
		t.Fatalf("ParseVoiceUSSDIndication(UCS2) error = %v", err)
	}
	if ucs2Ind.USSData == nil || ucs2Ind.USSData.Text != "余额100元" {
		t.Fatalf("UCS2 USSD decoded = %+v, want Text=\"余额100元\"", ucs2Ind.USSData)
	}
}

// MEDIUM-3: WMS ListMessagesAuto TLV 0x12 + MT_READ inclusion, and parseRawReadMessageValue 256-byte (0x0100) 4-byte framing.
func TestMedium3WMSListMessagesAutoAnd256ByteRawReadFraming(t *testing.T) {
	t.Run("ListMessagesAuto sends MessageMode TLV 0x12 and merges MT_READ when firmware requires tag", func(t *testing.T) {
		var sawMode0x12 bool
		client := newQMIServiceTestHarness(t, ServiceWMS, 5, func(req *Packet) (*Packet, error) {
			if req.MessageID != WMSListMessages {
				return nil, errors.New("unexpected WMS msg")
			}
			if FindTLV(req.TLVs, 0x10) != nil {
				t.Errorf("WMSListMessages sent bogus TLV 0x10 instead of MessageMode TLV 0x12")
			}
			if m := FindTLV(req.TLVs, 0x12); m != nil && bytes.Equal(m.Value, []byte{uint8(MessageModeGW)}) {
				sawMode0x12 = true
			}
			tagTLV := FindTLV(req.TLVs, 0x11)
			if tagTLV == nil {
				// Simulate strict firmware requiring explicit MessageTag TLV 0x11.
				return &Packet{TLVs: []TLV{{Type: 0x02, Value: []byte{0x01, 0x00, 0x30, 0x00}}}}, nil
			}
			tag := MessageTagType(tagTLV.Value[0])
			var list bytes.Buffer
			switch tag {
			case TagTypeMTNotRead:
				_ = binary.Write(&list, binary.LittleEndian, uint32(0)) // 0 unread!
			case TagTypeMTRead:
				_ = binary.Write(&list, binary.LittleEndian, uint32(1))
				_ = binary.Write(&list, binary.LittleEndian, uint32(7))
				list.WriteByte(byte(TagTypeMTRead))
			default:
				_ = binary.Write(&list, binary.LittleEndian, uint32(0))
			}
			return &Packet{
				TLVs: []TLV{
					successResultTLV(),
					{Type: 0x01, Value: list.Bytes()},
				},
			}, nil
		})
		wms := &WMSService{client: client, clientID: 5}
		msgs, err := wms.ListMessagesAuto(context.Background(), 0x01)
		if err != nil {
			t.Fatalf("ListMessagesAuto() error = %v", err)
		}
		if !sawMode0x12 {
			t.Fatal("expected ListMessagesAuto to encode MessageMode in TLV 0x12")
		}
		if len(msgs) != 1 || msgs[0].Index != 7 || msgs[0].Tag != TagTypeMTRead {
			t.Fatalf("ListMessagesAuto() = %+v, want [{Index:7 Tag:TagTypeMTRead}]", msgs)
		}
	})

	t.Run("parseRawReadMessageValue preserves 256-byte (0x0100) tagged PDU without truncating to 6 bytes", func(t *testing.T) {
		pdu := make([]byte, 256)
		for i := range pdu {
			pdu[i] = byte(i)
		}
		// 4-byte tagged layout: [Tag=TagTypeMTRead(0x00), Format=0x06, LenLE=0x00, 0x01 (256), Data(256)]
		val := make([]byte, 4+len(pdu))
		val[0] = byte(TagTypeMTRead)
		val[1] = 0x06
		binary.LittleEndian.PutUint16(val[2:4], uint16(len(pdu)))
		copy(val[4:], pdu)

		parsed, err := parseRawReadMessageValue(val)
		if err != nil {
			t.Fatalf("parseRawReadMessageValue(256B) error = %v", err)
		}
		if !parsed.hasTag || parsed.tag != TagTypeMTRead || !bytes.Equal(parsed.data, pdu) {
			t.Fatalf("parseRawReadMessageValue(256B) got hasTag=%v tag=%v len=%d, want true, TagTypeMTRead, 256", parsed.hasTag, parsed.tag, len(parsed.data))
		}
	})
}
