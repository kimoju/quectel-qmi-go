package manager

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
// ModemPool - 多模组管理池
// 管理多个 4G 模组，提供负载均衡和批量操作
// ============================================================================

// ModemPool manages multiple modem connections / ModemPool 管理多个模组连接
type ModemPool struct {
	mu       sync.RWMutex
	modems   map[string]*Manager // key = interface name (e.g. "wwan0") or control/USB path fallback
	selector Selector            // Load balancing strategy / 负载均衡策略
	log      Logger
	events   *EventEmitter

	poolEventsMu      sync.RWMutex
	poolEventHandlers []PoolEventHandler

	// Health monitoring / 健康监控
	healthCtx    context.Context
	healthCancel context.CancelFunc
	healthStatus map[string]*HealthStatus

	// Hot-plug / 热插拔
	hotplugCtx    context.Context
	hotplugCancel context.CancelFunc
	baseCfg       Config // Base config for auto-discovered modems / 自动发现模组的基础配置
	baseLogger    Logger // Logger for auto-discovered modems / 自动发现模组的日志器
}

func modemPoolKey(dev ModemDevice) string {
	if iface := strings.TrimSpace(dev.NetInterface); iface != "" {
		return iface
	}
	if ctrl := strings.TrimSpace(dev.ControlPath); ctrl != "" {
		return ctrl
	}
	return strings.TrimSpace(dev.USBPath)
}

func (p *ModemPool) logger() Logger {
	p.mu.RLock()
	l := p.log
	p.mu.RUnlock()
	if l == nil {
		return NewNopLogger()
	}
	return l
}

// HealthStatus represents the health of a modem / HealthStatus 表示模组的健康状态
type HealthStatus struct {
	Name       string    // Modem name / 模组名称
	Connected  bool      // Is connected / 是否已连接
	SignalRSSI int8      // Signal strength (dBm) / 信号强度 (dBm)
	LastCheck  time.Time // Last health check time / 上次健康检查时间
	LastError  error     // Last error if any / 上次错误 (如有)
}

// NewPool creates a new modem pool / NewPool 创建新的模组池
func NewPool() *ModemPool {
	return &ModemPool{
		modems:       make(map[string]*Manager),
		selector:     &RoundRobinSelector{},
		log:          NewNopLogger(),
		events:       NewEventEmitter(),
		healthStatus: make(map[string]*HealthStatus),
	}
}

// SetLogger sets the logger for the pool / SetLogger 设置池的日志器
func (p *ModemPool) SetLogger(logger Logger) {
	if logger == nil {
		logger = NewNopLogger()
	}
	p.mu.Lock()
	p.log = logger.WithField("component", "pool")
	p.mu.Unlock()
}

// SetSelector sets the modem selection strategy / SetSelector 设置模组选择策略
func (p *ModemPool) SetSelector(s Selector) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.selector = s
}

// ============================================================================
// 模组管理
// ============================================================================

// Add adds a managed modem to the pool / Add 向池中添加托管的模组
func (p *ModemPool) Add(name string, cfg Config, logger Logger) (*Manager, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.modems[name]; exists {
		return nil, fmt.Errorf("modem %s already exists in pool", name)
	}

	mgr := New(cfg, logger)
	p.modems[name] = mgr
	p.log.Infof("Added modem: %s (%s)", name, cfg.Device.NetInterface)
	return mgr, nil
}

// Remove removes a modem from the pool / Remove 从池中移除模组
func (p *ModemPool) Remove(name string) error {
	p.mu.Lock()
	mgr, exists := p.modems[name]
	if !exists {
		p.mu.Unlock()
		return fmt.Errorf("modem %s not found in pool", name)
	}
	delete(p.modems, name)
	delete(p.healthStatus, name)
	p.mu.Unlock()

	_ = mgr.Stop()
	p.logger().Infof("Removed modem: %s", name)
	return nil
}

// Get returns a modem using the configured selector / Get 使用配置的选择器返回模组
func (p *ModemPool) Get() *Manager {
	p.mu.RLock()
	if len(p.modems) == 0 {
		p.mu.RUnlock()
		return nil
	}

	managers := p.getConnectedManagers()
	if len(managers) == 0 {
		// No connected modems, return any / 没有已连接的模组，返回任意一个
		var anyMgr *Manager
		for _, mgr := range p.modems {
			anyMgr = mgr
			break
		}
		p.mu.RUnlock()
		return anyMgr
	}
	selector := p.selector
	p.mu.RUnlock()

	if selector == nil {
		return managers[0]
	}
	return selector.Select(managers)
}

// GetByName returns a specific modem by name / GetByName 通过名称获取特定模组
func (p *ModemPool) GetByName(name string) *Manager {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.modems[name]
}

// GetHealthy returns a connected modem with best signal / GetHealthy 返回信号最好的已连接模组
func (p *ModemPool) GetHealthy() *Manager {
	p.mu.RLock()
	managers := p.getConnectedManagers()
	p.mu.RUnlock()

	if len(managers) == 0 {
		return nil
	}

	// Use signal strength selector for healthy check / 使用信号强度选择器进行健康检查
	selector := &SignalStrengthSelector{}
	return selector.Select(managers)
}

// All returns a copy of all modems / All 返回所有模组的副本
func (p *ModemPool) All() map[string]*Manager {
	p.mu.RLock()
	defer p.mu.RUnlock()

	result := make(map[string]*Manager, len(p.modems))
	for k, v := range p.modems {
		result[k] = v
	}
	return result
}

// Size returns the number of modems in pool / Size 返回池中模组数量
func (p *ModemPool) Size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.modems)
}

// ConnectedCount returns the number of connected modems / ConnectedCount 返回已连接的模组数量
func (p *ModemPool) ConnectedCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.getConnectedManagers())
}

func (p *ModemPool) getConnectedManagers() []*Manager {
	var result []*Manager
	for _, mgr := range p.modems {
		if mgr.State() == StateConnected {
			result = append(result, mgr)
		}
	}
	return result
}

// ============================================================================
// 批量操作
// ============================================================================

// StartAll starts all modems / StartAll 启动所有模组
func (p *ModemPool) StartAll() error {
	p.mu.RLock()
	modems := make([]*Manager, 0, len(p.modems))
	for _, mgr := range p.modems {
		modems = append(modems, mgr)
	}
	p.mu.RUnlock()

	var wg sync.WaitGroup
	errCh := make(chan error, len(modems))

	for _, mgr := range modems {
		wg.Add(1)
		go func(m *Manager) {
			defer wg.Done()
			if err := m.Start(); err != nil {
				errCh <- err
			}
		}(mgr)
	}

	wg.Wait()
	close(errCh)

	// Collect errors / 收集错误
	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to start %d modems", len(errs))
	}
	return nil
}

// StopAll stops background monitors and all modems / StopAll 停止后台监控及所有模组
func (p *ModemPool) StopAll() {
	p.StopHotPlug()
	p.StopHealthMonitor()

	p.mu.RLock()
	modems := make([]*Manager, 0, len(p.modems))
	for _, mgr := range p.modems {
		modems = append(modems, mgr)
	}
	p.mu.RUnlock()

	var wg sync.WaitGroup
	for _, mgr := range modems {
		wg.Add(1)
		go func(m *Manager) {
			defer wg.Done()
			_ = m.Stop()
		}(mgr)
	}
	wg.Wait()
}

// ============================================================================
// 自动发现
// ============================================================================

// DiscoverAndAdd discovers modems and adds them to pool / DiscoverAndAdd 发现模组并添加到池
func (p *ModemPool) DiscoverAndAdd(baseCfg Config, logger Logger) (int, error) {
	discoverFn := getDeviceDiscoverer()
	if discoverFn == nil {
		return 0, fmt.Errorf("no device discoverer registered; import github.com/kimoju/quectel-qmi-go/pkg/device or inject devices manually")
	}
	modems, err := discoverFn()
	if err != nil {
		return 0, err
	}

	added := 0
	for _, modem := range modems {
		key := modemPoolKey(modem)
		if key == "" {
			p.logger().Warn("Skipping discovered modem with empty identifier")
			continue
		}
		cfg := baseCfg
		cfg.Device = modem

		_, err := p.Add(key, cfg, logger)
		if err != nil {
			p.logger().WithError(err).Warnf("Failed to add modem %s", key)
			continue
		}
		added++
	}

	return added, nil
}

// ============================================================================
// 健康监控
// ============================================================================

// StartHealthMonitor starts periodic health checks / StartHealthMonitor 启动定期健康检查
func (p *ModemPool) StartHealthMonitor(interval time.Duration) {
	p.mu.Lock()
	if p.healthCtx != nil {
		p.mu.Unlock()
		return // Already running / 已在运行
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.healthCtx = ctx
	p.healthCancel = cancel
	p.mu.Unlock()

	go p.healthLoop(ctx, interval)
	p.logger().Infof("Health monitor started (interval=%s)", interval)
}

// StopHealthMonitor stops the health monitor / StopHealthMonitor 停止健康监控
func (p *ModemPool) StopHealthMonitor() {
	p.mu.Lock()
	cancel := p.healthCancel
	p.healthCancel = nil
	p.healthCtx = nil
	p.mu.Unlock()

	if cancel != nil {
		cancel()
		p.logger().Info("Health monitor stopped")
	}
}

func (p *ModemPool) healthLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.checkHealth(ctx)
		}
	}
}

func queryManagerSignalRSSI(parentCtx context.Context, mgr *Manager) (int8, bool, error) {
	if mgr == nil {
		return 0, false, nil
	}
	mgr.mu.RLock()
	nas := mgr.nas
	hook := mgr.querySignalStrength
	timeout := mgr.cfg.Timeouts.StatusCheck
	mgr.mu.RUnlock()
	if nas == nil && hook == nil {
		return 0, false, nil
	}
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()
	if hook != nil {
		sig, err := hook(ctx)
		if err != nil {
			return 0, true, err
		}
		if sig == nil {
			return 0, false, nil
		}
		return sig.RSSI, true, nil
	}
	sig, err := nas.GetSignalStrength(ctx)
	if err != nil {
		return 0, true, err
	}
	if sig == nil {
		return 0, false, nil
	}
	return sig.RSSI, true, nil
}

func (p *ModemPool) checkHealth(ctx context.Context) {
	p.mu.RLock()
	names := make([]string, 0, len(p.modems))
	modems := make([]*Manager, 0, len(p.modems))
	for name, mgr := range p.modems {
		names = append(names, name)
		modems = append(modems, mgr)
	}
	p.mu.RUnlock()

	for i, mgr := range modems {
		if ctx != nil && ctx.Err() != nil {
			return
		}
		name := names[i]
		status := &HealthStatus{
			Name:      name,
			LastCheck: time.Now(),
		}

		status.Connected = mgr.State() == StateConnected

		if rssi, attempted, err := queryManagerSignalRSSI(ctx, mgr); attempted {
			if err != nil {
				status.LastError = err
			} else {
				status.SignalRSSI = rssi
			}
		}

		p.mu.Lock()
		if _, stillExists := p.modems[name]; stillExists {
			p.healthStatus[name] = status
		}
		p.mu.Unlock()
	}
}

// Health returns the current health status of all modems / Health 返回所有模组的当前健康状态
func (p *ModemPool) Health() map[string]*HealthStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()

	result := make(map[string]*HealthStatus, len(p.healthStatus))
	for k, v := range p.healthStatus {
		if v == nil {
			continue
		}
		copied := *v
		result[k] = &copied
	}
	return result
}

// ============================================================================
// 热插拔监控
// ============================================================================

// PoolEventType represents pool-level events / PoolEventType 表示池级别事件
type PoolEventType int

const (
	PoolEventModemAdded   PoolEventType = iota // Modem added to pool / 模组添加到池
	PoolEventModemRemoved                      // Modem removed from pool / 模组从池移除
)

// PoolEvent represents a pool-level event / PoolEvent 表示池级别事件
type PoolEvent struct {
	Type  PoolEventType
	Name  string // Modem name / 模组名称
	Modem *Manager
}

// PoolEventHandler is a callback for pool events / PoolEventHandler 是池事件的回调
type PoolEventHandler func(event PoolEvent)

// WatchHotPlug starts watching for modem hot-plug events / WatchHotPlug 开始监控模组热插拔事件
func (p *ModemPool) WatchHotPlug(baseCfg Config, logger Logger, interval time.Duration) {
	p.mu.Lock()
	if p.hotplugCtx != nil {
		p.mu.Unlock()
		return // Already running / 已在运行
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.baseCfg = baseCfg
	p.baseLogger = logger
	p.hotplugCtx = ctx
	p.hotplugCancel = cancel
	p.mu.Unlock()

	go p.hotplugLoop(ctx, interval)
	p.logger().Infof("Hot-plug watcher started (interval=%s)", interval)
}

// StopHotPlug stops watching for hot-plug events / StopHotPlug 停止监控热插拔
func (p *ModemPool) StopHotPlug() {
	p.mu.Lock()
	cancel := p.hotplugCancel
	p.hotplugCancel = nil
	p.hotplugCtx = nil
	p.mu.Unlock()

	if cancel != nil {
		cancel()
		p.logger().Info("Hot-plug watcher stopped")
	}
}

func (p *ModemPool) hotplugLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.scanForChanges()
		}
	}
}

func (p *ModemPool) scanForChanges() {
	discoverFn := getDeviceDiscoverer()
	if discoverFn == nil {
		p.logger().Debug("Hot-plug scan skipped: no device discoverer registered")
		return
	}
	// Discover currently available modems / 发现当前可用的模组
	discovered, err := discoverFn()
	if err != nil {
		p.logger().WithError(err).Debug("Hot-plug scan failed")
		return
	}

	discoveredMap := make(map[string]ModemDevice, len(discovered))
	for _, m := range discovered {
		key := modemPoolKey(m)
		if key == "" {
			continue
		}
		discoveredMap[key] = m
	}

	type poolEntry struct {
		name string
		mgr  *Manager
	}
	var removed []poolEntry
	var added []poolEntry

	p.mu.Lock()
	for name, mgr := range p.modems {
		if _, exists := discoveredMap[name]; !exists {
			removed = append(removed, poolEntry{name: name, mgr: mgr})
			delete(p.modems, name)
			delete(p.healthStatus, name)
		}
	}
	baseCfg := p.baseCfg
	baseLogger := p.baseLogger
	poolLog := p.log
	p.mu.Unlock()
	if poolLog == nil {
		poolLog = NewNopLogger()
	}

	p.mu.Lock()
	for name, dev := range discoveredMap {
		if _, exists := p.modems[name]; !exists {
			poolLog.Infof("Hot-plug: new modem %s detected", name)
			cfg := baseCfg
			cfg.Device = dev
			mgr := New(cfg, baseLogger)
			p.modems[name] = mgr
			added = append(added, poolEntry{name: name, mgr: mgr})
		}
	}
	p.mu.Unlock()

	for _, item := range removed {
		p.logger().Warnf("Hot-plug: modem %s removed", item.name)
		_ = item.mgr.Stop()
		p.events.Emit(Event{Type: EventDisconnected})
		p.emitPoolEvent(PoolEvent{
			Type:  PoolEventModemRemoved,
			Name:  item.name,
			Modem: item.mgr,
		})
	}

	for _, item := range added {
		p.emitPoolEvent(PoolEvent{
			Type:  PoolEventModemAdded,
			Name:  item.name,
			Modem: item.mgr,
		})
		go func(m *Manager) {
			if err := m.Start(); err != nil {
				p.logger().WithError(err).Warnf("Failed to start hot-plugged modem")
			}
		}(item.mgr)
	}
}

// ============================================================================
// 事件
// ============================================================================

// OnEvent registers a callback for manager-style events / OnEvent 为事件注册回调
func (p *ModemPool) OnEvent(handler EventHandler) {
	p.events.On(handler)
}

// OnPoolEvent registers a callback for pool-level hot-plug events (added/removed).
func (p *ModemPool) OnPoolEvent(handler PoolEventHandler) {
	if handler == nil {
		return
	}
	p.poolEventsMu.Lock()
	p.poolEventHandlers = append(p.poolEventHandlers, handler)
	p.poolEventsMu.Unlock()
}

func (p *ModemPool) emitPoolEvent(event PoolEvent) {
	p.poolEventsMu.RLock()
	handlers := append([]PoolEventHandler(nil), p.poolEventHandlers...)
	p.poolEventsMu.RUnlock()
	for _, h := range handlers {
		h(event)
	}
}

// ============================================================================
// Selector Interface - 选择器接口
// ============================================================================

// Selector defines the interface for modem selection strategies
// Selector 定义模组选择策略的接口
type Selector interface {
	Select(modems []*Manager) *Manager
}

// ============================================================================
// RoundRobinSelector - 轮询选择器
// ============================================================================

// RoundRobinSelector selects modems in round-robin order / RoundRobinSelector 按轮询顺序选择模组
type RoundRobinSelector struct {
	counter uint64
}

func (s *RoundRobinSelector) Select(modems []*Manager) *Manager {
	if len(modems) == 0 {
		return nil
	}
	idx := atomic.AddUint64(&s.counter, 1) % uint64(len(modems))
	return modems[idx]
}

// ============================================================================
// RandomSelector - 随机选择器
// ============================================================================

// RandomSelector selects a random modem / RandomSelector 随机选择模组
type RandomSelector struct{}

func (s *RandomSelector) Select(modems []*Manager) *Manager {
	if len(modems) == 0 {
		return nil
	}
	return modems[rand.Intn(len(modems))]
}

// ============================================================================
// SignalStrengthSelector - 信号强度选择器
// ============================================================================

// SignalStrengthSelector selects the modem with best signal / SignalStrengthSelector 选择信号最强的模组
type SignalStrengthSelector struct{}

func isValidCellularRSSI(rssi int8) bool {
	// Valid cellular RSSI in dBm is strictly negative (typically -125..-30 dBm).
	// Reject 0 (unmeasured default) and positive values like +125 (0x7D QMI unknown sentinel).
	return rssi < 0 && rssi >= -125
}

func (s *SignalStrengthSelector) Select(modems []*Manager) *Manager {
	if len(modems) == 0 {
		return nil
	}

	var best *Manager
	bestRSSI := int8(-128)

	for _, mgr := range modems {
		rssi, attempted, err := queryManagerSignalRSSI(context.Background(), mgr)
		if attempted && err == nil && isValidCellularRSSI(rssi) && (best == nil || rssi > bestRSSI) {
			bestRSSI = rssi
			best = mgr
		}
	}

	if best == nil {
		// Fallback to first modem if signal check failed / 如果信号检查失败，回退到第一个模组
		return modems[0]
	}
	return best
}

// ============================================================================
// LeastUsedSelector - 最少使用选择器
// ============================================================================

// LeastUsedSelector selects the least recently used modem / LeastUsedSelector 选择最近最少使用的模组
type LeastUsedSelector struct {
	mu       sync.Mutex
	lastUsed map[*Manager]time.Time
}

func NewLeastUsedSelector() *LeastUsedSelector {
	return &LeastUsedSelector{
		lastUsed: make(map[*Manager]time.Time),
	}
}

func (s *LeastUsedSelector) Select(modems []*Manager) *Manager {
	if len(modems) == 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastUsed == nil {
		s.lastUsed = make(map[*Manager]time.Time, len(modems))
	}

	// Prune stale *Manager keys from hot-unplugged modems so map does not leak.
	active := make(map[*Manager]struct{}, len(modems))
	for _, mgr := range modems {
		active[mgr] = struct{}{}
	}
	for mgr := range s.lastUsed {
		if _, ok := active[mgr]; !ok {
			delete(s.lastUsed, mgr)
		}
	}

	var leastUsed *Manager
	var leastTime time.Time

	for i, mgr := range modems {
		t, exists := s.lastUsed[mgr]
		if !exists {
			// Never used, select this one / 从未使用过，选择这个
			s.lastUsed[mgr] = time.Now()
			return mgr
		}
		if i == 0 || t.Before(leastTime) {
			leastTime = t
			leastUsed = mgr
		}
	}

	s.lastUsed[leastUsed] = time.Now()
	return leastUsed
}
