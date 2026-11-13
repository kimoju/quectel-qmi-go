package manager

import (
	"context"
	"strings"
	"testing"

	"github.com/warthog618/sms/encoding/tpdu"
	"github.com/warthog618/sms/encoding/ucs2"
)

func TestEncodeSMSWithOptionsForcesUCS2(t *testing.T) {
	m := &Manager{}

	pdus, err := m.encodeSMSWithOptions("10086", "hello", SendSMSOptions{Encoding: SMSEncodingUCS2})
	if err != nil {
		t.Fatalf("encodeSMSWithOptions() error = %v", err)
	}
	if len(pdus) != 1 {
		t.Fatalf("len(pdus) = %d, want 1", len(pdus))
	}
	pduWithSMSC := pdus[0]
	if len(pduWithSMSC) < 2 || pduWithSMSC[0] != 0x00 {
		t.Fatalf("unexpected PDU with SMSC header: %x", pduWithSMSC)
	}

	pdu := &tpdu.TPDU{Direction: tpdu.MO}
	if err := pdu.UnmarshalBinary(pduWithSMSC[1:]); err != nil {
		t.Fatalf("UnmarshalBinary() error = %v", err)
	}
	if pdu.DCS != tpdu.DcsUCS2Data {
		t.Fatalf("DCS=0x%02x want 0x%02x", byte(pdu.DCS), byte(tpdu.DcsUCS2Data))
	}
	if got, want := []byte(pdu.UD), ucs2.Encode([]rune("hello")); string(got) != string(want) {
		t.Fatalf("UD=%x want UCS2 %x", got, want)
	}
}

func TestEncodeSMSKeepsAutoEncodingByDefault(t *testing.T) {
	m := &Manager{}

	pdus, err := m.encodeSMS("10086", "hello")
	if err != nil {
		t.Fatalf("encodeSMS() error = %v", err)
	}
	if len(pdus) != 1 {
		t.Fatalf("len(pdus) = %d, want 1", len(pdus))
	}
	pduWithSMSC := pdus[0]
	if len(pduWithSMSC) < 2 || pduWithSMSC[0] != 0x00 {
		t.Fatalf("unexpected PDU with SMSC header: %x", pduWithSMSC)
	}

	pdu := &tpdu.TPDU{Direction: tpdu.MO}
	if err := pdu.UnmarshalBinary(pduWithSMSC[1:]); err != nil {
		t.Fatalf("UnmarshalBinary() error = %v", err)
	}
	if pdu.DCS != 0x00 {
		t.Fatalf("DCS=0x%02x want auto GSM7 0x00", byte(pdu.DCS))
	}
}

func TestSendSMSWithOptionsSendsAllMultipartSegments(t *testing.T) {
	t.Run("GSM7 multipart (>160 chars) sends all 3 segments sequentially", func(t *testing.T) {
		m := &Manager{}
		longText := strings.Repeat("0123456789ABCDEF", 22) // 352 GSM-7 chars => 3 segments (153*2 + 46)

		var sent [][]byte
		m.sendRawSMSHook = func(_ context.Context, format uint8, pdu []byte) error {
			if format != 0x06 {
				t.Fatalf("format = 0x%02x, want 0x06", format)
			}
			sent = append(sent, append([]byte(nil), pdu...))
			return nil
		}

		if err := m.SendSMSWithOptions("+8613800138000", longText, SendSMSOptions{}); err != nil {
			t.Fatalf("SendSMSWithOptions() error = %v", err)
		}
		if len(sent) != 3 {
			t.Fatalf("len(sent) = %d, want 3 segments", len(sent))
		}

		var concatRef int
		for i, raw := range sent {
			if len(raw) < 2 || raw[0] != 0x00 {
				t.Fatalf("segment %d missing 0x00 SMSC prefix: %x", i+1, raw)
			}
			seg := &tpdu.TPDU{Direction: tpdu.MO}
			if err := seg.UnmarshalBinary(raw[1:]); err != nil {
				t.Fatalf("segment %d UnmarshalBinary() error = %v", i+1, err)
			}
			if seg.DA.TypeOfNumber() != tpdu.TonInternational || seg.DA.Addr != "8613800138000" {
				t.Fatalf("segment %d DA = (TON=%v Addr=%q), want (TonInternational, 8613800138000)", i+1, seg.DA.TypeOfNumber(), seg.DA.Addr)
			}
			var info DecodedSMS
			populateConcatInfo(&info, seg)
			if !info.IsConcat || info.ConcatTotal != 3 || info.ConcatSeq != i+1 {
				t.Fatalf("segment %d concat = %+v, want total=3 seq=%d", i+1, info, i+1)
			}
			if i == 0 {
				concatRef = info.ConcatRef
			} else if info.ConcatRef != concatRef {
				t.Fatalf("segment %d concatRef = %d, want %d", i+1, info.ConcatRef, concatRef)
			}
		}
	})

	t.Run("UCS2 multipart (>70 runes) sends all 3 segments and preserves full text", func(t *testing.T) {
		m := &Manager{}
		longCJK := strings.Repeat("这是一条多段测试短信", 16) // 160 runes => 3 UCS-2 segments (67*2 + 26)

		var sent [][]byte
		m.sendRawSMSHook = func(_ context.Context, format uint8, pdu []byte) error {
			sent = append(sent, append([]byte(nil), pdu...))
			return nil
		}

		if err := m.SendSMSWithOptions("10690123456789", longCJK, SendSMSOptions{Encoding: SMSEncodingUCS2}); err != nil {
			t.Fatalf("SendSMSWithOptions() error = %v", err)
		}
		if len(sent) != 3 {
			t.Fatalf("len(sent) = %d, want 3 segments", len(sent))
		}

		var reassembledRunes []rune
		for i, raw := range sent {
			seg := &tpdu.TPDU{Direction: tpdu.MO}
			if err := seg.UnmarshalBinary(raw[1:]); err != nil {
				t.Fatalf("segment %d UnmarshalBinary() error = %v", i+1, err)
			}
			if seg.DA.TypeOfNumber() != tpdu.TonUnknown || seg.DA.Addr != "10690123456789" {
				t.Fatalf("segment %d DA = (TON=%v Addr=%q), want (TonUnknown, 10690123456789)", i+1, seg.DA.TypeOfNumber(), seg.DA.Addr)
			}
			decodedRunes, err := ucs2.Decode(seg.UD)
			if err != nil {
				t.Fatalf("segment %d ucs2.Decode() error = %v", i+1, err)
			}
			reassembledRunes = append(reassembledRunes, decodedRunes...)
		}
		if got := string(reassembledRunes); got != longCJK {
			t.Fatalf("reassembled UCS2 text len=%d, want %d (%q != %q)", len([]rune(got)), len([]rune(longCJK)), got, longCJK)
		}
	})
}

func TestEncodeSMSDestinationTONSelection(t *testing.T) {
	m := &Manager{}
	cases := []struct {
		name     string
		input    string
		wantTON  tpdu.TypeOfNumber
		wantAddr string
	}{
		{
			name:     "plus international E164",
			input:    "+8613800138000",
			wantTON:  tpdu.TonInternational,
			wantAddr: "8613800138000",
		},
		{
			name:     "00 international prefix stripped",
			input:    "0085291234567",
			wantTON:  tpdu.TonInternational,
			wantAddr: "85291234567",
		},
		{
			name:     "carrier 5-digit shortcode",
			input:    "10086",
			wantTON:  tpdu.TonUnknown,
			wantAddr: "10086",
		},
		{
			name:     "bank 5-digit shortcode",
			input:    "95588",
			wantTON:  tpdu.TonUnknown,
			wantAddr: "95588",
		},
		{
			name:     "long 18-digit 1069 service shortcode",
			input:    "106901234567890123",
			wantTON:  tpdu.TonUnknown,
			wantAddr: "106901234567890123",
		},
		{
			name:     "domestic 11-digit mobile without plus",
			input:    "13800138000",
			wantTON:  tpdu.TonUnknown,
			wantAddr: "13800138000",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pdus, err := m.encodeSMSWithOptions(tc.input, "test", SendSMSOptions{})
			if err != nil {
				t.Fatalf("encodeSMSWithOptions(%q) error = %v", tc.input, err)
			}
			if len(pdus) != 1 {
				t.Fatalf("len(pdus) = %d, want 1", len(pdus))
			}
			seg := &tpdu.TPDU{Direction: tpdu.MO}
			if err := seg.UnmarshalBinary(pdus[0][1:]); err != nil {
				t.Fatalf("UnmarshalBinary() error = %v", err)
			}
			if seg.DA.TypeOfNumber() != tc.wantTON {
				t.Fatalf("TON = %v (type byte 0x%02x), want %v", seg.DA.TypeOfNumber(), seg.DA.TOA, tc.wantTON)
			}
			if seg.DA.Addr != tc.wantAddr {
				t.Fatalf("Addr = %q, want %q", seg.DA.Addr, tc.wantAddr)
			}
		})
	}
}
