package manager

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/kimoju/quectel-qmi-go/pkg/qmi"
)

type apduScript struct {
	responses   map[string][][]byte
	defaultAPDU []byte
}

func (s *apduScript) send(command []byte) ([]byte, error) {
	if len(s.responses) == 0 {
		if len(s.defaultAPDU) > 0 {
			return append([]byte(nil), s.defaultAPDU...), nil
		}
		return []byte{0x6A, 0x82}, nil
	}
	key := strings.ToUpper(hex.EncodeToString(command))
	queue := s.responses[key]
	if len(queue) == 0 {
		if len(s.defaultAPDU) > 0 {
			return append([]byte(nil), s.defaultAPDU...), nil
		}
		return []byte{0x6A, 0x82}, nil
	}
	out := append([]byte(nil), queue[0]...)
	s.responses[key] = queue[1:]
	return out, nil
}

type fakeUIMFileReader struct {
	getFileAttributesWithSessionFn func(ctx context.Context, sessionType uint8, fileID uint16, path []uint8) (*qmi.UIMFileAttributes, error)
	readRecordWithSessionFn        func(ctx context.Context, sessionType uint8, fileID uint16, path []uint8, recordNumber uint16, recordLength uint16) (*qmi.UIMRecordData, error)
	readTransparentWithSessionFn   func(ctx context.Context, sessionType uint8, fileID uint16, path []uint8) ([]byte, error)
}

func (f *fakeUIMFileReader) GetFileAttributesWithSession(ctx context.Context, sessionType uint8, fileID uint16, path []uint8) (*qmi.UIMFileAttributes, error) {
	if f.getFileAttributesWithSessionFn == nil {
		return nil, errors.New("not implemented")
	}
	return f.getFileAttributesWithSessionFn(ctx, sessionType, fileID, path)
}

func (f *fakeUIMFileReader) ReadRecordWithSession(ctx context.Context, sessionType uint8, fileID uint16, path []uint8, recordNumber uint16, recordLength uint16) (*qmi.UIMRecordData, error) {
	if f.readRecordWithSessionFn == nil {
		return nil, errors.New("not implemented")
	}
	return f.readRecordWithSessionFn(ctx, sessionType, fileID, path, recordNumber, recordLength)
}

func (f *fakeUIMFileReader) ReadTransparentWithSession(ctx context.Context, sessionType uint8, fileID uint16, path []uint8) ([]byte, error) {
	if f.readTransparentWithSessionFn == nil {
		return nil, errors.New("not implemented")
	}
	return f.readTransparentWithSessionFn(ctx, sessionType, fileID, path)
}

func encodeAddress(number string) []byte {
	number = strings.TrimSpace(number)
	if number == "" {
		return []byte{0x00}
	}

	ton := byte(0x81)
	if strings.HasPrefix(number, "+") {
		ton = 0x91
		number = strings.TrimPrefix(number, "+")
	}

	length := len(number)
	bcdLen := (length + 1) / 2
	bcd := make([]byte, bcdLen)
	for i := 0; i < length; i++ {
		digit := byte(number[i] - '0')
		if i%2 == 0 {
			bcd[i/2] |= digit
		} else {
			bcd[i/2] |= digit << 4
		}
	}
	if length%2 != 0 {
		bcd[length/2] |= 0xF0
	}

	totalLen := 1 + len(bcd)
	out := make([]byte, 1+totalLen)
	out[0] = byte(totalLen)
	out[1] = ton
	copy(out[2:], bcd)
	return out
}

func TestGetSMSCFromUIMEFSMSPSuccess(t *testing.T) {
	record := make([]byte, 0x2A)
	for i := range record {
		record[i] = 0xFF
	}
	record[14] = 0xFD // Parameter Indicators: TS-SCA present (bit 1 = 0)
	copy(record[27:], encodeAddress("+447870002308"))
	fcp := []byte{
		0x62, 0x1E,
		0x82, 0x05, 0x42, 0x21, 0x00, 0x2A, 0x01,
		0x83, 0x02, 0x6F, 0x42,
		0xA5, 0x03, 0x80, 0x01, 0x61,
		0x8A, 0x01, 0x05,
		0x8B, 0x03, 0x6F, 0x06, 0x05,
		0x80, 0x02, 0x00, 0x7E,
		0x88, 0x00,
	}

	script := &apduScript{
		responses: map[string][][]byte{
			"00A40004026F42": {append(append([]byte{}, fcp...), 0x90, 0x00)},
			"00B201042A":     {append(append([]byte{}, record...), 0x90, 0x00)},
		},
	}

	got, err := getSMSCFromUIM(script.send)
	if err != nil {
		t.Fatalf("getSMSCFromUIM() error=%v", err)
	}
	if got != "+447870002308" {
		t.Fatalf("getSMSCFromUIM()=%q want=%q", got, "+447870002308")
	}
}

func TestGetSMSCFromUIMFallbackToEFPSISMSC(t *testing.T) {
	field := make([]byte, 0x1C)
	for i := range field {
		field[i] = 0xFF
	}
	copy(field, encodeAddress("+8613800250500"))

	script := &apduScript{
		responses: map[string][][]byte{
			"00A40004026F42": {{0x6A, 0x82}},
			"00A40004026FE5": {{0x90, 0x00}},
			"00B000001C":     {append(append([]byte{}, field...), 0x90, 0x00)},
		},
	}

	got, err := getSMSCFromUIM(script.send)
	if err != nil {
		t.Fatalf("getSMSCFromUIM() error=%v", err)
	}
	if got != "+8613800250500" {
		t.Fatalf("getSMSCFromUIM()=%q want=%q", got, "+8613800250500")
	}
}

func TestGetSMSCFromUIMFailureWhenNoSources(t *testing.T) {
	script := &apduScript{
		responses: map[string][][]byte{
			"00A40004026F42": {{0x6A, 0x82}},
			"00A40004026FE5": {{0x6A, 0x82}},
		},
	}

	if _, err := getSMSCFromUIM(script.send); err == nil {
		t.Fatal("getSMSCFromUIM() expected error, got nil")
	}
}

func TestSelectUSIMOrFallbackFallbackToMFDFGSMSucceeds(t *testing.T) {
	script := &apduScript{
		responses: map[string][][]byte{
			"00A4040007A0000000871002": {{0x6A, 0x82}},
			"00A40004023F00":           {{0x90, 0x00}},
			"00A40004027F20":           {{0x90, 0x00}},
		},
	}

	if err := selectUSIMOrFallback(script.send); err != nil {
		t.Fatalf("selectUSIMOrFallback() error=%v", err)
	}
}

func TestGetSMSCFromUIMFilesFromEFSMSP(t *testing.T) {
	record := make([]byte, 0x2A)
	for i := range record {
		record[i] = 0xFF
	}
	record[14] = 0xFD // Parameter Indicators: TS-SCA present (bit 1 = 0)
	copy(record[27:], encodeAddress("+447870002308"))

	reader := &fakeUIMFileReader{
		getFileAttributesWithSessionFn: func(ctx context.Context, sessionType uint8, fileID uint16, path []uint8) (*qmi.UIMFileAttributes, error) {
			if fileID == efSMSP && len(path) == 0 {
				return &qmi.UIMFileAttributes{RecordSize: uint16(len(record)), RecordCount: 1}, nil
			}
			return nil, errors.New("not found")
		},
		readRecordWithSessionFn: func(ctx context.Context, sessionType uint8, fileID uint16, path []uint8, recordNumber uint16, recordLength uint16) (*qmi.UIMRecordData, error) {
			if fileID == efSMSP && len(path) == 0 && recordNumber == 1 {
				return &qmi.UIMRecordData{Data: append([]byte(nil), record...)}, nil
			}
			return nil, errors.New("not found")
		},
		readTransparentWithSessionFn: func(ctx context.Context, sessionType uint8, fileID uint16, path []uint8) ([]byte, error) {
			return nil, errors.New("not found")
		},
	}

	got, err := getSMSCFromUIMFiles(context.Background(), reader)
	if err != nil {
		t.Fatalf("getSMSCFromUIMFiles() error=%v", err)
	}
	if got != "+447870002308" {
		t.Fatalf("getSMSCFromUIMFiles()=%q want=%q", got, "+447870002308")
	}
}

func TestGetSMSCFromUIMFilesFallbackToEFPSISMSC(t *testing.T) {
	field := make([]byte, 0x1C)
	for i := range field {
		field[i] = 0xFF
	}
	copy(field, encodeAddress("+8613800250500"))

	reader := &fakeUIMFileReader{
		getFileAttributesWithSessionFn: func(ctx context.Context, sessionType uint8, fileID uint16, path []uint8) (*qmi.UIMFileAttributes, error) {
			return nil, errors.New("not found")
		},
		readRecordWithSessionFn: func(ctx context.Context, sessionType uint8, fileID uint16, path []uint8, recordNumber uint16, recordLength uint16) (*qmi.UIMRecordData, error) {
			return nil, errors.New("not found")
		},
		readTransparentWithSessionFn: func(ctx context.Context, sessionType uint8, fileID uint16, path []uint8) ([]byte, error) {
			if fileID == efPSISMSC && len(path) == 0 {
				return append([]byte(nil), field...), nil
			}
			return nil, errors.New("not found")
		},
	}

	got, err := getSMSCFromUIMFiles(context.Background(), reader)
	if err != nil {
		t.Fatalf("getSMSCFromUIMFiles() error=%v", err)
	}
	if got != "+8613800250500" {
		t.Fatalf("getSMSCFromUIMFiles()=%q want=%q", got, "+8613800250500")
	}
}

func TestManagerGetSMSCServiceNotReady(t *testing.T) {
	m := &Manager{}
	_, err := m.GetSMSC(context.Background())
	if err == nil {
		t.Fatal("expected service-not-ready error, got nil")
	}
	if _, ok := err.(*ServiceNotReadyError); !ok {
		t.Fatalf("unexpected error type: %T", err)
	}
}

func TestManagerGetSMSCUsesManagerAPDUTransport(t *testing.T) {
	record := make([]byte, 0x2A)
	for i := range record {
		record[i] = 0xFF
	}
	record[14] = 0xFD // Parameter Indicators: TS-SCA present (bit 1 = 0)
	copy(record[27:], encodeAddress("+447870002308"))
	fcp := []byte{
		0x62, 0x1E,
		0x82, 0x05, 0x42, 0x21, 0x00, 0x2A, 0x01,
		0x83, 0x02, 0x6F, 0x42,
		0xA5, 0x03, 0x80, 0x01, 0x61,
		0x8A, 0x01, 0x05,
		0x8B, 0x03, 0x6F, 0x06, 0x05,
		0x80, 0x02, 0x00, 0x7E,
		0x88, 0x00,
	}
	script := &apduScript{
		responses: map[string][][]byte{
			"00A40004026F42": {append(append([]byte{}, fcp...), 0x90, 0x00)},
			"00B201042A":     {append(append([]byte{}, record...), 0x90, 0x00)},
		},
	}

	m := newRecoveryTestManager()
	sendCalls := 0
	m.sendAPDUHook = func(ctx context.Context, slot uint8, channel uint8, command []byte) ([]byte, error) {
		sendCalls++
		if slot != smscAPDUSlot {
			t.Fatalf("slot=%d, want %d", slot, smscAPDUSlot)
		}
		if channel != 0 {
			t.Fatalf("channel=%d, want basic channel", channel)
		}
		return script.send(command)
	}

	got, err := m.GetSMSC(context.Background())
	if err != nil {
		t.Fatalf("GetSMSC() error=%v", err)
	}
	if got != "+447870002308" {
		t.Fatalf("GetSMSC()=%q want=%q", got, "+447870002308")
	}
	if sendCalls == 0 {
		t.Fatal("expected GetSMSC to use manager APDU transport")
	}
}

func TestParseLinearFixedMetaFromFCPMultiByteBERLength(t *testing.T) {
	buildFCP := func(paddingLen int) []byte {
		// File descriptor TLV: 0x82, len=5, linear fixed (0x42), 0x21, 0x00, recordLen=0x2A, count=0x02
		fd := []byte{0x82, 0x05, 0x42, 0x21, 0x00, 0x2A, 0x02}

		var padTLV []byte
		if paddingLen <= 127 {
			padTLV = append([]byte{0xA5, byte(paddingLen)}, make([]byte, paddingLen)...)
		} else if paddingLen <= 255 {
			padTLV = append([]byte{0xA5, 0x81, byte(paddingLen)}, make([]byte, paddingLen)...)
		} else {
			padTLV = append([]byte{0xA5, 0x82, byte(paddingLen >> 8), byte(paddingLen)}, make([]byte, paddingLen)...)
		}

		body := append(padTLV, fd...)
		if len(body) <= 127 {
			return append([]byte{0x62, byte(len(body))}, body...)
		}
		if len(body) <= 255 {
			return append([]byte{0x62, 0x81, byte(len(body))}, body...)
		}
		return append([]byte{0x62, 0x82, byte(len(body) >> 8), byte(len(body))}, body...)
	}

	t.Run("BER 0x81 single-byte extended length", func(t *testing.T) {
		fcp := buildFCP(150) // total body > 127 && <= 255 => 0x62 0x81 <len>
		recordLen, count := parseLinearFixedMetaFromFCP(fcp)
		if recordLen != 0x2A || count != 2 {
			t.Fatalf("got recordLen=0x%x count=%d, want 0x2a/2", recordLen, count)
		}
	})

	t.Run("BER 0x82 two-byte extended length", func(t *testing.T) {
		fcp := buildFCP(280) // total body > 255 => 0x62 0x82 <hi> <lo>
		recordLen, count := parseLinearFixedMetaFromFCP(fcp)
		if recordLen != 0x2A || count != 2 {
			t.Fatalf("got recordLen=0x%x count=%d, want 0x2a/2", recordLen, count)
		}
	})
}

func TestManagerGetSMSCUsesActiveSlot2FromUIMCardStatus(t *testing.T) {
	record := make([]byte, 0x2A)
	for i := range record {
		record[i] = 0xFF
	}
	record[14] = 0xFD // Parameter Indicators: TS-SCA present (bit 1 = 0)
	copy(record[27:], encodeAddress("+8613800100500"))
	fcp := []byte{
		0x62, 0x07,
		0x82, 0x05, 0x42, 0x21, 0x00, 0x2A, 0x01,
	}
	script := &apduScript{
		responses: map[string][][]byte{
			"00A40004026F42": {append(append([]byte{}, fcp...), 0x90, 0x00)},
			"00B201042A":     {append(append([]byte{}, record...), 0x90, 0x00)},
		},
	}

	// Build 2-slot UIMGetCardStatus packet where PrimaryGWSlot=2 (index_gw_pri = 0x0100)
	cardStatusValue := []byte{
		0x00, 0x01, // index_gw_pri = 0x0100 (Slot 2, App 0)
		0xFF, 0xFF,
		0xFF, 0xFF,
		0xFF, 0xFF,
		0x02, // 2 slots
		// Slot 1: absent
		0x00, 0x00, 0x00, 0x00, 0x01, 0x00,
		// Slot 2: present, 0 apps needed for slot selection
		0x01, 0x00, 0x03, 0x0A, 0x00, 0x00,
	}
	uimSvc := newUIMReadinessTestService(t, func(req *qmi.Packet) (*qmi.Packet, error) {
		if req.MessageID == qmi.UIMGetCardStatus {
			return &qmi.Packet{
				TLVs: []qmi.TLV{
					{Type: 0x02, Value: []byte{0x00, 0x00, 0x00, 0x00}},
					{Type: 0x10, Value: cardStatusValue},
				},
			}, nil
		}
		return nil, errors.New("unexpected UIM request")
	})

	m := newRecoveryTestManager()
	m.uim = uimSvc
	var usedSlots []uint8
	m.sendAPDUHook = func(ctx context.Context, slot uint8, channel uint8, command []byte) ([]byte, error) {
		usedSlots = append(usedSlots, slot)
		if slot != 2 {
			return []byte{0x6A, 0x82}, nil
		}
		return script.send(command)
	}

	got, err := m.GetSMSC(context.Background())
	if err != nil {
		t.Fatalf("GetSMSC() error = %v", err)
	}
	if got != "+8613800100500" {
		t.Fatalf("GetSMSC() = %q, want %q", got, "+8613800100500")
	}
	if len(usedSlots) == 0 || usedSlots[0] != 2 {
		t.Fatalf("GetSMSC() used APDU slots %v, want slot 2", usedSlots)
	}
}
