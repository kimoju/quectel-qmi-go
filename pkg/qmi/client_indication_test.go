package qmi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestClientLogfUsesInjectedLogger(t *testing.T) {
	var gotLevel ClientLogLevel
	var gotMessage string
	c := &Client{
		opts: ClientOptions{
			Logf: func(level ClientLogLevel, format string, args ...any) {
				gotLevel = level
				gotMessage = fmt.Sprintf(format, args...)
			},
		},
	}

	c.logf(ClientLogLevelDebug, "hello %s", "qmi")

	if gotLevel != ClientLogLevelDebug {
		t.Fatalf("level=%v want %v", gotLevel, ClientLogLevelDebug)
	}
	if gotMessage != "hello qmi" {
		t.Fatalf("message=%q want %q", gotMessage, "hello qmi")
	}
}

func TestDispatchIndicationClassifiesNASEventReportSeparately(t *testing.T) {
	c := &Client{
		eventCh:            make(chan Event, 1),
		indicationInCh:     nil,
		closeCh:            make(chan struct{}),
		transactions:       make(map[uint32]*transactionEntry),
		recentTransactions: make(map[uint32]recentTransaction),
		clientIDs:          make(map[uint8]uint8),
	}

	c.dispatchIndication(&Packet{
		ServiceType:  ServiceNAS,
		MessageID:    NASEventReportInd,
		IsIndication: true,
	})

	got := <-c.eventCh
	if got.Type != EventNASEventReport {
		t.Fatalf("event type=%v want EventNASEventReport", got.Type)
	}
	if got.ServiceID != ServiceNAS || got.MessageID != NASEventReportInd {
		t.Fatalf("raw ids service=0x%02x msg=0x%04x", got.ServiceID, got.MessageID)
	}
}

func TestModemResetIndicationNotDroppedWhenQueueFull(t *testing.T) {
	c := &Client{
		opts:              ClientOptions{ReadDeadline: 5 * time.Millisecond},
		eventCh:           make(chan Event, 4),
		indicationInCh:    make(chan Event, 1),
		coalescedSignalCh: make(chan struct{}, 1),
		closeCh:           make(chan struct{}),
		coalesced: coalescedEventStore{
			events: make(map[string]Event),
		},
	}

	// Fill indication queue first so enqueueIndication must use coalesced fallback.
	c.indicationInCh <- Event{Type: EventUnknown}
	c.enqueueIndication(Event{
		Type:      EventModemReset,
		ServiceID: ServiceControl,
		MessageID: CTLRevokeClientIDInd,
	})

	done := make(chan struct{})
	c.wg.Add(1)
	go func() {
		c.indicationLoop()
		close(done)
	}()
	defer func() {
		close(c.closeCh)
		<-done
	}()

	deadline := time.After(1 * time.Second)
	for {
		select {
		case evt := <-c.eventCh:
			if evt.Type == EventModemReset {
				return
			}
		case <-deadline:
			t.Fatal("expected EventModemReset to be delivered")
		}
	}
}

func TestSendRequestWithCanceledContextDoesNotQueueWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := &Client{
		opts:         DefaultClientOptions(),
		writeCh:      make(chan writeRequest, 1),
		closeCh:      make(chan struct{}),
		transactions: make(map[uint32]*transactionEntry),
	}

	_, err := c.SendRequest(ctx, ServiceUIM, 1, UIMReadRecord, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if got := len(c.writeCh); got != 0 {
		t.Fatalf("SendRequest queued %d write(s) after context cancellation", got)
	}
	if got := len(c.transactions); got != 0 {
		t.Fatalf("SendRequest left %d transaction(s) after context cancellation", got)
	}
}

func TestCompletedTimedOutTransactionIsRememberedForLateResponse(t *testing.T) {
	c := &Client{
		opts:               DefaultClientOptions(),
		writeCh:            make(chan writeRequest, 1),
		closeCh:            make(chan struct{}),
		transactions:       make(map[uint32]*transactionEntry),
		recentTransactions: make(map[uint32]recentTransaction),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		_, err := c.SendRequest(ctx, ServiceUIM, 1, UIMReadRecord, nil)
		errCh <- err
	}()

	wr := <-c.writeCh
	wr.result <- nil

	err := <-errCh
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline exceeded, got %v", err)
	}

	key := uint32(ServiceUIM)<<16 | 1
	if !c.isRecentTransaction(key, ServiceUIM, UIMReadRecord) {
		t.Fatalf("timed out UIMReadRecord transaction was not retained for late response matching")
	}
	if got := len(c.transactions); got != 0 {
		t.Fatalf("timed out request left %d active transaction(s)", got)
	}
}

func buildServiceWireFrame(service uint8, clientID uint8, controlFlags uint8, txID uint16, msgID uint16, tlvs []TLV) []byte {
	var tlvBytes []byte
	for _, tlv := range tlvs {
		tlvBytes = append(tlvBytes, tlv.Marshal()...)
	}
	svcHdr := ServiceHeader{
		ControlFlags:  controlFlags,
		TransactionID: txID,
		MessageID:     msgID,
		Length:        uint16(len(tlvBytes)),
	}
	body := append(svcHdr.Marshal(), tlvBytes...)
	qmux := QmuxHeader{
		IFType:       0x01,
		Length:       uint16(len(body) + 5),
		ControlFlags: 0x80,
		ServiceType:  service,
		ClientID:     clientID,
	}
	return append(qmux.Marshal(), body...)
}

func TestReadLoopDropsUnmatchedResponseWithoutSpoofingIndication(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()

	restore := replaceRawTransportForTest(t, func(string) (qmiTransport, error) {
		return clientConn, nil
	})
	defer restore()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	client, err := NewClientWithOptions(ctx, "/dev/cdc-wdm-mock", ClientOptions{
		SyncOnOpen:   false,
		ReadDeadline: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewClientWithOptions() error = %v", err)
	}
	defer client.Close()

	// 1. Write an unmatched RESPONSE (controlFlags=0x02, NOT 0x04 indication)
	// using WDSGetPktSrvcStatus (0x0022, same MessageID as WDSGetPktSrvcStatusInd!).
	// Before the fix, readLoop passed unmatched responses to dispatchIndication,
	// spoofing an EventPacketServiceStatusChanged disconnect event!
	unmatchedResp := buildServiceWireFrame(ServiceWDS, 1, 0x02, 0x0099, WDSGetPktSrvcStatus, []TLV{
		successTLV(),
		{Type: 0x01, Value: []byte{PktDataDisconnected, 0x00}},
	})
	if _, err := serverConn.Write(unmatchedResp); err != nil {
		t.Fatalf("write unmatched response: %v", err)
	}

	// 2. Write a genuine NAS indication (controlFlags=0x04) to prove readLoop processed the prior frame.
	realInd := buildServiceWireFrame(ServiceNAS, 2, 0x04, 0x0001, NASEventReportInd, nil)
	if _, err := serverConn.Write(realInd); err != nil {
		t.Fatalf("write real indication: %v", err)
	}

	select {
	case evt := <-client.Events():
		if evt.Type == EventPacketServiceStatusChanged {
			t.Fatalf("unmatched non-indication response spoofed EventPacketServiceStatusChanged: %+v", evt)
		}
		if evt.Type != EventNASEventReport {
			t.Fatalf("got event %v, want EventNASEventReport", evt.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for genuine indication after unmatched response")
	}

	if got := client.Stats().UnmatchedResponses; got != 1 {
		t.Fatalf("Stats().UnmatchedResponses = %d, want 1", got)
	}
}

func TestAllocateTransactionWraparoundSkipsInFlightKeys(t *testing.T) {
	c := &Client{
		opts:               DefaultClientOptions(),
		transactions:       make(map[uint32]*transactionEntry),
		recentTransactions: make(map[uint32]recentTransaction),
	}

	// 1. 8-bit CTL txID wraparound: 255 -> 1 while txID=1 is still in flight
	inFlightCTL := &transactionEntry{
		ch:      make(chan *Packet, 1),
		service: ServiceControl,
		msgID:   CTLGetClientID,
		txID:    1,
	}
	ctlKey1 := uint32(ServiceControl)<<16 | 1
	c.transactions[ctlKey1] = inFlightCTL
	c.ctlTxID = 255

	txID, key, entry2, err := c.allocateTransaction(ServiceControl, CTLGetVersionInfo, make(chan *Packet, 1), time.Now().Add(time.Second))
	if err != nil {
		t.Fatalf("allocateTransaction(CTL) error = %v", err)
	}
	if txID != 2 {
		t.Fatalf("CTL wraparound allocated txID=%d, want 2 (skipping in-flight 1)", txID)
	}
	if c.transactions[ctlKey1] != inFlightCTL {
		t.Fatal("CTL wraparound overwrote in-flight transaction 1")
	}
	if c.transactions[key] != entry2 {
		t.Fatal("new CTL transaction 2 not stored")
	}

	// Completing a stale entry pointer for key1 must NOT delete inFlightCTL
	staleEntry := &transactionEntry{service: ServiceControl, msgID: CTLGetClientID, txID: 1}
	c.completeTransaction(ctlKey1, staleEntry, context.DeadlineExceeded)
	if c.transactions[ctlKey1] != inFlightCTL {
		t.Fatal("completeTransaction with stale pointer deleted active in-flight entry")
	}

	// 2. 16-bit service txID wraparound: 65535 -> 1 while txID=1 is still in flight
	inFlightNAS := &transactionEntry{
		ch:      make(chan *Packet, 1),
		service: ServiceNAS,
		msgID:   NASPerformNetworkScan,
		txID:    1,
	}
	nasKey1 := uint32(ServiceNAS)<<16 | 1
	c.transactions[nasKey1] = inFlightNAS
	c.lastTxID = 65535

	nasTxID, _, _, err := c.allocateTransaction(ServiceNAS, NASGetServingSystem, make(chan *Packet, 1), time.Now().Add(time.Second))
	if err != nil {
		t.Fatalf("allocateTransaction(NAS) error = %v", err)
	}
	if nasTxID != 2 {
		t.Fatalf("NAS wraparound allocated txID=%d, want 2 (skipping in-flight 1)", nasTxID)
	}
	if c.transactions[nasKey1] != inFlightNAS {
		t.Fatal("NAS wraparound overwrote in-flight transaction 1")
	}
}
