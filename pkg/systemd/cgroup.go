package systemd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/nomad/client/lib/cpustats"
	"github.com/hashicorp/nomad/plugins/drivers"

	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/logx"
	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/logx/semconv"
)

const (
	// cgroupV2Root is the unified cgroup hierarchy, the default for
	// Manager.cgroupRoot.
	cgroupV2Root = "/sys/fs/cgroup"

	// cgroupRetryInterval is how long a cached empty ControlGroup is kept
	// before being re-read.
	cgroupRetryInterval = 5 * time.Second
)

// ResourceStats holds one sample of a unit's CPU and memory usage.
//
// Both fields are non-nil in every value this package produces, so callers need
// no nil checks. Each carries a Measured list naming the metrics actually
// obtained; a metric absent from that list is zero because it could not be read,
// not because its value is zero.
type ResourceStats struct {
	// CPUStats holds CPU usage, including a percentage derived from the change
	// since the previous sample taken for the same unit.
	CPUStats *drivers.CpuStats

	// MemoryStats holds memory usage: Usage is total charged memory, RSS the
	// anonymous portion, and Cache the page-cache portion.
	MemoryStats *drivers.MemoryStats
}

// EmptyResourceStats returns a sample with non-nil, zeroed CPU and memory stats
// and an empty Measured list on each.
//
// It lets callers that cannot obtain real numbers - no cgroup path yet, DBus
// unavailable, a cgroup v1 host - return a usable value instead of nil.
func EmptyResourceStats() *ResourceStats {
	return &ResourceStats{
		CPUStats:    &drivers.CpuStats{},
		MemoryStats: &drivers.MemoryStats{},
	}
}

// CgroupV2Available reports whether the host uses the unified (v2) cgroup
// hierarchy that [Manager.ResourceStats] requires.
//
// When it reports false, units still start and stop normally but resource
// statistics read as zeros. Callers should surface that up front rather than
// leave it to be discovered as unexplained empty statistics.
func (sm *Manager) CgroupV2Available() bool {
	_, err := os.Stat(filepath.Join(sm.cgroupRoot, "cgroup.controllers"))

	return err == nil
}

// ResourceStats samples the unit's current CPU and memory usage.
//
// It returns an error wrapping [ErrUnitNotRegistered], and only that error, if
// the unit is not registered. A sample that could not
// be read - the unit has no cgroup yet, or the host is not on cgroup v2 - comes
// back zeroed with empty Measured lists rather than as an error, and both the
// result and its metrics are non-nil in that case. Consult those lists to tell
// "zero" from "not measured".
//
// CPU percentage is derived from the change since the previous call for the same
// unit, so the first sample after registration reports no percentage.
func (sm *Manager) ResourceStats(ctx context.Context, unit string) (*ResourceStats, error) {
	controlGroup, cachedAt, registered := sm.cachedControlGroup(unit)
	if !registered {
		return nil, fmt.Errorf("resource stats for %s: %w", unit, ErrUnitNotRegistered)
	}

	logger := sm.logger.With(semconv.Unit(unit))

	// Re-read if never read, or if an empty reading has gone stale.
	if cachedAt.IsZero() || (controlGroup == "" && time.Since(cachedAt) > cgroupRetryInterval) {
		logger.Debug("refreshing cached unit properties")
		sm.cacheUnitProperties(ctx, unit)

		controlGroup, _, registered = sm.cachedControlGroup(unit)
		if !registered {
			return nil, fmt.Errorf("resource stats for %s: %w", unit, ErrUnitNotRegistered)
		}
	}

	if controlGroup == "" {
		logger.Warn("cgroup path is empty for unit")

		return EmptyResourceStats(), nil
	}

	stats := sm.getCgroupV2Stats(unit, controlGroup)
	if stats == nil {
		logger.Warn("read cgroup v2 stats", semconv.UnitCgroup(controlGroup))

		return EmptyResourceStats(), nil
	}

	return stats, nil
}

// cachedControlGroup returns unit's cached cgroup path and when it was read. The
// final result reports whether the unit is registered at all; a zero cachedAt
// means it is registered but nothing has been read yet.
func (sm *Manager) cachedControlGroup(unit string) (controlGroup string, cachedAt time.Time, registered bool) {
	sm.unitsLock.RLock()
	defer sm.unitsLock.RUnlock()

	st := sm.unitLocked(unit)
	if st == nil {
		return "", time.Time{}, false
	}

	return st.controlGroup, st.cachedAt, true
}

// getCgroupV2Stats reads a unit's cgroup accounting files. It returns nil if
// nothing at all could be measured; a partial read is still reported, with
// Measured naming what was obtained.
func (sm *Manager) getCgroupV2Stats(unit, cgroupPath string) *ResourceStats {
	logger := sm.logger.With(semconv.Unit(unit))
	fullPath := filepath.Join(sm.cgroupRoot, cgroupPath)

	var (
		cpuStats    drivers.CpuStats
		memoryStats drivers.MemoryStats
	)

	memoryStats.Measured = make([]string, 0, 3)

	// memory.current is total charged memory, including page cache and kernel
	// memory, so it is Usage. RSS and Cache come from memory.stat below.
	memCurrentPath := filepath.Join(fullPath, "memory.current")
	if memCurrent, err := readCgroupV2File(memCurrentPath); err == nil {
		memoryStats.Usage = memCurrent
		memoryStats.Measured = append(memoryStats.Measured, "Usage")
	} else {
		logger.Warn("read memory.current", semconv.FilePath(memCurrentPath), logx.Err(err))
	}

	memStatPath := filepath.Join(fullPath, "memory.stat")
	if data, err := os.ReadFile(memStatPath); err == nil {
		var haveAnon, haveFile bool

		scanCgroupKeyValue(data, func(key string, value uint64) bool {
			switch key {
			case "anon":
				memoryStats.RSS = value
				memoryStats.Measured = append(memoryStats.Measured, "RSS")
				haveAnon = true
			case "file":
				memoryStats.Cache = value
				memoryStats.Measured = append(memoryStats.Measured, "Cache")
				haveFile = true
			}

			// No further key is consumed, so stop once both are in hand.
			// Tracked with flags, not values, since 0 is a real reading.
			return !haveAnon || !haveFile
		})
	} else {
		logger.Warn("read memory.stat", semconv.FilePath(memStatPath), logx.Err(err))
	}

	cpuStatPath := filepath.Join(fullPath, "cpu.stat")
	if data, err := os.ReadFile(cpuStatPath); err == nil {
		scanCgroupKeyValue(data, func(key string, value uint64) bool {
			if key != "usage_usec" {
				return true
			}

			sm.calculateCPUPercent(unit, value*1000, &cpuStats)

			return false
		})
	} else {
		logger.Warn("read cpu.stat", semconv.FilePath(cpuStatPath), logx.Err(err))
	}

	if len(memoryStats.Measured) > 0 || len(cpuStats.Measured) > 0 {
		return &ResourceStats{
			CPUStats:    &cpuStats,
			MemoryStats: &memoryStats,
		}
	}

	logger.Warn("no stats measured from cgroup v2")

	return nil
}

// calculateCPUPercent turns cumulative CPU nanoseconds into the percentage and
// tick count Nomad reports, using the unit's previous sample.
func (sm *Manager) calculateCPUPercent(unit string, cpuUsageNsec uint64, cpuStats *drivers.CpuStats) {
	// The write lock, not the read lock: the tracker itself is mutated here.
	sm.unitsLock.Lock()
	defer sm.unitsLock.Unlock()

	st := sm.unitLocked(unit)
	if st == nil {
		return
	}

	if st.cpu == nil {
		st.cpu = cpustats.New(sm.compute)
	}

	percent := st.cpu.Percent(float64(cpuUsageNsec))
	ticks := st.cpu.TicksConsumed(percent)

	cpuStats.Percent = percent
	cpuStats.TotalTicks = ticks
	cpuStats.Measured = []string{"Percent", "Total Ticks"}
}

// scanCgroupKeyValue walks the "key value" lines of a cgroup stat file, calling
// fn for each line that parses and stopping early when fn returns false.
//
// Malformed lines are skipped rather than fatal: these files gain keys across
// kernel versions, and one odd line must not cost the remaining metrics.
func scanCgroupKeyValue(data []byte, fn func(key string, value uint64) bool) {
	for len(data) > 0 {
		line := data
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			data = nil
		}

		key, rest, ok := bytes.Cut(line, []byte{' '})
		if !ok {
			continue
		}

		value, err := strconv.ParseUint(string(bytes.TrimSpace(rest)), 10, 64)
		if err != nil {
			continue
		}

		if !fn(string(key), value) {
			return
		}
	}
}

// readCgroupV2File reads a cgroup file holding one unsigned decimal value. It
// returns an error for anything else, including the "max" sentinel.
func readCgroupV2File(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}

	value, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", path, err)
	}

	return value, nil
}
