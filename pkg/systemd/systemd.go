package systemd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
	"github.com/coreos/go-systemd/v22/sdjournal"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/lib/cpustats"
	"github.com/hashicorp/nomad/plugins/drivers"
)

// Manager handles all systemd interactions via DBus
// It owns the DBus connection and communicates via channels
type Manager struct {
	logger hclog.Logger
	Conn   *dbus.Conn

	// Command channel receives requests from Driver
	CmdCh chan Command

	// Event channels send events to TaskHandlers (keyed by unit name)
	eventChans  map[string]chan Message
	eventChLock sync.RWMutex

	// Unit subscriptions for monitoring - REMOVED to eliminate ListUnits() overhead
	// subscriptions map[string]*dbus.SubscriptionSet
	// subLock       sync.RWMutex

	// CPU stats tracking per unit
	cpuTracking  map[string]*cpuTracker
	cpuTrackLock sync.RWMutex

	// Cached unit properties (fetched once, never change during unit lifetime)
	unitProperties     map[string]*unitPropertyCache
	unitPropertiesLock sync.RWMutex

	compute cpustats.Compute

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// cpuTracker tracks CPU usage for percentage calculations
type cpuTracker struct {
	tracker *cpustats.Tracker
}

// unitPropertyCache stores static properties that don't change during unit lifetime
type unitPropertyCache struct {
	ControlGroup string                 // cgroup path for reading stats
	Properties   map[string]interface{} // all unit properties
	cachedAt     time.Time
}

const (
	// statsCollectionInterval is how often we collect and push stats to all units
	statsCollectionInterval = 1 * time.Second
)

// NewManager creates a new SystemdManager
func NewManager(ctx context.Context, compute cpustats.Compute, logger hclog.Logger) (*Manager, error) {
	conn, err := dbus.NewSystemConnectionContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect to systemd: %w", err)
	}

	managerCtx, cancel := context.WithCancel(ctx)
	sm := &Manager{
		Conn:           conn,
		logger:         logger.Named("systemd_manager"),
		CmdCh:          make(chan Command, 10),
		eventChans:     make(map[string]chan Message),
		cpuTracking:    make(map[string]*cpuTracker),
		unitProperties: make(map[string]*unitPropertyCache),
		compute:        compute,
		ctx:            managerCtx,
		cancel:         cancel,
	}

	return sm, nil
}

// Start begins processing commands and monitoring units
func (sm *Manager) Start() {
	sm.wg.Add(2)
	go sm.commandLoop()
	go sm.statsCollectionLoop()
}

// Stop gracefully shuts down the manager
func (sm *Manager) Stop() {
	sm.logger.Info("stopping systemd manager")
	sm.cancel()

	sm.wg.Wait()
	sm.eventChLock.Lock()
	for unit, ch := range sm.eventChans {
		close(ch)
		delete(sm.eventChans, unit)
	}

	sm.eventChLock.Unlock()
	if sm.Conn != nil {
		sm.Conn.Close()
	}

	sm.logger.Info("systemd manager stopped")
}

// CommandChannel returns the command channel for sending requests
func (sm *Manager) CommandChannel() chan<- Command {
	return sm.CmdCh
}

// RegisterUnit registers a unit for stats collection and returns its event channel
func (sm *Manager) RegisterUnit(unit string) <-chan Message {
	sm.eventChLock.Lock()
	defer sm.eventChLock.Unlock()

	eventCh := make(chan Message, 10)
	sm.eventChans[unit] = eventCh

	// Fetch and cache static properties once
	sm.cacheUnitProperties(unit)

	sm.logger.Debug("registered unit", "unit", unit)

	return eventCh
}

// UnregisterUnit stops monitoring a unit
func (sm *Manager) UnregisterUnit(unit string) {
	// Close the event channel
	sm.eventChLock.Lock()
	if ch, ok := sm.eventChans[unit]; ok {
		close(ch)
		delete(sm.eventChans, unit)
	}
	sm.eventChLock.Unlock()

	// Clean up caches
	sm.cpuTrackLock.Lock()
	delete(sm.cpuTracking, unit)
	sm.cpuTrackLock.Unlock()

	sm.unitPropertiesLock.Lock()
	delete(sm.unitProperties, unit)
	sm.unitPropertiesLock.Unlock()

	sm.logger.Debug("unregistered unit", "unit", unit)
}

// commandLoop processes incoming commands
func (sm *Manager) commandLoop() {
	defer sm.wg.Done()

	for {
		select {
		case <-sm.ctx.Done():
			return
		case cmd := <-sm.CmdCh:
			sm.handleCommand(cmd)
		}
	}
}

// handleCommand processes a single command
func (sm *Manager) handleCommand(cmd Command) {
	sm.logger.Debug("handling command", "type", cmd.Type, "unit", cmd.Unit)

	resp := Message{
		Unit: cmd.Unit,
	}

	switch cmd.Type {
	case CmdStart:
		resp.Error = sm.startUnit(cmd.Unit)

	case CmdStop:
		resp.Error = sm.stopUnit(cmd.Unit)

	case CmdGetStatus:
		state, err := sm.getUnitState(cmd.Unit)
		resp.Data = state
		resp.Error = err

	case CmdGetProperties:
		props, err := sm.getUnitProperties(cmd.Unit)
		resp.Data = props
		resp.Error = err

	default:
		resp.Error = fmt.Errorf("unknown command type: %d", cmd.Type)
	}

	select {
	case cmd.RespCh <- resp:
	case <-sm.ctx.Done():
	}
}

// statsCollectionLoop periodically collects stats for all registered units
// and pushes them as events to TaskHandlers
func (sm *Manager) statsCollectionLoop() {
	defer sm.wg.Done()

	ticker := time.NewTicker(statsCollectionInterval)
	defer ticker.Stop()

	sm.logger.Info("stats collection loop started", "interval", statsCollectionInterval.String())

	for {
		select {
		case <-sm.ctx.Done():
			sm.logger.Info("stats collection loop stopped")
			return

		case <-ticker.C:
			sm.collectAndPushStats()
		}
	}
}

// collectAndPushStats collects stats for all registered units and pushes events
func (sm *Manager) collectAndPushStats() {
	sm.eventChLock.RLock()
	units := make([]string, 0, len(sm.eventChans))
	for unit := range sm.eventChans {
		units = append(units, unit)
	}
	sm.eventChLock.RUnlock()

	for _, unit := range units {
		stats, err := sm.getResourceStats(unit)
		if err != nil {
			sm.logger.Debug("collect stats for unit", "unit", unit, "error", err)
			sm.pushStatsEvent(unit, nil, err)
			continue
		}

		sm.pushStatsEvent(unit, stats, nil)
	}
}

// pushStatsEvent sends a stats update event to the unit's TaskHandler
func (sm *Manager) pushStatsEvent(unit string, stats *ResourceStats, err error) {
	sm.eventChLock.RLock()
	eventCh, ok := sm.eventChans[unit]
	sm.eventChLock.RUnlock()

	if !ok {
		// Unit was unregistered, skip
		return
	}

	event := Message{
		Unit:      unit,
		Type:      EventStatsUpdate,
		Data:      stats,
		Error:     err,
		Timestamp: time.Now(),
	}

	defer func() {
		if r := recover(); r != nil {
			sm.logger.Warn("push stats event (channel likely closed)", "unit", unit)
		}
	}()

	select {
	case eventCh <- event:
		// Stats pushed successfully
	default:
		sm.logger.Warn("event channel full, dropping stats update", "unit", unit)
	}
}

// startUnit starts a systemd unit
func (sm *Manager) startUnit(unit string) error {
	sm.logger.Info("starting unit", "unit", unit)
	_, err := sm.Conn.StartUnitContext(sm.ctx, unit, "replace", nil)
	return err
}

// stopUnit stops a systemd unit
func (sm *Manager) stopUnit(unit string) error {
	sm.logger.Info("stopping unit", "unit", unit)
	_, err := sm.Conn.StopUnitContext(sm.ctx, unit, "replace", nil)
	return err
}

// getUnitState retrieves the current state of a unit as a SystemdUnitState
func (sm *Manager) getUnitState(unit string) (UnitState, error) {
	properties, err := sm.Conn.GetUnitPropertiesContext(sm.ctx, unit)
	if err != nil {
		return "", fmt.Errorf("get unit properties: %w", err)
	}

	activeStateStr, ok := properties["ActiveState"].(string)
	if !ok {
		return "", fmt.Errorf("ActiveState property not found")
	}

	return ParseSystemdUnitState(activeStateStr), nil
}

// getUnitProperties retrieves all properties of a unit from cache
func (sm *Manager) getUnitProperties(unit string) (map[string]interface{}, error) {
	sm.unitPropertiesLock.RLock()
	cache, ok := sm.unitProperties[unit]
	sm.unitPropertiesLock.RUnlock()

	if ok && cache.Properties != nil {
		sm.logger.Debug("returning cached unit properties", "unit", unit, "count", len(cache.Properties))
		return cache.Properties, nil
	}

	// Not cached yet, fetch and cache now
	sm.logger.Warn("unit properties not cached, fetching via dbus", "unit", unit)
	sm.cacheUnitProperties(unit)

	sm.unitPropertiesLock.RLock()
	cache, ok = sm.unitProperties[unit]
	sm.unitPropertiesLock.RUnlock()

	if !ok || cache.Properties == nil {
		return nil, fmt.Errorf("failed to fetch unit properties")
	}

	return cache.Properties, nil
}

// cacheUnitProperties fetches and caches static unit properties that don't change
func (sm *Manager) cacheUnitProperties(unit string) {
	sm.logger.Debug("caching unit properties", "unit", unit)

	// Fetch properties once
	properties, err := sm.Conn.GetUnitPropertiesContext(sm.ctx, unit)
	if err != nil {
		sm.logger.Warn("failed to cache unit properties", "unit", unit, "error", err)
		return
	}

	// Also fetch service-specific properties
	serviceProps, err := sm.Conn.GetUnitTypePropertiesContext(sm.ctx, unit, "Service")
	if err == nil {
		for k, v := range serviceProps {
			if _, exists := properties[k]; !exists {
				properties[k] = v
			}
		}
	}

	// Extract the cgroup path
	controlGroup, ok := properties["ControlGroup"].(string)
	if !ok || controlGroup == "" {
		sm.logger.Warn("ControlGroup property missing", "unit", unit)
		controlGroup = "" // Empty but still cache to avoid repeated attempts
	}

	cache := &unitPropertyCache{
		ControlGroup: controlGroup,
		Properties:   properties, // Cache all properties
		cachedAt:     time.Now(),
	}

	sm.unitPropertiesLock.Lock()
	sm.unitProperties[unit] = cache
	sm.unitPropertiesLock.Unlock()

	sm.logger.Info("cached unit properties", "unit", unit, "cgroup", controlGroup, "properties_count", len(properties))
}

// getResourceStats retrieves CPU and memory statistics using cached properties
// Stats are read directly from cgroup v2 filesystem - NO DBUS CALLS!
func (sm *Manager) getResourceStats(unit string) (*ResourceStats, error) {
	// Get cached cgroup path (fetched once at unit registration)
	sm.unitPropertiesLock.RLock()
	cache, hasCached := sm.unitProperties[unit]
	sm.unitPropertiesLock.RUnlock()

	// If no cache exists, fetch it now (shouldn't happen in normal operation)
	if !hasCached {
		sm.logger.Warn("unit properties not cached, fetching on demand", "unit", unit)
		sm.cacheUnitProperties(unit)
		sm.unitPropertiesLock.RLock()
		cache, hasCached = sm.unitProperties[unit]
		sm.unitPropertiesLock.RUnlock()

		if !hasCached {
			sm.logger.Error("failed to cache unit properties", "unit", unit)
			return &ResourceStats{
				CPUStats:    &drivers.CpuStats{},
				MemoryStats: &drivers.MemoryStats{},
			}, nil
		}
	}

	// Read stats directly from cgroup v2 filesystem - NO DBUS!
	controlGroup := cache.ControlGroup
	if controlGroup == "" {
		sm.logger.Warn("cgroup path is empty for unit", "unit", unit)
		return &ResourceStats{
			CPUStats:    &drivers.CpuStats{},
			MemoryStats: &drivers.MemoryStats{},
		}, nil
	}

	stats := sm.getCgroupV2Stats(unit, controlGroup)
	if stats == nil {
		sm.logger.Warn("failed to read cgroup v2 stats", "unit", unit, "cgroup", controlGroup)
		return &ResourceStats{
			CPUStats:    &drivers.CpuStats{},
			MemoryStats: &drivers.MemoryStats{},
		}, nil
	}

	return stats, nil
}

// getCgroupV2Stats reads stats directly from cgroup v2 filesystem
func (sm *Manager) getCgroupV2Stats(unit string, cgroupPath string) *ResourceStats {
	cgroupV2Root := "/sys/fs/cgroup"
	fullPath := filepath.Join(cgroupV2Root, cgroupPath)
	if _, err := os.Stat(fullPath); err != nil {
		sm.logger.Warn("cgroup path not found", "unit", unit, "path", fullPath, "error", err)
		return nil
	}

	var (
		cpuStats    drivers.CpuStats
		memoryStats drivers.MemoryStats
	)

	memPath := filepath.Join(fullPath, "memory.current")
	if memCurrent, err := readCgroupV2File(memPath); err == nil {
		memoryStats.RSS = memCurrent
		memoryStats.Usage = memCurrent
		memoryStats.Measured = []string{"RSS", "Usage"}
	} else {
		sm.logger.Warn("read memory.current", "unit", unit, "path", memPath, "error", err)
	}

	cpuStatPath := filepath.Join(fullPath, "cpu.stat")
	if data, err := os.ReadFile(cpuStatPath); err == nil {
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == "usage_usec" {
				if cpuUsageUsec, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
					cpuUsageNsec := cpuUsageUsec * 1000
					sm.calculateCPUPercent(unit, cpuUsageNsec, &cpuStats)
				}

				break
			}
		}
	} else {
		sm.logger.Warn("read cpu.stat", "unit", unit, "path", cpuStatPath, "error", err)
	}

	if len(memoryStats.Measured) > 0 || len(cpuStats.Measured) > 0 {
		return &ResourceStats{
			CPUStats:    &cpuStats,
			MemoryStats: &memoryStats,
		}
	}

	sm.logger.Warn("no stats measured from cgroup v2", "unit", unit)
	return nil
}

// calculateCPUPercent calculates CPU percentage from cumulative nanoseconds
func (sm *Manager) calculateCPUPercent(unit string, cpuUsageNsec uint64, cpuStats *drivers.CpuStats) {
	sm.cpuTrackLock.Lock()
	defer sm.cpuTrackLock.Unlock()

	tracker, ok := sm.cpuTracking[unit]
	if !ok {
		tracker = &cpuTracker{
			tracker: cpustats.New(sm.compute),
		}
		sm.cpuTracking[unit] = tracker
	}

	// Convert nanoseconds to float for Nomad's tracker (expects nanoseconds as float)
	cpuTimeFloat := float64(cpuUsageNsec)

	// Percent() calculates the delta and returns CPU percent
	percent := tracker.tracker.Percent(cpuTimeFloat)

	// TicksConsumed() converts percent to ticks based on system compute capacity
	ticks := tracker.tracker.TicksConsumed(percent)

	cpuStats.Percent = percent
	cpuStats.TotalTicks = ticks
	cpuStats.Measured = []string{"Percent", "Total Ticks"}
}

// StreamLogs streams journal logs for a unit to the provided channels
func (sm *Manager) StreamLogs(unit string, logCh chan<- *LogEntry) error {
	sm.logger.Debug("starting log streamer", "unit", unit)

	journal, err := sdjournal.NewJournal()
	if err != nil {
		return fmt.Errorf("open journal: %w", err)
	}

	if err := journal.AddMatch("_SYSTEMD_UNIT=" + unit); err != nil {
		journal.Close()
		return fmt.Errorf("add journal match: %w", err)
	}

	// Start from current time
	if err := journal.SeekRealtimeUsec(uint64(time.Now().UnixMicro())); err != nil {
		sm.logger.Warn("seek journal failed, starting from tail", "error", err)
		journal.SeekTail()
	}

	sm.wg.Add(1)
	go func() {
		defer sm.wg.Done()
		defer journal.Close()

		for {
			select {
			case <-sm.ctx.Done():
				return
			default:
				n, err := journal.Next()
				if err != nil {
					sm.logger.Warn("journal read error", "error", err)
					time.Sleep(500 * time.Millisecond)
					continue
				}

				if n == 0 {
					time.Sleep(500 * time.Millisecond)
					continue
				}

				entry, err := journal.GetEntry()
				if err != nil {
					sm.logger.Error("get journal entry", "error", err)
					continue
				}

				message, ok := entry.Fields["MESSAGE"]
				if !ok {
					continue
				}

				logEntry := &LogEntry{
					Message:          message,
					Priority:         entry.Fields["PRIORITY"],
					SyslogIdentifier: entry.Fields["SYSLOG_IDENTIFIER"],
					Timestamp:        time.Unix(0, int64(entry.RealtimeTimestamp)*1000),
				}

				select {
				case logCh <- logEntry:
				case <-sm.ctx.Done():
					return
				}
			}
		}
	}()

	return nil
}

// readCgroupV2File reads a single-value file from cgroup v2 filesystem
func readCgroupV2File(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}

	value, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse %s: %w", path, err)
	}

	return value, nil
}
