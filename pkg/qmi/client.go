package qmi

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
// Event types for indication handling / 指示处理的事件类型
// ============================================================================

type EventType int

const (
	EventUnknown                               EventType = iota
	EventPacketServiceStatusChanged                      // WDS connection status changed / WDS连接状态改变
	EventServingSystemChanged                            // NAS registration state changed / NAS注册状态改变
	EventNASOperatorNameChanged                          // NAS operator name changed / NAS 运营商名称变化
	EventNASNetworkTimeChanged                           // NAS network time changed / NAS 网络时间变化
	EventNASSignalInfoChanged                            // NAS signal info changed / NAS 信号信息变化
	EventNASNetworkReject                                // NAS network reject / NAS 驻网拒绝
	EventNASIncrementalNetworkScan                       // NAS incremental network scan / NAS 增量搜网
	EventModemReset                                      // CTL revoke client ID (modem reset) / CTL撤销客户端ID (modem重置)
	EventNewMessage                                      // WMS new message / WMS新消息
	EventWMSSMSCAddress                                  // WMS SMSC address indication / WMS 短信中心地址指示
	EventWMSTransportNetworkRegistrationStatus           // WMS transport network registration status indication / WMS 传输网络注册状态指示
	EventIMSRegistrationStatus                           // IMSA registration status changed / IMSA 注册状态变化
	EventIMSServicesStatus                               // IMSA services status changed / IMSA 业务状态变化
	EventIMSSettingsChanged                              // IMS settings changed / IMS 配置状态变化
	EventVoiceCallStatus                                 // Voice all call status indication / VOICE 通话状态指示
	EventVoiceSupplementaryService                       // Voice supplementary service indication / VOICE 补充业务指示
	EventUSSD                                            // Voice USSD indication / Voice USSD指示
	EventVoiceUSSDReleased                               // Voice USSD released indication / VOICE USSD 释放指示
	EventVoiceUSSDNoWaitResult                           // Voice originate USSD no wait indication / VOICE 异步 USSD 指示
	EventSimStatusChanged                                // UIM SIM status changed / UIM SIM状态改变
	EventUIMSessionClosed                                // UIM session closed indication / UIM 会话关闭指示
	EventUIMRefresh                                      // UIM refresh indication / UIM 刷新指示
	EventUIMSlotStatus                                   // UIM slot status indication / UIM 卡槽状态指示
	EventNASEventReport                                  // NAS event report / NAS 事件报告
	EventVoiceSupplementaryServiceRequest                // Voice supplementary service request indication / VOICE 补充业务请求指示
)

// Event represents an asynchronous indication from the modem / Event代表来自modem的异步指示
type Event struct {
	Type      EventType
	ServiceID uint8
	MessageID uint16
	Packet    *Packet
}

type ClientLogLevel string

const (
	ClientLogLevelDebug ClientLogLevel = "debug"
	ClientLogLevelWarn  ClientLogLevel = "warn"
	ClientLogLevelError ClientLogLevel = "error"
)

type ClientLogFunc func(level ClientLogLevel, format string, args ...any)

// ClientOptions controls runtime behavior for the low-level QMI client.
type ClientOptions struct {
	SyncOnOpen            bool
	QueryVersionOnOpen    bool // 启动时自动查询服务版本信息
	ReadDeadline          time.Duration
	DefaultRequestTimeout time.Duration
	TxQueueSize           int
	IndicationQueueSize   int
	UseProxy              bool
	ProxyPath             string
	ProxyExecutable       string
	ProxyOpenTimeout      time.Duration
	ProxyFallbackToRaw    bool
	Logf                  ClientLogFunc
}

// DefaultClientOptions returns the production defaults used by NewClientWithOptions.
func DefaultClientOptions() ClientOptions {
	return ClientOptions{
		SyncOnOpen:            true,
		QueryVersionOnOpen:    true,
		ReadDeadline:          100 * time.Millisecond,
		DefaultRequestTimeout: 30 * time.Second,
		TxQueueSize:           128,
		IndicationQueueSize:   256,
		ProxyPath:             defaultProxyPath,
		ProxyExecutable:       defaultProxyExecutable,
		ProxyOpenTimeout:      defaultProxyOpenTimeout,
	}
}

// ClientStats summarizes key runtime behaviors without exposing payloads.
type ClientStats struct {
	UnmatchedResponses     uint64
	ParseErrors            uint64
	CoalescedIndications   uint64
	DroppedEdgeIndications uint64
}

type writeRequest struct {
	data   []byte
	result chan error
}

type coalescedEventStore struct {
	events map[string]Event
	order  []string
}

type transactionEntry struct {
	ch       chan *Packet
	service  uint8
	msgID    uint16
	txID     uint16
	start    time.Time
	deadline time.Time
}

type recentTransaction struct {
	service     uint8
	msgID       uint16
	txID        uint16
	start       time.Time
	deadline    time.Time
	completedAt time.Time
	err         string
}

const (
	recentTransactionTTL       = 2 * time.Minute
	maxRecentTransactions      = 256
	lateResponseLogMinInterval = 5 * time.Second
)

// ============================================================================
// Client - QMI communication client / 客户端 - QMI通信客户端
// ============================================================================

type Client struct {
	conn qmiTransport
	path string
	opts ClientOptions

	// 服务版本缓存 (由 GetServiceVersions 填充)
	serviceVersions map[uint8]ServiceVersion
	versionQueried  bool

	// Transaction management / 事务管理
	mu                     sync.Mutex
	transactions           map[uint32]*transactionEntry
	recentTransactions     map[uint32]recentTransaction
	lateResponseLastLog    time.Time
	lateResponseSuppressed uint64
	lastTxID               uint32 // atomic counter / 原子计数器
	ctlTxID                uint32 // separate counter for CTL (1 byte) / CTL的独立计数器 (1字节)

	// Client ID cache / 客户端ID缓存
	clientIDs map[uint8]map[uint8]struct{} // service -> set of clientIDs / 服务 -> 客户端ID集合
	fatalErr  error

	// Event handling / 事件处理
	eventCh           chan Event
	indicationInCh    chan Event
	coalescedSignalCh chan struct{}
	writeCh           chan writeRequest
	closeCh           chan struct{}
	deadCh            chan struct{}
	closeOnce         sync.Once
	deadOnce          sync.Once
	wg                sync.WaitGroup

	coalescedMu sync.Mutex
	coalesced   coalescedEventStore

	unmatchedResponses     atomic.Uint64
	parseErrors            atomic.Uint64
	coalescedIndications   atomic.Uint64
	droppedEdgeIndications atomic.Uint64
}

func isZeroClientOptions(opts ClientOptions) bool {
	return !opts.SyncOnOpen &&
		!opts.QueryVersionOnOpen &&
		opts.ReadDeadline == 0 &&
		opts.DefaultRequestTimeout == 0 &&
		opts.TxQueueSize == 0 &&
		opts.IndicationQueueSize == 0 &&
		!opts.UseProxy &&
		opts.ProxyPath == "" &&
		opts.ProxyExecutable == "" &&
		opts.ProxyOpenTimeout == 0 &&
		!opts.ProxyFallbackToRaw &&
		opts.Logf == nil
}

func normalizeClientOptions(opts ClientOptions) ClientOptions {
	defaults := DefaultClientOptions()
	isZero := isZeroClientOptions(opts)
	if opts.ReadDeadline <= 0 {
		opts.ReadDeadline = defaults.ReadDeadline
	}
	if opts.DefaultRequestTimeout <= 0 {
		opts.DefaultRequestTimeout = defaults.DefaultRequestTimeout
	}
	if opts.TxQueueSize <= 0 {
		opts.TxQueueSize = defaults.TxQueueSize
	}
	if opts.IndicationQueueSize <= 0 {
		opts.IndicationQueueSize = defaults.IndicationQueueSize
	}
	if opts.ProxyPath == "" {
		opts.ProxyPath = defaults.ProxyPath
	}
	if opts.ProxyExecutable == "" {
		opts.ProxyExecutable = defaults.ProxyExecutable
	}
	if opts.ProxyOpenTimeout <= 0 {
		opts.ProxyOpenTimeout = defaults.ProxyOpenTimeout
	}
	// Preserve backwards-compatible zero-value construction while honoring explicit false
	// whenever any option is specified. Never send CTLSync when UseProxy is enabled.
	if isZero {
		opts.SyncOnOpen = defaults.SyncOnOpen
		opts.QueryVersionOnOpen = defaults.QueryVersionOnOpen
	}
	if opts.UseProxy {
		opts.SyncOnOpen = false
	}
	return opts
}

func (c *Client) logf(level ClientLogLevel, format string, args ...any) {
	if c != nil && c.opts.Logf != nil {
		c.opts.Logf(level, format, args...)
		return
	}
	log.Printf(format, args...)
}

func logClientOption(opts ClientOptions, level ClientLogLevel, format string, args ...any) {
	if opts.Logf != nil {
		opts.Logf(level, format, args...)
		return
	}
	log.Printf(format, args...)
}

// NewClientWithOptions creates a new QMI client connected to the given device path.
func NewClientWithOptions(ctx context.Context, path string, opts ClientOptions) (*Client, error) {
	opts = normalizeClientOptions(opts)
	openCtx := ctx
	if openCtx == nil {
		openCtx = context.Background()
	}
	if _, hasDeadline := openCtx.Deadline(); !hasDeadline && opts.UseProxy && opts.ProxyOpenTimeout > 0 {
		var cancel context.CancelFunc
		openCtx, cancel = context.WithTimeout(openCtx, opts.ProxyOpenTimeout)
		defer cancel()
	}

	var (
		conn qmiTransport
		err  error
	)
	if opts.UseProxy {
		conn, err = openProxyTransportHook(openCtx, opts)
		if err != nil && opts.ProxyFallbackToRaw {
			logClientOption(opts, ClientLogLevelWarn, "QMI: qmi-proxy transport unavailable, falling back to raw QMI for %s: %v", path, err)
			opts.UseProxy = false
			conn, err = openRawTransportHook(path)
			if err != nil {
				return nil, fmt.Errorf("qmi-proxy unavailable and raw QMI fallback failed for %s: %w", path, err)
			}
		}
	} else {
		conn, err = openRawTransportHook(path)
	}
	if err != nil {
		return nil, err
	}

	c := newClientWithTransport(path, opts, conn)

	if opts.UseProxy {
		if err := c.openProxyDevice(openCtx, path); err != nil {
			if opts.ProxyFallbackToRaw {
				_ = c.Close()
				logClientOption(opts, ClientLogLevelWarn, "QMI: qmi-proxy failed to open device %s, falling back to raw QMI: %v", path, err)
				fallbackOpts := opts
				fallbackOpts.UseProxy = false
				conn, rawErr := openRawTransportHook(path)
				if rawErr != nil {
					return nil, fmt.Errorf("qmi-proxy device open failed for %s: %v; raw QMI fallback failed: %w", path, err, rawErr)
				}
				c = newClientWithTransport(path, fallbackOpts, conn)
			} else {
				_ = c.Close()
				return nil, err
			}
		}
	}

	// Initial sync (non-fatal, helps clear modem state on raw transport; never send over qmi-proxy!)
	if c.opts.SyncOnOpen && !c.opts.UseProxy {
		syncCtx := ctx
		if syncCtx == nil {
			syncCtx = context.Background()
		}
		if _, hasDeadline := syncCtx.Deadline(); !hasDeadline {
			var cancel context.CancelFunc
			syncCtx, cancel = context.WithTimeout(syncCtx, 5*time.Second)
			defer cancel()
		}
		if err := c.Sync(syncCtx); err != nil {
			c.logf(ClientLogLevelDebug, "QMI: initial sync failed (non-fatal): %v", err)
		}
	}

	// Query version info (non-fatal) / 查询版本信息 (非致命)
	if opts.QueryVersionOnOpen {
		versionCtx := ctx
		if versionCtx == nil {
			versionCtx = context.Background()
		}
		if _, hasDeadline := versionCtx.Deadline(); !hasDeadline {
			var cancel context.CancelFunc
			versionCtx, cancel = context.WithTimeout(versionCtx, 5*time.Second)
			defer cancel()
		}
		if versions, err := c.GetServiceVersions(versionCtx); err != nil {
			c.logf(ClientLogLevelDebug, "QMI: version info query failed (non-fatal): %v", err)
		} else {
			c.serviceVersions = ServiceVersionMap(versions)
			c.versionQueried = true
			c.logf(ClientLogLevelDebug, "QMI: modem 支持 %d 个 QMI 服务", len(versions))
		}
	}

	return c, nil
}

// HasService 查询 modem 是否支持指定的 QMI 服务。
// 如果尚未执行版本查询，返回 true（乐观假设）。
func (c *Client) HasService(service uint8) bool {
	if !c.versionQueried {
		return true
	}
	_, ok := c.serviceVersions[service]
	return ok
}

func (c *Client) ensureServiceAllocatable(service uint8) error {
	if !c.versionQueried {
		return nil
	}
	if _, ok := c.serviceVersions[service]; ok {
		return nil
	}
	return ErrServiceNotSupported
}

// GetCachedServiceVersions 返回缓存的服务版本信息。
// 如果尚未查询过，返回 nil。
func (c *Client) GetCachedServiceVersions() map[uint8]ServiceVersion {
	if !c.versionQueried {
		return nil
	}
	// 返回浅拷贝，避免外部修改
	result := make(map[uint8]ServiceVersion, len(c.serviceVersions))
	for k, v := range c.serviceVersions {
		result[k] = v
	}
	return result
}

func newClientWithTransport(path string, opts ClientOptions, conn qmiTransport) *Client {
	c := &Client{
		conn:               conn,
		path:               path,
		opts:               opts,
		transactions:       make(map[uint32]*transactionEntry),
		recentTransactions: make(map[uint32]recentTransaction),
		clientIDs:          make(map[uint8]map[uint8]struct{}),
		eventCh:            make(chan Event, opts.IndicationQueueSize),
		indicationInCh:     make(chan Event, opts.IndicationQueueSize),
		coalescedSignalCh:  make(chan struct{}, 1),
		writeCh:            make(chan writeRequest, opts.TxQueueSize),
		closeCh:            make(chan struct{}),
		deadCh:             make(chan struct{}),
		coalesced: coalescedEventStore{
			events: make(map[string]Event),
		},
	}

	// Start runtime loops / 启动运行时循环
	c.wg.Add(3)
	go c.readLoop()
	go c.writerLoop()
	go c.indicationLoop()

	return c
}

// Close shuts down the client / Close关闭客户端
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	var err error
	c.closeOnce.Do(func() {
		if c.closeCh != nil {
			close(c.closeCh)
		}
		if c.conn != nil {
			err = c.conn.Close()
		}
		c.wg.Wait()
		c.failPendingTransactions(fmt.Errorf("client closed"))
		if c.eventCh != nil {
			close(c.eventCh)
		}
	})
	return err
}

// Events returns a channel for receiving asynchronous indications / Events返回用于接收异步指示的通道
func (c *Client) Events() <-chan Event {
	return c.eventCh
}

// Stats returns a point-in-time snapshot of client runtime metrics.
func (c *Client) Stats() ClientStats {
	if c == nil {
		return ClientStats{}
	}
	return ClientStats{
		UnmatchedResponses:     c.unmatchedResponses.Load(),
		ParseErrors:            c.parseErrors.Load(),
		CoalescedIndications:   c.coalescedIndications.Load(),
		DroppedEdgeIndications: c.droppedEdgeIndications.Load(),
	}
}

func (c *Client) getFatalErr() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fatalErr
}

func (c *Client) handleFatalTransportError(err error) {
	if err == nil {
		err = io.EOF
	}
	fatal := fmt.Errorf("qmi transport closed: %w", err)
	c.mu.Lock()
	if c.fatalErr == nil {
		c.fatalErr = fatal
	} else {
		fatal = c.fatalErr
	}
	c.mu.Unlock()

	c.deadOnce.Do(func() {
		if c.deadCh != nil {
			close(c.deadCh)
		}
	})

	c.failPendingTransactions(fatal)
	c.enqueueIndication(Event{
		Type:      EventModemReset,
		ServiceID: ServiceControl,
	})
}

func (c *Client) allocateTransaction(service uint8, msgID uint16, respCh chan *Packet, deadline time.Time) (uint16, uint32, *transactionEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.fatalErr != nil {
		return 0, 0, nil, c.fatalErr
	}
	if c.closeCh != nil {
		select {
		case <-c.closeCh:
			return 0, 0, nil, fmt.Errorf("connection closed")
		default:
		}
	}

	if c.transactions == nil {
		c.transactions = make(map[uint32]*transactionEntry)
	}

	var maxAttempts int
	if service == ServiceControl {
		maxAttempts = 255
	} else {
		maxAttempts = 65535
	}

	for i := 0; i < maxAttempts; i++ {
		var cand uint16
		if service == ServiceControl {
			cand = uint16(atomic.AddUint32(&c.ctlTxID, 1) & 0xFF)
			if cand == 0 {
				cand = uint16(atomic.AddUint32(&c.ctlTxID, 1) & 0xFF)
			}
		} else {
			cand = uint16(atomic.AddUint32(&c.lastTxID, 1) & 0xFFFF)
			if cand == 0 {
				cand = uint16(atomic.AddUint32(&c.lastTxID, 1) & 0xFFFF)
			}
		}
		key := uint32(service)<<16 | uint32(cand)
		if _, exists := c.transactions[key]; exists {
			continue
		}
		entry := &transactionEntry{
			ch:       respCh,
			service:  service,
			msgID:    msgID,
			txID:     cand,
			start:    time.Now(),
			deadline: deadline,
		}
		c.transactions[key] = entry
		return cand, key, entry, nil
	}

	return 0, 0, nil, fmt.Errorf("QMI transaction IDs exhausted for service 0x%02x", service)
}

func (c *Client) completeTransaction(key uint32, entry *transactionEntry, cause error) {
	c.mu.Lock()
	if c.transactions[key] == entry {
		delete(c.transactions, key)
	}
	if isContextDone(cause) && entry != nil {
		c.rememberRecentTransactionLocked(key, entry, cause)
	}
	c.mu.Unlock()
}

func isContextDone(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (c *Client) rememberRecentTransactionLocked(key uint32, entry *transactionEntry, cause error) {
	if c.recentTransactions == nil {
		c.recentTransactions = make(map[uint32]recentTransaction)
	}
	now := time.Now()
	c.pruneRecentTransactionsLocked(now)
	if len(c.recentTransactions) >= maxRecentTransactions {
		var oldestKey uint32
		var oldest time.Time
		for k, recent := range c.recentTransactions {
			if oldest.IsZero() || recent.completedAt.Before(oldest) {
				oldestKey = k
				oldest = recent.completedAt
			}
		}
		delete(c.recentTransactions, oldestKey)
	}
	c.recentTransactions[key] = recentTransaction{
		service:     entry.service,
		msgID:       entry.msgID,
		txID:        entry.txID,
		start:       entry.start,
		deadline:    entry.deadline,
		completedAt: now,
		err:         cause.Error(),
	}
}

func (c *Client) pruneRecentTransactionsLocked(now time.Time) {
	for key, recent := range c.recentTransactions {
		if now.Sub(recent.completedAt) > recentTransactionTTL {
			delete(c.recentTransactions, key)
		}
	}
}

func (c *Client) isRecentTransaction(key uint32, service uint8, msgID uint16) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	recent, ok := c.recentTransactions[key]
	if !ok {
		return false
	}
	if time.Since(recent.completedAt) > recentTransactionTTL {
		delete(c.recentTransactions, key)
		return false
	}
	return recent.service == service && recent.msgID == msgID
}

func (c *Client) takeRecentTransaction(key uint32, service uint8, msgID uint16) (recentTransaction, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	recent, ok := c.recentTransactions[key]
	if !ok {
		return recentTransaction{}, false
	}
	expired := time.Since(recent.completedAt) > recentTransactionTTL
	if expired {
		delete(c.recentTransactions, key)
		return recentTransaction{}, false
	}
	if recent.service != service || recent.msgID != msgID {
		return recentTransaction{}, false
	}
	delete(c.recentTransactions, key)
	return recent, true
}

func (c *Client) logLateResponse(key uint32, packet *Packet, recent recentTransaction) {
	now := time.Now()
	c.mu.Lock()
	suppressed := c.lateResponseSuppressed
	if !c.lateResponseLastLog.IsZero() && now.Sub(c.lateResponseLastLog) < lateResponseLogMinInterval {
		c.lateResponseSuppressed++
		c.mu.Unlock()
		return
	}
	c.lateResponseLastLog = now
	c.lateResponseSuppressed = 0
	c.mu.Unlock()

	extra := ""
	if suppressed > 0 {
		extra = fmt.Sprintf(", suppressed %d similar late response(s)", suppressed)
	}
	c.logf(ClientLogLevelDebug, "QMI: received late response for completed transaction key 0x%08x (MsgID 0x%04x, Service 0x%02x, TxID %d, completed_err %q%s)",
		key, packet.MessageID, packet.ServiceType, packet.TransactionID, recent.err, extra)
}

// ============================================================================
// Read Loop - handles responses and indications / 读取循环 - 处理响应和指示
// ============================================================================

func (c *Client) readLoop() {
	defer c.wg.Done()
	buf := make([]byte, 16384)
	var pending []byte

	for {
		select {
		case <-c.closeCh:
			return
		default:
		}

		// Set read deadline to allow periodic checking of closeCh / 设置读取截止时间以允许定期检查closeCh
		_ = c.conn.SetReadDeadline(time.Now().Add(c.opts.ReadDeadline))

		n, err := c.conn.Read(buf)
		if err != nil {
			if os.IsTimeout(err) || errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			select {
			case <-c.closeCh:
				return
			default:
			}
			c.logf(ClientLogLevelWarn, "QMI: read failed: %v", err)
			c.handleFatalTransportError(err)
			return
		}

		if n <= 0 {
			continue
		}

		pending = append(pending, buf[:n]...)

		for {
			if len(pending) < 3 {
				break
			}
			if pending[0] != 0x01 {
				i := 0
				for i < len(pending) && pending[i] != 0x01 {
					i++
				}
				if i == len(pending) {
					pending = pending[:0]
					break
				}
				pending = pending[i:]
				continue
			}
			if len(pending) < QmuxHeaderSize {
				break
			}

			frameLen := 1 + int(binary.LittleEndian.Uint16(pending[1:3]))
			if frameLen < QmuxHeaderSize {
				pending = pending[1:]
				continue
			}
			if len(pending) < frameLen {
				break
			}

			frame := make([]byte, frameLen)
			copy(frame, pending[:frameLen])
			pending = pending[frameLen:]

			packet, err := UnmarshalPacket(frame)
			if err != nil {
				c.parseErrors.Add(1)
				c.logf(ClientLogLevelWarn, "QMI: failed to parse packet (%d bytes): %v", frameLen, err)
				continue
			}

			key := uint32(packet.ServiceType)<<16 | uint32(packet.TransactionID)
			c.mu.Lock()
			entry, ok := c.transactions[key]
			if ok && !packet.IsIndication {
				delete(c.transactions, key)
			}
			c.mu.Unlock()

			if !packet.IsIndication {
				if ok {
					select {
					case entry.ch <- packet:
					default:
					}
					continue
				}
				if packet.ServiceType != ServiceControl {
					c.unmatchedResponses.Add(1)
					if recent, ok := c.takeRecentTransaction(key, packet.ServiceType, packet.MessageID); ok {
						c.logLateResponse(key, packet, recent)
						continue
					}
					c.logf(ClientLogLevelDebug, "QMI: received response with unmatched key 0x%08x (MsgID 0x%04x, Service 0x%02x, TxID %d)",
						key, packet.MessageID, packet.ServiceType, packet.TransactionID)
				} else if recent, ok := c.takeRecentTransaction(key, packet.ServiceType, packet.MessageID); ok {
					c.unmatchedResponses.Add(1)
					c.logLateResponse(key, packet, recent)
				}
				continue
			}
			c.dispatchIndication(packet)
		}
	}
}

func (c *Client) writerLoop() {
	defer c.wg.Done()

	for {
		select {
		case <-c.closeCh:
			return
		case req := <-c.writeCh:
			var err error
			if fatalErr := c.getFatalErr(); fatalErr != nil {
				err = fatalErr
			} else {
				err = c.writeAll(req.data)
			}
			select {
			case req.result <- err:
			default:
			}
		}
	}
}

func (c *Client) indicationLoop() {
	defer c.wg.Done()

	for {
		if evt, ok := c.popCoalescedEvent(); ok {
			if !c.deliverEvent(evt) {
				return
			}
			continue
		}

		select {
		case <-c.closeCh:
			return
		case evt := <-c.indicationInCh:
			if !c.deliverEvent(evt) {
				return
			}
		case <-c.coalescedSignalCh:
		}
	}
}

func (c *Client) deliverEvent(evt Event) bool {
	select {
	case c.eventCh <- evt:
		return true
	case <-c.closeCh:
		return false
	}
}

func (c *Client) writeAll(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if !c.opts.UseProxy {
		// On raw QMI character devices (/dev/cdc-wdm*), each write(2) submits a
		// single atomic QMUX URB. Sending a partial tail in a second write()
		// corrupts framing on the modem.
		n, err := c.conn.Write(data)
		if err != nil {
			return fmt.Errorf("write failed: %w", err)
		}
		if n != len(data) {
			return fmt.Errorf("write failed: %w", io.ErrShortWrite)
		}
		return nil
	}

	written := 0
	for written < len(data) {
		n, err := c.conn.Write(data[written:])
		if err != nil {
			return fmt.Errorf("write failed: %w", err)
		}
		if n <= 0 {
			return fmt.Errorf("write failed: %w", io.ErrShortWrite)
		}
		written += n
	}
	return nil
}

func (c *Client) failPendingTransactions(cause error) {
	c.mu.Lock()
	for key, entry := range c.transactions {
		delete(c.transactions, key)
		close(entry.ch)
	}
	c.mu.Unlock()
}

func (c *Client) enqueueIndication(event Event) {
	if c.indicationInCh == nil {
		select {
		case c.eventCh <- event:
		default:
		}
		return
	}

	if key, ok := c.coalescingKey(event); ok {
		select {
		case c.indicationInCh <- event:
			return
		default:
			c.storeCoalescedEvent(key, event)
			c.coalescedIndications.Add(1)
			select {
			case c.coalescedSignalCh <- struct{}{}:
			default:
			}
			return
		}
	}

	timer := time.NewTimer(c.opts.ReadDeadline)
	defer timer.Stop()

	select {
	case c.indicationInCh <- event:
	case <-c.closeCh:
	case <-timer.C:
		c.droppedEdgeIndications.Add(1)
		c.logf(ClientLogLevelWarn, "QMI: dropping edge indication type=%d service=0x%02x msg=0x%04x because indication queue is full",
			event.Type, event.ServiceID, event.MessageID)
	}
}

func (c *Client) storeCoalescedEvent(key string, event Event) {
	c.coalescedMu.Lock()
	defer c.coalescedMu.Unlock()

	if _, exists := c.coalesced.events[key]; !exists {
		c.coalesced.order = append(c.coalesced.order, key)
	}
	c.coalesced.events[key] = event
}

func (c *Client) popCoalescedEvent() (Event, bool) {
	c.coalescedMu.Lock()
	defer c.coalescedMu.Unlock()

	for len(c.coalesced.order) > 0 {
		key := c.coalesced.order[0]
		c.coalesced.order = c.coalesced.order[1:]
		event, ok := c.coalesced.events[key]
		delete(c.coalesced.events, key)
		if ok {
			return event, true
		}
	}
	return Event{}, false
}

func (c *Client) coalescingKey(event Event) (string, bool) {
	var clientID uint8
	if event.Packet != nil {
		clientID = event.Packet.ClientID
	}
	switch event.Type {
	case EventPacketServiceStatusChanged:
		return fmt.Sprintf("packet-status:%d:%d:%d", event.ServiceID, clientID, event.MessageID), true
	case EventServingSystemChanged:
		return fmt.Sprintf("serving-system:%d:%d:%d", event.ServiceID, clientID, event.MessageID), true
	case EventNASOperatorNameChanged:
		return fmt.Sprintf("nas-operator-name:%d:%d:%d", event.ServiceID, clientID, event.MessageID), true
	case EventNASNetworkTimeChanged:
		return fmt.Sprintf("nas-network-time:%d:%d:%d", event.ServiceID, clientID, event.MessageID), true
	case EventNASSignalInfoChanged:
		return fmt.Sprintf("nas-signal-info:%d:%d:%d", event.ServiceID, clientID, event.MessageID), true
	case EventNASNetworkReject:
		return fmt.Sprintf("nas-network-reject:%d:%d:%d", event.ServiceID, clientID, event.MessageID), true
	case EventNASIncrementalNetworkScan:
		return fmt.Sprintf("nas-incremental-scan:%d:%d:%d", event.ServiceID, clientID, event.MessageID), true
	case EventNASEventReport:
		return fmt.Sprintf("nas-event-report:%d:%d:%d", event.ServiceID, clientID, event.MessageID), true
	case EventWMSTransportNetworkRegistrationStatus:
		return fmt.Sprintf("wms-transport:%d:%d:%d", event.ServiceID, clientID, event.MessageID), true
	case EventModemReset:
		if event.Packet != nil {
			if tlv := FindTLV(event.Packet.TLVs, 0x01); tlv != nil && len(tlv.Value) >= 2 {
				return fmt.Sprintf("critical-modem-reset:%d:%d:%d:%d", event.ServiceID, event.MessageID, tlv.Value[0], tlv.Value[1]), true
			}
		}
		return fmt.Sprintf("critical-modem-reset:%d:%d", event.ServiceID, event.MessageID), true
	case EventUIMSessionClosed:
		var sessionType uint8 = 0xFF
		if event.Packet != nil {
			if tlv := FindTLV(event.Packet.TLVs, 0x10); tlv != nil && len(tlv.Value) >= 1 {
				sessionType = tlv.Value[0]
			} else if tlv := FindTLV(event.Packet.TLVs, 0x01); tlv != nil && len(tlv.Value) >= 1 {
				sessionType = tlv.Value[0]
			}
		}
		return fmt.Sprintf("critical-uim-session-closed:%d:%d:%d:%d", event.ServiceID, clientID, event.MessageID, sessionType), true
	default:
		return "", false
	}
}

// dispatchIndication sends an indication to the event channel / dispatchIndication将指示发送到事件通道
func (c *Client) dispatchIndication(p *Packet) {
	var eventType EventType

	switch {
	case p.ServiceType == ServiceControl && p.MessageID == CTLRevokeClientIDInd:
		c.handleClientIDRevoke(p)
		eventType = EventModemReset
	case p.ServiceType == ServiceWDS && p.MessageID == WDSGetPktSrvcStatusInd:
		eventType = EventPacketServiceStatusChanged
	case p.ServiceType == ServiceNAS && (p.MessageID == NASServingSystemInd || p.MessageID == NASSysInfoInd):
		eventType = EventServingSystemChanged
	case p.ServiceType == ServiceNAS && p.MessageID == NASEventReportInd:
		eventType = EventNASEventReport
	case p.ServiceType == ServiceNAS && p.MessageID == NASOperatorNameInd:
		eventType = EventNASOperatorNameChanged
	case p.ServiceType == ServiceNAS && p.MessageID == NASNetworkTimeInd:
		eventType = EventNASNetworkTimeChanged
	case p.ServiceType == ServiceNAS && p.MessageID == NASSignalInfoInd:
		eventType = EventNASSignalInfoChanged
	case p.ServiceType == ServiceNAS && p.MessageID == NASNetworkRejectInd:
		eventType = EventNASNetworkReject
	case p.ServiceType == ServiceNAS && p.MessageID == NASIncrementalNetworkScanInd:
		eventType = EventNASIncrementalNetworkScan
	case p.ServiceType == ServiceWMS && p.MessageID == WMSEventReportInd:
		eventType = EventNewMessage
	case p.ServiceType == ServiceWMS && p.MessageID == WMSSMSCAddressInd:
		eventType = EventWMSSMSCAddress
	case p.ServiceType == ServiceWMS && p.MessageID == WMSTransportNetworkRegistrationStatusInd:
		eventType = EventWMSTransportNetworkRegistrationStatus
	case p.ServiceType == ServiceIMSA && p.MessageID == IMSARegistrationStatusChanged:
		eventType = EventIMSRegistrationStatus
	case p.ServiceType == ServiceIMSA && p.MessageID == IMSAServicesStatusChanged:
		eventType = EventIMSServicesStatus
	case p.ServiceType == ServiceIMS && p.MessageID == IMSSettingsChangedInd:
		eventType = EventIMSSettingsChanged
	case p.ServiceType == ServiceVOICE && p.MessageID == VOICEAllCallStatusInd:
		eventType = EventVoiceCallStatus
	case p.ServiceType == ServiceVOICE && p.MessageID == VOICESupplementaryServiceInd:
		eventType = EventVoiceSupplementaryService
	case p.ServiceType == ServiceVOICE && p.MessageID == VOICESupplementaryServiceRequestInd:
		eventType = EventVoiceSupplementaryServiceRequest
	case p.ServiceType == ServiceVOICE && p.MessageID == VOICEUSSDInd:
		eventType = EventUSSD
	case p.ServiceType == ServiceVOICE && p.MessageID == VOICEReleaseUSSDInd:
		eventType = EventVoiceUSSDReleased
	case p.ServiceType == ServiceVOICE && p.MessageID == VOICEOriginateUSSDNoWait:
		eventType = EventVoiceUSSDNoWaitResult
	case p.ServiceType == ServiceUIM && p.MessageID == UIMStatusChangeInd:
		eventType = EventSimStatusChanged
	case p.ServiceType == ServiceUIM && p.MessageID == UIMSessionClosedInd:
		eventType = EventUIMSessionClosed
	case p.ServiceType == ServiceUIM && p.MessageID == UIMRefreshInd:
		eventType = EventUIMRefresh
	case p.ServiceType == ServiceUIM && p.MessageID == UIMSlotStatusInd:
		eventType = EventUIMSlotStatus
	default:
		eventType = EventUnknown
	}

	event := Event{
		Type:      eventType,
		ServiceID: p.ServiceType,
		MessageID: p.MessageID,
		Packet:    p,
	}
	c.enqueueIndication(event)
}

func (c *Client) addClientID(service uint8, clientID uint8) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.clientIDs == nil {
		c.clientIDs = make(map[uint8]map[uint8]struct{})
	}
	if c.clientIDs[service] == nil {
		c.clientIDs[service] = make(map[uint8]struct{})
	}
	c.clientIDs[service][clientID] = struct{}{}
}

func (c *Client) removeClientID(service uint8, clientID uint8) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	ids, ok := c.clientIDs[service]
	if !ok {
		return false
	}
	if _, exists := ids[clientID]; !exists {
		return false
	}
	delete(ids, clientID)
	if len(ids) == 0 {
		delete(c.clientIDs, service)
	}
	return true
}

func (c *Client) handleClientIDRevoke(p *Packet) {
	if p.ServiceType != ServiceControl || p.MessageID != CTLRevokeClientIDInd {
		return
	}
	tlv := FindTLV(p.TLVs, 0x01)
	if tlv == nil || len(tlv.Value) < 2 {
		return
	}
	service := tlv.Value[0]
	clientID := tlv.Value[1]
	c.removeClientID(service, clientID)
}

// ============================================================================
// Request/Response handling / 请求/响应处理
// ============================================================================

// SendRequest sends a QMI request and waits for response / SendRequest发送QMI请求并等待响应
func (c *Client) SendRequest(ctx context.Context, service uint8, clientID uint8, msgID uint16, tlvs []TLV) (resp *Packet, err error) {
	if c == nil || c.writeCh == nil {
		return nil, fmt.Errorf("qmi client not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, ok := ctx.Deadline(); !ok && c.opts.DefaultRequestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.opts.DefaultRequestTimeout)
		defer cancel()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Allocate transaction ID and register response channel without colliding with in-flight transactions
	respCh := make(chan *Packet, 1)
	deadline, _ := ctx.Deadline()
	txID, key, entry, allocErr := c.allocateTransaction(service, msgID, respCh, deadline)
	if allocErr != nil {
		return nil, allocErr
	}

	defer func() {
		c.completeTransaction(key, entry, err)
	}()

	// Build packet / 构建数据包
	p := Packet{
		ServiceType:   service,
		ClientID:      clientID,
		TransactionID: txID,
		MessageID:     msgID,
		TLVs:          tlvs,
	}

	writeReq := writeRequest{
		data:   p.Marshal(),
		result: make(chan error, 1),
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case c.writeCh <- writeReq:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.deadCh:
		if fatalErr := c.getFatalErr(); fatalErr != nil {
			return nil, fatalErr
		}
		return nil, fmt.Errorf("connection closed")
	case <-c.closeCh:
		return nil, fmt.Errorf("connection closed")
	}

	drainResp := func() *Packet {
		select {
		case pkt, ok := <-respCh:
			if ok && pkt != nil {
				return pkt
			}
		default:
		}
		return nil
	}

	select {
	case err := <-writeReq.result:
		if err != nil {
			if pkt := drainResp(); pkt != nil {
				return pkt, nil
			}
			return nil, err
		}
	case <-ctx.Done():
		if pkt := drainResp(); pkt != nil {
			return pkt, nil
		}
		return nil, ctx.Err()
	case <-c.deadCh:
		if pkt := drainResp(); pkt != nil {
			return pkt, nil
		}
		if fatalErr := c.getFatalErr(); fatalErr != nil {
			return nil, fatalErr
		}
		return nil, fmt.Errorf("connection closed")
	case <-c.closeCh:
		if pkt := drainResp(); pkt != nil {
			return pkt, nil
		}
		return nil, fmt.Errorf("connection closed")
	}

	// Wait for response / 等待响应
	select {
	case resp, ok := <-respCh:
		if !ok || resp == nil {
			if fatalErr := c.getFatalErr(); fatalErr != nil {
				return nil, fatalErr
			}
			return nil, fmt.Errorf("connection closed")
		}
		return resp, nil
	case <-ctx.Done():
		if pkt := drainResp(); pkt != nil {
			return pkt, nil
		}
		return nil, ctx.Err()
	case <-c.deadCh:
		if pkt := drainResp(); pkt != nil {
			return pkt, nil
		}
		if fatalErr := c.getFatalErr(); fatalErr != nil {
			return nil, fatalErr
		}
		return nil, fmt.Errorf("connection closed")
	case <-c.closeCh:
		if pkt := drainResp(); pkt != nil {
			return pkt, nil
		}
		return nil, fmt.Errorf("connection closed")
	}
}

// Sync sends a QMI CTL sync request / Sync发送QMI CTL同步请求
func (c *Client) Sync(ctx context.Context) error {
	_, err := c.SendRequest(ctx, ServiceControl, 0, CTLSync, nil)
	return err
}

func (c *Client) openProxyDevice(ctx context.Context, devicePath string) error {
	resp, err := c.SendRequest(ctx, ServiceControl, 0, CTLInternalProxyOpen, []TLV{
		NewTLVString(TLVProxyDevicePath, devicePath),
	})
	if err != nil {
		return fmt.Errorf("qmi-proxy open %s: %w", devicePath, err)
	}
	if err := resp.CheckResult(); err != nil {
		return fmt.Errorf("qmi-proxy open %s: %w", devicePath, err)
	}
	return nil
}

// AllocateClientID requests a client ID for the given service / AllocateClientID为给定服务请求客户端ID
func (c *Client) AllocateClientID(service uint8) (uint8, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return c.AllocateClientIDWithContext(ctx, service)
}

func (c *Client) AllocateClientIDWithContext(ctx context.Context, service uint8) (uint8, error) {
	if err := c.ensureServiceAllocatable(service); err != nil {
		return 0, err
	}

	var lastErr error
	for retry := 0; retry < 3; retry++ {
		attemptCtx, attemptCancel := context.WithTimeout(ctx, 20*time.Second)

		// Build request: TLV 0x01 = service type / 构建请求: TLV 0x01 = 服务类型
		tlvs := []TLV{NewTLVUint8(0x01, service)}

		resp, err := c.SendRequest(attemptCtx, ServiceControl, 0, CTLGetClientID, tlvs)
		attemptCancel()
		if err == nil {
			if err := resp.CheckResult(); err != nil {
				return 0, err
			}

			// Parse response TLV 0x01: {service, clientID} / 解析响应 TLV 0x01: {服务, clientID}
			tlv := FindTLV(resp.TLVs, 0x01)
			if tlv == nil || len(tlv.Value) < 2 {
				return 0, fmt.Errorf("invalid response TLV")
			}

			clientID := tlv.Value[1]
			c.addClientID(service, clientID)

			return clientID, nil
		}
		lastErr = err
		if ctx.Err() != nil || c.getFatalErr() != nil {
			break
		}
		retryTimer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			retryTimer.Stop()
		case <-c.closeCh:
			retryTimer.Stop()
		case <-c.deadCh:
			retryTimer.Stop()
		case <-retryTimer.C:
		}
		if ctx.Err() != nil || c.getFatalErr() != nil {
			break
		}
	}

	return 0, fmt.Errorf("allocate client ID request failed after retries: %w", lastErr)
}

// ReleaseClientID releases a client ID for the given service / ReleaseClientID释放给定服务的客户端ID
func (c *Client) ReleaseClientID(service uint8, clientID uint8) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.ReleaseClientIDWithContext(ctx, service, clientID)
}

func (c *Client) ReleaseClientIDWithContext(ctx context.Context, service uint8, clientID uint8) error {
	if c == nil {
		return nil
	}
	// Build request: TLV 0x01 = {service, clientID} / 构建请求: TLV 0x01 = {服务, clientID}
	tlvs := []TLV{{Type: 0x01, Value: []byte{service, clientID}}}

	resp, err := c.SendRequest(ctx, ServiceControl, 0, CTLReleaseClientID, tlvs)
	if err != nil {
		return fmt.Errorf("release client ID request failed: %w", err)
	}

	if err := resp.CheckResult(); err != nil {
		return err
	}

	c.removeClientID(service, clientID)

	return nil
}

// GetClientID returns a cached client ID for a service, or 0 if not allocated / GetClientID返回服务的缓存客户端ID，如果未分配则返回0
func (c *Client) GetClientID(service uint8) uint8 {
	ids := c.GetClientIDs(service)
	if len(ids) == 0 {
		return 0
	}
	return ids[0]
}

// GetClientIDs returns all cached client IDs allocated for a service in ascending order.
func (c *Client) GetClientIDs(service uint8) []uint8 {
	c.mu.Lock()
	defer c.mu.Unlock()
	set := c.clientIDs[service]
	if len(set) == 0 {
		return nil
	}
	out := make([]uint8, 0, len(set))
	for cid := range set {
		out = append(out, cid)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
