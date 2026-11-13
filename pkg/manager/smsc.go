package manager

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/kimoju/quectel-qmi-go/pkg/qmi"
)

const (
	smscAPDUSlot uint8  = 1
	efSMSP       uint16 = 0x6F42
	efPSISMSC    uint16 = 0x6FE5
)

var (
	// Generic USIM AID prefix used by reader mode.
	genericUSIMAID = []byte{0xA0, 0x00, 0x00, 0x00, 0x87, 0x10, 0x02}

	// Candidate paths for UIM ReadRecord/ReadTransparent fallback.
	smscCandidatePaths = [][]uint8{
		{},
		{0x00, 0x3F, 0xFF, 0x7F},
		{0x20, 0x7F},
		{0x00, 0x3F, 0x10, 0x7F},
	}
)

// GetSMSC reads SMSC for QMI mode.
//
// Strategy:
//  1. APDU on basic channel (reader-style select flow).
//  2. APDU on USIM logical channel.
//  3. UIM ReadRecord/ReadTransparent fallback (for modems rejecting UIM SendAPDU 0x003B).
func (m *Manager) GetSMSC(ctx context.Context) (string, error) {
	smsc, err := m.querySMSCFromDevice(ctx)
	if err != nil {
		return "", err
	}

	trimmed := strings.TrimSpace(smsc)
	known := trimmed != ""
	now := time.Now()
	m.setWMSSMSCState(trimmed, known, known, false, now, now)
	return trimmed, nil
}

func (m *Manager) resolveSMSCAPDUSlot(ctx context.Context) uint8 {
	if m == nil {
		return smscAPDUSlot
	}
	m.mu.RLock()
	hasUIM := m.uim != nil || m.client != nil || m.ensureUIMServiceHook != nil
	m.mu.RUnlock()
	if !hasUIM {
		return smscAPDUSlot
	}

	details, err := withUIMRecoveryValue(m, "GetSMSC.GetCardStatusDetails", func(uim *qmi.UIMService) (*qmi.CardStatusDetails, error) {
		d, _, err := uim.GetCardStatusDetails(ctx)
		return d, err
	})
	if err == nil && details != nil {
		if details.PrimaryGWSlot > 0 {
			return details.PrimaryGWSlot
		}
		if details.ActiveSlot > 0 {
			return details.ActiveSlot
		}
	}

	slotInfo, slotErr := withUIMRecoveryValue(m, "GetSMSC.GetSlotStatus", func(uim *qmi.UIMService) (*qmi.UIMSlotStatus, error) {
		return uim.GetSlotStatus(ctx)
	})
	if slotErr == nil {
		if slot, ok, _ := resolveActiveUIMSlot(slotInfo); ok && slot > 0 {
			return slot
		}
	}

	return smscAPDUSlot
}

func (m *Manager) querySMSCFromDevice(ctx context.Context) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if m.sendAPDUHook == nil {
		if _, err := withUIMRecoveryValue(m, "GetSMSC.EnsureUIM", func(uim *qmi.UIMService) (struct{}, error) {
			return struct{}{}, nil
		}); err != nil {
			return "", err
		}
	}

	slot := m.resolveSMSCAPDUSlot(ctx)

	txBasic := func(command []byte) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return m.SendAPDUContext(ctx, slot, 0, command)
	}
	smsc, errBasic := getSMSCFromUIM(txBasic)
	if strings.TrimSpace(smsc) != "" {
		return strings.TrimSpace(smsc), nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	smsc, errLogical := m.getSMSCFromUIMLogicalChannel(ctx, slot)
	if strings.TrimSpace(smsc) != "" {
		return strings.TrimSpace(smsc), nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	smsc, errFile := getSMSCFromUIMFiles(ctx, managerUIMFileReader{m: m})
	if strings.TrimSpace(smsc) != "" {
		return strings.TrimSpace(smsc), nil
	}

	return "", fmt.Errorf("QMI read SMSC failed: APDU-basic=%v; APDU-logical=%v; UIM-file=%v",
		errBasic, errLogical, errFile)
}

type apduSender func(command []byte) ([]byte, error)

func getSMSCFromUIM(tx apduSender) (string, error) {
	selectErr := selectUSIMOrFallback(tx)

	smsc, readErr := getSMSCFromSelectedFiles(tx)
	if strings.TrimSpace(smsc) != "" {
		return strings.TrimSpace(smsc), nil
	}

	if selectErr != nil && readErr != nil {
		return "", fmt.Errorf("select USIM failed: %v; read SMSC failed: %w", selectErr, readErr)
	}
	if readErr != nil {
		return "", readErr
	}
	if selectErr != nil {
		return "", selectErr
	}
	return "", fmt.Errorf("QMI did not resolve SMSC")
}

func getSMSCFromSelectedFiles(tx apduSender) (string, error) {
	smsc, errSMSP := readSMSCFromEFSMSP(tx)
	if strings.TrimSpace(smsc) != "" {
		return strings.TrimSpace(smsc), nil
	}

	smsc, errPSI := readSMSCFromEFPSISMSC(tx)
	if strings.TrimSpace(smsc) != "" {
		return strings.TrimSpace(smsc), nil
	}

	if errSMSP != nil && errPSI != nil {
		return "", fmt.Errorf("QMI read SMSC failed: EFSMSP=%v; EFPSISMSC=%v", errSMSP, errPSI)
	}
	if errSMSP != nil {
		return "", errSMSP
	}
	if errPSI != nil {
		return "", errPSI
	}
	return "", fmt.Errorf("QMI did not resolve SMSC")
}

func selectUSIMOrFallback(tx apduSender) error {
	selectAID := append([]byte{0x00, 0xA4, 0x04, 0x00, byte(len(genericUSIMAID))}, genericUSIMAID...)
	rsp, err := transmitBasicAPDUWithFollowUp(tx, selectAID)
	if err == nil {
		if _, err := extractAPDUSuccessData(rsp); err == nil {
			return nil
		}
	}
	selectAIDErr := err
	if selectAIDErr == nil {
		_, selectAIDErr = extractAPDUSuccessData(rsp)
	}

	selectMF := []byte{0x00, 0xA4, 0x00, 0x04, 0x02, 0x3F, 0x00}
	rsp, err = transmitBasicAPDUWithFollowUp(tx, selectMF)
	if err != nil {
		return fmt.Errorf("select USIM AID failed: %v; select MF failed: %w", selectAIDErr, err)
	}
	if _, err := extractAPDUSuccessData(rsp); err != nil {
		return fmt.Errorf("select USIM AID failed: %v; select MF failed: %w", selectAIDErr, err)
	}

	selectDFGSM := []byte{0x00, 0xA4, 0x00, 0x04, 0x02, 0x7F, 0x20}
	rsp, err = transmitBasicAPDUWithFollowUp(tx, selectDFGSM)
	if err != nil {
		return fmt.Errorf("select USIM AID failed: %v; select DF_GSM failed: %w", selectAIDErr, err)
	}
	if _, err := extractAPDUSuccessData(rsp); err != nil {
		return fmt.Errorf("select USIM AID failed: %v; select DF_GSM failed: %w", selectAIDErr, err)
	}
	return nil
}

func encodeISO7816LogicalChannelCLA(cla, channel byte) byte {
	if channel == 0 {
		return cla
	}
	if channel <= 3 {
		// ISO/IEC 7816-4 first interindustry CLA coding (channels 0..3): clear b7 and set b2..b1.
		return (cla & 0xBC) | (channel & 0x03)
	}
	if channel <= 19 {
		// ISO/IEC 7816-4 further interindustry CLA coding (channels 4..19): set b7=1 and b4..b1=(ch-4).
		return (cla & 0xB0) | 0x40 | ((channel - 4) & 0x0F)
	}
	return cla
}

func (m *Manager) getSMSCFromUIMLogicalChannel(ctx context.Context, slot uint8) (string, error) {
	if slot == 0 {
		slot = smscAPDUSlot
	}
	channel, err := m.OpenLogicalChannelContext(ctx, slot, genericUSIMAID)
	if err != nil {
		return "", fmt.Errorf("open USIM logical channel failed: %w", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = m.CloseLogicalChannelContext(closeCtx, slot, channel)
	}()

	tx := func(command []byte) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cmdWithCLA := append([]byte(nil), command...)
		if len(cmdWithCLA) > 0 {
			cmdWithCLA[0] = encodeISO7816LogicalChannelCLA(cmdWithCLA[0], channel)
		}
		return m.SendAPDUContext(ctx, slot, channel, cmdWithCLA)
	}
	smsc, err := getSMSCFromSelectedFiles(tx)
	if strings.TrimSpace(smsc) != "" {
		return strings.TrimSpace(smsc), nil
	}
	if err != nil {
		return "", fmt.Errorf("logical channel SMSC read failed: %w", err)
	}
	return "", fmt.Errorf("logical channel did not resolve SMSC")
}

type uimFileReader interface {
	GetFileAttributesWithSession(ctx context.Context, sessionType uint8, fileID uint16, path []uint8) (*qmi.UIMFileAttributes, error)
	ReadRecordWithSession(ctx context.Context, sessionType uint8, fileID uint16, path []uint8, recordNumber uint16, recordLength uint16) (*qmi.UIMRecordData, error)
	ReadTransparentWithSession(ctx context.Context, sessionType uint8, fileID uint16, path []uint8) ([]byte, error)
}

type managerUIMFileReader struct {
	m *Manager
}

func (r managerUIMFileReader) GetFileAttributesWithSession(ctx context.Context, sessionType uint8, fileID uint16, path []uint8) (*qmi.UIMFileAttributes, error) {
	return r.m.UIMGetFileAttributesWithSession(ctx, sessionType, fileID, path)
}

func (r managerUIMFileReader) ReadRecordWithSession(ctx context.Context, sessionType uint8, fileID uint16, path []uint8, recordNumber uint16, recordLength uint16) (*qmi.UIMRecordData, error) {
	return r.m.UIMReadRecordWithSession(ctx, sessionType, fileID, path, recordNumber, recordLength)
}

func (r managerUIMFileReader) ReadTransparentWithSession(ctx context.Context, sessionType uint8, fileID uint16, path []uint8) ([]byte, error) {
	return r.m.UIMReadTransparentWithSession(ctx, sessionType, fileID, path)
}

func getSMSCFromUIMFiles(ctx context.Context, uim uimFileReader) (string, error) {
	var errSMSP error
	for _, path := range smscCandidatePaths {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		smsc, err := readSMSCFromEFSMSPWithUIMRead(ctx, uim, path)
		if strings.TrimSpace(smsc) != "" {
			return strings.TrimSpace(smsc), nil
		}
		if err != nil {
			errSMSP = err
		}
	}

	var errPSI error
	for _, path := range smscCandidatePaths {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		smsc, err := readSMSCFromEFPSISMSCWithUIMRead(ctx, uim, path)
		if strings.TrimSpace(smsc) != "" {
			return strings.TrimSpace(smsc), nil
		}
		if err != nil {
			errPSI = err
		}
	}

	if errSMSP != nil && errPSI != nil {
		return "", fmt.Errorf("UIM file read failed: EFSMSP=%v; EFPSISMSC=%v", errSMSP, errPSI)
	}
	if errSMSP != nil {
		return "", errSMSP
	}
	if errPSI != nil {
		return "", errPSI
	}
	return "", fmt.Errorf("UIM file read did not resolve SMSC")
}

func readSMSCFromEFSMSPWithUIMRead(ctx context.Context, uim uimFileReader, path []uint8) (string, error) {
	recordLen := 0x1C
	maxRecords := 10

	attrs, attrErr := uim.GetFileAttributesWithSession(ctx, qmi.UIMSessionTypePrimaryGWProvisioning, efSMSP, path)
	if attrErr == nil && attrs != nil {
		if attrs.RecordSize > 0 && attrs.RecordSize <= 0xFF {
			recordLen = int(attrs.RecordSize)
		}
		if attrs.RecordCount > 0 {
			maxRecords = int(attrs.RecordCount)
			if maxRecords > 30 {
				maxRecords = 30
			}
		}
	}

	var lastErr error
	for record := 1; record <= maxRecords; record++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		rec, err := uim.ReadRecordWithSession(
			ctx,
			qmi.UIMSessionTypePrimaryGWProvisioning,
			efSMSP,
			path,
			uint16(record),
			uint16(recordLen),
		)
		if err != nil {
			lastErr = err
			continue
		}
		if rec == nil || len(rec.Data) == 0 {
			continue
		}
		if smsc := parseSMSCFromSMSPRecord(rec.Data); smsc != "" {
			return smsc, nil
		}
	}

	p := formatUIMPath(path)
	if attrErr != nil && lastErr != nil {
		return "", fmt.Errorf("UIM read EFSMSP failed path=%s: attrs=%v read=%w", p, attrErr, lastErr)
	}
	if lastErr != nil {
		return "", fmt.Errorf("UIM read EFSMSP failed path=%s: %w", p, lastErr)
	}
	if attrErr != nil {
		return "", fmt.Errorf("UIM read EFSMSP attrs failed path=%s: %w", p, attrErr)
	}
	return "", fmt.Errorf("UIM EFSMSP has no valid SMSC path=%s", p)
}

func readSMSCFromEFPSISMSCWithUIMRead(ctx context.Context, uim uimFileReader, path []uint8) (string, error) {
	data, err := uim.ReadTransparentWithSession(ctx, qmi.UIMSessionTypePrimaryGWProvisioning, efPSISMSC, path)
	if err != nil {
		return "", fmt.Errorf("UIM read EFPSISMSC failed path=%s: %w", formatUIMPath(path), err)
	}
	if smsc := parseSMSCFromTSServiceCentreAddress(data); smsc != "" {
		return smsc, nil
	}
	return "", fmt.Errorf("UIM EFPSISMSC has no valid SMSC path=%s", formatUIMPath(path))
}

func formatUIMPath(path []uint8) string {
	if len(path) == 0 {
		return "<current>"
	}
	return strings.ToUpper(hex.EncodeToString(path))
}

func readSMSCFromEFPSISMSC(tx apduSender) (string, error) {
	selectCmd := []byte{0x00, 0xA4, 0x00, 0x04, 0x02, 0x6F, 0xE5}
	rsp, err := transmitBasicAPDUWithFollowUp(tx, selectCmd)
	if err != nil {
		return "", fmt.Errorf("select EFPSISMSC failed: %w", err)
	}
	if _, err := extractAPDUSuccessData(rsp); err != nil {
		return "", fmt.Errorf("select EFPSISMSC failed: %w", err)
	}

	readCmd := []byte{0x00, 0xB0, 0x00, 0x00, 0x1C}
	rsp, err = transmitBasicAPDUWithFollowUp(tx, readCmd)
	if err != nil {
		return "", fmt.Errorf("read EFPSISMSC failed: %w", err)
	}
	data, err := extractAPDUSuccessData(rsp)
	if err != nil {
		return "", fmt.Errorf("read EFPSISMSC failed: %w", err)
	}
	if smsc := parseSMSCFromTSServiceCentreAddress(data); smsc != "" {
		return smsc, nil
	}
	return "", fmt.Errorf("EFPSISMSC has no valid SMSC")
}

func readSMSCFromEFSMSP(tx apduSender) (string, error) {
	selectCmd := []byte{0x00, 0xA4, 0x00, 0x04, 0x02, 0x6F, 0x42}
	rsp, err := transmitBasicAPDUWithFollowUp(tx, selectCmd)
	if err != nil {
		return "", fmt.Errorf("select EFSMSP failed: %w", err)
	}
	fcpData, err := extractAPDUSuccessData(rsp)
	if err != nil {
		return "", fmt.Errorf("select EFSMSP failed: %w", err)
	}

	recordLen, recordCount := parseLinearFixedMetaFromFCP(fcpData)
	if recordLen <= 0 || recordLen > 0xFF {
		recordLen = 0x1C
	}
	maxRecords := 10
	if recordCount > 0 && recordCount < maxRecords {
		maxRecords = recordCount
	}

	var lastErr error
	for record := 1; record <= maxRecords; record++ {
		readCmd := []byte{0x00, 0xB2, byte(record), 0x04, byte(recordLen)}
		rsp, err := transmitBasicAPDUWithFollowUp(tx, readCmd)
		if err != nil {
			lastErr = err
			continue
		}

		sw1, sw2, ok := apduStatusFromResp(rsp)
		if !ok {
			lastErr = fmt.Errorf("record %d response too short", record)
			continue
		}
		if sw1 == 0x6A && (sw2 == 0x83 || sw2 == 0x82) {
			break
		}
		if sw1 == 0x67 && sw2 > 0 {
			recordLen = int(sw2)
			readCmd = []byte{0x00, 0xB2, byte(record), 0x04, byte(recordLen)}
			rsp, err = transmitBasicAPDUWithFollowUp(tx, readCmd)
			if err != nil {
				lastErr = err
				continue
			}
			sw1, sw2, ok = apduStatusFromResp(rsp)
			if !ok {
				lastErr = fmt.Errorf("record %d retry response too short", record)
				continue
			}
			if sw1 == 0x6A && (sw2 == 0x83 || sw2 == 0x82) {
				break
			}
		}
		if !isAPDUSuccess(sw1, sw2) {
			lastErr = fmt.Errorf("read record %d failed: SW=%02X%02X", record, sw1, sw2)
			continue
		}
		data := rsp[:len(rsp)-2]
		if smsc := parseSMSCFromSMSPRecord(data); smsc != "" {
			return smsc, nil
		}
	}

	if lastErr != nil {
		return "", fmt.Errorf("EFSMSP parse failed: %w", lastErr)
	}
	return "", fmt.Errorf("EFSMSP parse failed")
}

func transmitBasicAPDUWithFollowUp(tx apduSender, apdu []byte) ([]byte, error) {
	rsp, err := tx(apdu)
	if err != nil {
		return nil, err
	}
	sw1, sw2, ok := apduStatusFromResp(rsp)
	if !ok {
		return nil, fmt.Errorf("APDU response too short: %X", rsp)
	}

	// 6Cxx: Le mismatch, retry once with modem suggested length.
	if sw1 == 0x6C && len(apdu) >= 5 {
		retry := append([]byte(nil), apdu...)
		if len(apdu) > 5 && len(apdu) == 5+int(apdu[4]) {
			// Case-3 APDU (CLA INS P1 P2 Lc Data, no Le): append sw2 as Le instead of overwriting Data[Lc-1].
			retry = append(retry, sw2)
		} else {
			retry[len(retry)-1] = sw2
		}
		rsp, err = tx(retry)
		if err != nil {
			return nil, err
		}
		sw1, sw2, ok = apduStatusFromResp(rsp)
		if !ok {
			return nil, fmt.Errorf("APDU response too short: %X", rsp)
		}
	}

	// 61xx: follow up by GET RESPONSE (preserve CLA logical channel bits if set).
	if sw1 == 0x61 {
		cla := byte(0x00)
		if len(apdu) > 0 {
			cla = apdu[0]
		}
		getRespCmd := []byte{cla, 0xC0, 0x00, 0x00, sw2}
		rsp, err = tx(getRespCmd)
		if err != nil {
			return nil, err
		}
	}
	return rsp, nil
}

func apduStatusFromResp(rsp []byte) (byte, byte, bool) {
	if len(rsp) < 2 {
		return 0, 0, false
	}
	return rsp[len(rsp)-2], rsp[len(rsp)-1], true
}

func isAPDUSuccess(sw1, sw2 byte) bool {
	_ = sw2
	return sw1 == 0x90 || sw1 == 0x62 || sw1 == 0x63
}

func extractAPDUSuccessData(rsp []byte) ([]byte, error) {
	sw1, sw2, ok := apduStatusFromResp(rsp)
	if !ok {
		return nil, fmt.Errorf("APDU response too short: %X", rsp)
	}
	if !isAPDUSuccess(sw1, sw2) {
		return nil, fmt.Errorf("SW=%02X%02X", sw1, sw2)
	}
	return rsp[:len(rsp)-2], nil
}

func parseSMSCFromTSServiceCentreAddress(raw []byte) string {
	if len(raw) == 0 || allBytes(raw, 0xFF) {
		return ""
	}
	length := int(raw[0])
	if length < 2 || length == 0xFF {
		return ""
	}
	end := 1 + length
	if end > len(raw) {
		return ""
	}
	v := raw[1:end]
	smsc, err := decodeAddressValue(v)
	if err != nil {
		return ""
	}
	if !isLikelySMSC(smsc) {
		return ""
	}
	return smsc
}

func parseSMSCFromSMSPRecord(record []byte) string {
	if len(record) == 0 || allBytes(record, 0xFF) {
		return ""
	}

	// Per 3GPP TS 31.102 §4.2.25, an EF_SMSP record has Y >= 0 bytes of Alpha-ID
	// followed by a fixed 28-byte tail:
	//   base + 0       : Parameter Indicators (bit 1 == 0 means TS-SCA present)
	//   base + 1..12   : TP-DestinationAddress (12 bytes - MUST NOT be mistaken for SMSC!)
	//   base + 13..24  : TS-ServiceCentreAddress (12 bytes)
	//   base + 25..27  : TP-PID, TP-DCS, TP-VP
	if len(record) >= 28 {
		base := len(record) - 28
		paramInd := record[base]
		if paramInd != 0xFF && (paramInd&0x02) != 0 {
			return ""
		}
		return decodeSMSCAtOffset(record, base+13)
	}

	// Fallback only for non-standard truncated payloads (< 28 bytes).
	return parseSMSCFromTSServiceCentreAddress(record)
}

func decodeSMSCAtOffset(buf []byte, offset int) string {
	if offset < 0 || offset >= len(buf)-1 {
		return ""
	}
	length := int(buf[offset])
	if length < 2 || length > 12 {
		return ""
	}
	end := offset + 1 + length
	if end > len(buf) {
		return ""
	}
	typeOfAddress := buf[offset+1]
	if typeOfAddress == 0x00 || typeOfAddress == 0xFF || (typeOfAddress&0x80) == 0 {
		return ""
	}
	smsc, err := decodeAddressValue(buf[offset+1 : end])
	if err != nil {
		return ""
	}
	if !isLikelySMSC(smsc) {
		return ""
	}
	return smsc
}

func isLikelySMSC(smsc string) bool {
	v := strings.TrimSpace(smsc)
	if v == "" {
		return false
	}
	v = strings.TrimPrefix(v, "+")
	if len(v) < 5 || len(v) > 16 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return false
		}
	}
	return true
}

func parseBERTLVLength(buf []byte, offset int) (length int, nextOffset int, ok bool) {
	if offset < 0 || offset >= len(buf) {
		return 0, 0, false
	}
	first := buf[offset]
	offset++
	if first&0x80 == 0 {
		return int(first), offset, true
	}
	numOctets := int(first & 0x7F)
	// ISO/IEC 8825-1 definite long form: 0x81 (1 byte), 0x82 (2 bytes), up to 4 bytes; 0x80 (indefinite) is invalid here.
	if numOctets <= 0 || numOctets > 4 || offset+numOctets > len(buf) {
		return 0, 0, false
	}
	val := 0
	for j := 0; j < numOctets; j++ {
		val = (val << 8) | int(buf[offset+j])
	}
	return val, offset + numOctets, true
}

func parseLinearFixedMetaFromFCP(fcp []byte) (recordLen int, recordCount int) {
	if len(fcp) < 2 {
		return 0, 0
	}
	data := fcp
	if fcp[0] == 0x62 {
		total, hdrEnd, ok := parseBERTLVLength(fcp, 1)
		if !ok {
			return 0, 0
		}
		if total > len(fcp)-hdrEnd {
			total = len(fcp) - hdrEnd
		}
		data = fcp[hdrEnd : hdrEnd+total]
	}

	for i := 0; i < len(data); {
		if i+2 > len(data) {
			break
		}
		tag := data[i]
		i++
		l, nextIdx, ok := parseBERTLVLength(data, i)
		if !ok {
			break
		}
		i = nextIdx
		if i+l > len(data) {
			break
		}
		v := data[i : i+l]
		i += l

		if tag == 0x82 && len(v) >= 5 {
			recordLen = (int(v[len(v)-3]) << 8) | int(v[len(v)-2])
			recordCount = int(v[len(v)-1])
			if recordLen > 0 {
				return recordLen, recordCount
			}
		}
	}
	return 0, 0
}

func decodeAddressValue(v []byte) (string, error) {
	if len(v) < 1 {
		return "", fmt.Errorf("address value is empty")
	}
	ton := v[0]
	bcd := v[1:]

	prefix := ""
	if ton&0x70 == 0x10 {
		prefix = "+"
	}

	var sb strings.Builder
	sb.WriteString(prefix)
	for _, b := range bcd {
		lo := b & 0x0F
		hi := (b >> 4) & 0x0F
		if lo <= 9 {
			sb.WriteByte('0' + lo)
		} else if lo != 0x0F {
			return "", fmt.Errorf("invalid BCD digit: %x", lo)
		}

		if hi <= 9 {
			sb.WriteByte('0' + hi)
		} else if hi != 0x0F {
			return "", fmt.Errorf("invalid BCD digit: %x", hi)
		}
	}
	return sb.String(), nil
}

func allBytes(buf []byte, value byte) bool {
	if len(buf) == 0 {
		return false
	}
	for _, b := range buf {
		if b != value {
			return false
		}
	}
	return true
}

// CachedSMSC returns the last known good SMSC address, or empty string if unknown.
func (m *Manager) CachedSMSC() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.wmsSMSCValue
}
