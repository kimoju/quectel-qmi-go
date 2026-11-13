//go:build linux

package manager

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/kimoju/quectel-qmi-go/pkg/qmi"
)

func startUnixMockQMIProxy(t *testing.T, handler func(req *qmi.Packet) []qmi.TLV) *qmi.Client {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "qmi-proxy.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen(unix): %v", err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
	})

	var nextCID uint8 = 1
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			hdr := make([]byte, 3)
			if _, err := io.ReadFull(conn, hdr); err != nil {
				return
			}
			total := 1 + int(binary.LittleEndian.Uint16(hdr[1:3]))
			frame := make([]byte, total)
			copy(frame, hdr)
			if _, err := io.ReadFull(conn, frame[3:]); err != nil {
				return
			}
			req, err := qmi.UnmarshalPacket(frame)
			if err != nil {
				return
			}

			if req.ServiceType == qmi.ServiceControl {
				tlvs := []qmi.TLV{{Type: 0x02, Value: []byte{0, 0, 0, 0}}}
				switch req.MessageID {
				case qmi.CTLGetClientID:
					svc := req.TLVs[0].Value[0]
					cid := nextCID
					nextCID++
					tlvs = append(tlvs, qmi.TLV{Type: 0x01, Value: []byte{svc, cid}})
				case qmi.CTLReleaseClientID:
					tlvs = append(tlvs, req.TLVs...)
				}
				var tlvBytes []byte
				for _, tlv := range tlvs {
					tlvBytes = append(tlvBytes, tlv.Marshal()...)
				}
				ctlH := qmi.CTLHeader{
					ControlFlags:  0x01,
					TransactionID: uint8(req.TransactionID),
					MessageID:     req.MessageID,
					Length:        uint16(len(tlvBytes)),
				}
				body := append(ctlH.Marshal(), tlvBytes...)
				qmux := qmi.QmuxHeader{
					IFType:       0x01,
					Length:       uint16(len(body) + 5),
					ControlFlags: 0x00,
					ServiceType:  qmi.ServiceControl,
					ClientID:     0x00,
				}
				_, _ = conn.Write(append(qmux.Marshal(), body...))
				continue
			}

			respTLVs := handler(req)
			var tlvBytes []byte
			for _, tlv := range respTLVs {
				tlvBytes = append(tlvBytes, tlv.Marshal()...)
			}
			svcH := qmi.ServiceHeader{
				ControlFlags:  0x02,
				TransactionID: req.TransactionID,
				MessageID:     req.MessageID,
				Length:        uint16(len(tlvBytes)),
			}
			body := append(svcH.Marshal(), tlvBytes...)
			qmux := qmi.QmuxHeader{
				IFType:       0x01,
				Length:       uint16(len(body) + 5),
				ControlFlags: 0x80,
				ServiceType:  req.ServiceType,
				ClientID:     req.ClientID,
			}
			_, _ = conn.Write(append(qmux.Marshal(), body...))
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := qmi.NewClientWithOptions(ctx, "/dev/cdc-wdm-mock", qmi.ClientOptions{
		UseProxy:              true,
		ProxyPath:             sockPath,
		DefaultRequestTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewClientWithOptions(unix mock): %v", err)
	}
	t.Cleanup(func() {
		_ = client.Close()
	})
	return client
}

// HIGH-2: ResetExistingDataConnection over QMI wire checks and stops BOTH IpFamilyV4 (0x04) and IpFamilyV6 (0x06) sessions!
func TestHigh2ResetExistingDataConnectionStopsBothIPv4AndIPv6Sessions(t *testing.T) {
	var mu sync.Mutex
	curFamily := uint8(qmi.IpFamilyV4)
	v4Connected := true
	v6Connected := true
	var stoppedFamilies []uint8

	okTLV := qmi.TLV{Type: 0x02, Value: []byte{0, 0, 0, 0}}
	client := startUnixMockQMIProxy(t, func(req *qmi.Packet) []qmi.TLV {
		mu.Lock()
		defer mu.Unlock()
		switch req.MessageID {
		case qmi.WDSSetClientIPFamilyPref:
			if tlv := qmi.FindTLV(req.TLVs, 0x01); tlv != nil && len(tlv.Value) == 1 {
				curFamily = tlv.Value[0]
			}
			return []qmi.TLV{okTLV}
		case qmi.WDSGetPktSrvcStatus:
			st := qmi.PktDataDisconnected
			if curFamily == qmi.IpFamilyV4 && v4Connected {
				st = qmi.PktDataConnected
			} else if curFamily == qmi.IpFamilyV6 && v6Connected {
				st = qmi.PktDataConnected
			}
			return []qmi.TLV{okTLV, {Type: 0x01, Value: []byte{st, 0x00}}}
		case qmi.WDSStopNetworkInterface:
			stoppedFamilies = append(stoppedFamilies, curFamily)
			if curFamily == qmi.IpFamilyV4 {
				v4Connected = false
			} else if curFamily == qmi.IpFamilyV6 {
				v6Connected = false
			}
			return []qmi.TLV{okTLV}
		default:
			return []qmi.TLV{okTLV}
		}
	})

	m := newRecoveryTestManager()
	m.client = client
	m.coreReady = true

	reset, err := m.ResetExistingDataConnection(context.Background())
	if err != nil {
		t.Fatalf("ResetExistingDataConnection() error = %v", err)
	}
	if !reset {
		t.Fatal("ResetExistingDataConnection() = false, want true")
	}
	mu.Lock()
	gotStopped := append([]uint8(nil), stoppedFamilies...)
	stillUp := v4Connected || v6Connected
	mu.Unlock()
	if stillUp || !reflect.DeepEqual(gotStopped, []uint8{qmi.IpFamilyV4, qmi.IpFamilyV6}) {
		t.Fatalf("stoppedFamilies = %v (v4Up=%v v6Up=%v), want [4 6] both stopped", gotStopped, v4Connected, v6Connected)
	}
}

// HIGH-3: handleDialFailure runs RadioReset synchronously BEFORE scheduling the reconnect retry timer.
func TestHigh3HandleDialFailureRunsRadioResetSynchronouslyBeforeSchedulingTimer(t *testing.T) {
	var mu sync.Mutex
	var events []string
	okTLV := qmi.TLV{Type: 0x02, Value: []byte{0, 0, 0, 0}}

	client := startUnixMockQMIProxy(t, func(req *qmi.Packet) []qmi.TLV {
		if req.ServiceType == qmi.ServiceDMS && req.MessageID == qmi.DMSSetOperatingMode {
			if tlv := qmi.FindTLV(req.TLVs, 0x01); tlv != nil && len(tlv.Value) == 1 {
				mu.Lock()
				if tlv.Value[0] == 0x01 {
					events = append(events, "radio_low_power")
				} else if tlv.Value[0] == 0x00 {
					events = append(events, "radio_online")
				}
				mu.Unlock()
			}
		}
		return []qmi.TLV{okTLV}
	})

	dms, err := qmi.NewDMSService(client)
	if err != nil {
		t.Fatalf("NewDMSService(): %v", err)
	}

	m := New(Config{
		AutoReconnect: true,
		RetryPolicy: RetryPolicy{
			ReconnectDelays: []time.Duration{time.Hour},
			RadioResetAfter: 1,
		},
	}, NewNopLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.ctx = ctx
	m.cancel = cancel
	m.client = client
	m.dms = dms
	m.desiredConnection = true
	m.afterFunc = func(_ time.Duration, _ func()) *time.Timer {
		mu.Lock()
		events = append(events, "schedule_reconnect_timer")
		mu.Unlock()
		return time.NewTimer(time.Hour)
	}

	m.handleDialFailure(errors.New("dial failed"))

	mu.Lock()
	gotOrder := append([]string(nil), events...)
	mu.Unlock()
	wantOrder := []string{"radio_low_power", "radio_online", "schedule_reconnect_timer"}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("handleDialFailure ordering = %v, want %v (RadioReset must finish synchronously before retry timer)", gotOrder, wantOrder)
	}
}
