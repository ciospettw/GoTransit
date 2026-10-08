// Package memrelease continuously balances resident memory against GC cost.
// All decisions scale from the live heap, allocation velocity and the memory
// ceiling visible to the process; callers do not configure network sizes.
package memrelease

import (
	"context"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	interval             = 30 * time.Second
	minCooldown          = time.Minute
	maxCooldown          = 30 * time.Minute
	pressureCooldown     = 30 * time.Second
	targetReleaseCPU     = 0.001 // at most 0.1% wall time outside pressure
	reuseHorizon         = 2 * time.Second
	minUsefulRelease     = 1 << 20
	maxRelativeThreshold = 512 << 20
	minAutoGOGC          = 10
	maxAutoGOGC          = 200
)

var busy atomic.Int64
var once sync.Once
var cacheBudget atomic.Uint64
var report struct {
	sync.Mutex
	Status
}

func init() { cacheBudget.Store(math.MaxUint64) }

type Status struct {
	Releases         uint64        `json:"releases"`
	LastAt           time.Time     `json:"last_at,omitempty"`
	LastDuration     time.Duration `json:"last_duration_ns"`
	LastIdleBefore   uint64        `json:"last_idle_before_bytes"`
	LastIdleAfter    uint64        `json:"last_idle_after_bytes"`
	LastPause        uint64        `json:"last_gc_pause_ns"`
	LastCacheDropped uint64        `json:"last_cache_dropped_bytes"`
	PendingGC        bool          `json:"pending_gc"`
	Busy             int64         `json:"busy"`
	AllocationRate   uint64        `json:"allocation_rate_bytes_per_second"`
	MinimumRelease   uint64        `json:"minimum_release_bytes"`
	Cooldown         time.Duration `json:"cooldown_ns"`
	UnderPressure    bool          `json:"under_pressure"`
	MemoryCurrent    uint64        `json:"memory_current_bytes,omitempty"`
	MemoryLimit      uint64        `json:"memory_limit_bytes,omitempty"`
	GCPercent        int           `json:"gogc_percent"`
	AutomaticGC      bool          `json:"automatic_gogc"`
	RecentGCCPU      float64       `json:"recent_gc_cpu_fraction"`
	IdleCacheBudget  uint64        `json:"idle_cache_budget_bytes"`
}

func Snapshot() Status { report.Lock(); defer report.Unlock(); return report.Status }

// Begin marks a phase whose working set must not be collected underneath it.
func Begin() func() { busy.Add(1); return func() { busy.Add(-1) } }

// Start launches one process-wide controller. trim releases optional engine
// caches before scavenging and returns the number of reachable bytes dropped.
func Start(ctx context.Context, trim func(pressure bool) uint64) {
	once.Do(func() { go run(ctx, trim) })
}

// IdleCacheEntries returns how many equally sized idle query states fit in the
// current automatic cache budget. One hot state is retained when possible;
// under memory pressure the controller publishes a zero budget.
func IdleCacheEntries(bytesPerEntry uint64) int {
	if bytesPerEntry == 0 {
		return 1
	}
	budget := cacheBudget.Load()
	if budget == 0 {
		return 0
	}
	procs := runtime.GOMAXPROCS(0)
	n := budget / bytesPerEntry
	if n >= uint64(procs) {
		return procs
	}
	if n == 0 {
		return 1
	}
	return int(n)
}

type observation struct {
	at       time.Time
	alloc    uint64
	gc       uint32
	gcCPU    float64
	totalCPU float64
}

type policyDecision struct {
	release    bool
	rate       uint64
	minimum    uint64
	cooldown   time.Duration
	pressure   bool
	memCurrent uint64
	memLimit   uint64
	gcShare    float64
}

func decide(previous observation, now time.Time, stats runtime.MemStats, last time.Time, lastDuration time.Duration, pending bool) policyDecision {
	current, limit, pressure := memoryState(stats)
	d := policyDecision{pressure: pressure, memCurrent: current, memLimit: limit}
	elapsed := now.Sub(previous.at)
	if elapsed > 0 && stats.TotalAlloc >= previous.alloc {
		d.rate = uint64(float64(stats.TotalAlloc-previous.alloc) / elapsed.Seconds())
	}
	if cpu := statsCPU(); cpu.total > previous.totalCPU {
		d.gcShare = (cpu.gc - previous.gcCPU) / (cpu.total - previous.totalCPU)
		if d.gcShare < 0 || math.IsNaN(d.gcShare) || math.IsInf(d.gcShare, 0) {
			d.gcShare = 0
		}
	}
	d.cooldown = adaptiveCooldown(lastDuration, pressure)
	d.minimum = releaseThreshold(stats.HeapAlloc, d.rate, pressure)
	if busy.Load() != 0 || !pending || previous.at.IsZero() || elapsed < interval || now.Sub(last) < d.cooldown || stats.HeapIdle < stats.HeapReleased {
		return d
	}
	idle := stats.HeapIdle - stats.HeapReleased
	d.release = idle >= d.minimum
	return d
}

func releaseThreshold(live, rate uint64, pressure bool) uint64 {
	if pressure {
		return minUsefulRelease
	}
	relative := live / 32
	if relative < minUsefulRelease {
		relative = minUsefulRelease
	}
	if relative > maxRelativeThreshold {
		relative = maxRelativeThreshold
	}
	reuse := rate * uint64(reuseHorizon/time.Second)
	if reuse > relative {
		return reuse
	}
	return relative
}

func adaptiveCooldown(lastDuration time.Duration, pressure bool) time.Duration {
	if pressure {
		return pressureCooldown
	}
	d := minCooldown
	if lastDuration > 0 {
		d = time.Duration(float64(lastDuration) / targetReleaseCPU)
	}
	if d < minCooldown {
		return minCooldown
	}
	if d > maxCooldown {
		return maxCooldown
	}
	return d
}

func tuneGOGC(current int, gcShare float64, pressure bool) int {
	if current < minAutoGOGC || current > maxAutoGOGC {
		current = 100
	}
	next := current
	switch {
	case pressure:
		next = current * 3 / 4
	case gcShare > 0 && gcShare < 0.005:
		next = current * 85 / 100
	case gcShare > 0.02:
		next = current * 5 / 4
	}
	if next < minAutoGOGC {
		next = minAutoGOGC
	}
	if next > maxAutoGOGC {
		next = maxAutoGOGC
	}
	return next
}

func cacheBudgetFor(live, current, limit uint64, pressure bool) uint64 {
	if pressure {
		return 0
	}
	budget := live / 32
	if limit > current {
		headroom := (limit - current) / 8
		if headroom < budget {
			budget = headroom
		}
	}
	return budget
}

func run(ctx context.Context, trim func(bool) uint64) {
	autoGC := automaticGC()
	configureMemoryLimit()
	var initial runtime.MemStats
	runtime.ReadMemStats(&initial)
	current, limit, pressure := memoryState(initial)
	cacheBudget.Store(cacheBudgetFor(initial.HeapAlloc, current, limit, pressure))
	cpu := statsCPU()
	previous := observation{time.Now(), initial.TotalAlloc, initial.NumGC, cpu.gc, cpu.total}
	pending := false
	last := time.Now()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			var before runtime.MemStats
			runtime.ReadMemStats(&before)
			if before.NumGC != previous.gc {
				pending = true
			}
			report.Lock()
			lastDuration := report.LastDuration
			report.Unlock()
			d := decide(previous, now, before, last, lastDuration, pending)
			budget := cacheBudgetFor(before.HeapAlloc, d.memCurrent, d.memLimit, d.pressure)
			cacheBudget.Store(budget)

			gcPercent := readGOGC()
			if autoGC && busy.Load() == 0 && before.NumGC != previous.gc {
				if next := tuneGOGC(gcPercent, d.gcShare, d.pressure); next != gcPercent {
					debug.SetGCPercent(next)
					gcPercent = next
				}
			}

			var dropped uint64
			if d.release {
				if trim != nil {
					dropped = trim(d.pressure)
				}
				start := time.Now()
				debug.FreeOSMemory()
				var after runtime.MemStats
				runtime.ReadMemStats(&after)
				report.Lock()
				report.Releases++
				report.LastAt = time.Now().UTC()
				report.LastDuration = time.Since(start)
				report.LastIdleBefore = before.HeapIdle - before.HeapReleased
				report.LastIdleAfter = after.HeapIdle - after.HeapReleased
				report.LastPause = after.PauseTotalNs - before.PauseTotalNs
				report.LastCacheDropped = dropped
				report.Unlock()
				last = now
				pending = false
				before = after
			}

			cpu = statsCPU()
			previous = observation{now, before.TotalAlloc, before.NumGC, cpu.gc, cpu.total}
			report.Lock()
			report.PendingGC = pending
			report.Busy = busy.Load()
			report.AllocationRate = d.rate
			report.MinimumRelease = d.minimum
			report.Cooldown = d.cooldown
			report.UnderPressure = d.pressure
			report.MemoryCurrent = d.memCurrent
			report.MemoryLimit = d.memLimit
			report.GCPercent = gcPercent
			report.AutomaticGC = autoGC
			report.RecentGCCPU = d.gcShare
			report.IdleCacheBudget = budget
			report.Unlock()
		}
	}
}

func automaticGC() bool {
	return os.Getenv("GOGC") == "" && readGOGC() == 100
}

type cpuStats struct{ gc, total float64 }

func statsCPU() cpuStats {
	samples := []metrics.Sample{{Name: "/cpu/classes/gc/total:cpu-seconds"}, {Name: "/cpu/classes/total:cpu-seconds"}}
	metrics.Read(samples)
	return cpuStats{gc: samples[0].Value.Float64(), total: samples[1].Value.Float64()}
}

func readGOGC() int {
	s := []metrics.Sample{{Name: "/gc/gogc:percent"}}
	metrics.Read(s)
	v := s[0].Value.Uint64()
	if v > math.MaxInt32 {
		return -1 // GOGC=off
	}
	return int(v)
}

func configureMemoryLimit() {
	if os.Getenv("GOMEMLIMIT") != "" || debug.SetMemoryLimit(-1) < math.MaxInt64 {
		return
	}
	_, limit, ok := cgroupMemory()
	if !ok || limit > math.MaxInt64 {
		return
	}
	debug.SetMemoryLimit(int64(limit - limit/10))
}

func memoryState(stats runtime.MemStats) (current, limit uint64, pressure bool) {
	if c, l, ok := cgroupMemory(); ok {
		current, limit = c, l
		pressure = fraction(c, l) >= 0.85
	} else if c, l, ok := hostMemory(); ok {
		current, limit = c, l
		pressure = fraction(c, l) >= 0.90
	}
	goLimit := debug.SetMemoryLimit(-1)
	goCurrent := stats.Sys - min(stats.Sys, stats.HeapReleased)
	if goLimit > 0 && goLimit < math.MaxInt64 && fraction(goCurrent, uint64(goLimit)) >= 0.85 {
		pressure = true
	}
	return current, limit, pressure
}

func fraction(current, limit uint64) float64 {
	if limit == 0 {
		return 0
	}
	return float64(current) / float64(limit)
}

func cgroupMemory() (current, limit uint64, ok bool) {
	for _, pair := range [][2]string{
		{"/sys/fs/cgroup/memory.current", "/sys/fs/cgroup/memory.max"},
		{"/sys/fs/cgroup/memory/memory.usage_in_bytes", "/sys/fs/cgroup/memory/memory.limit_in_bytes"},
	} {
		c, cok := readUintFile(pair[0])
		l, lok := readUintFile(pair[1])
		if cok && lok && l > 0 && l < 1<<62 {
			return c, l, true
		}
	}
	return 0, 0, false
}

func hostMemory() (current, limit uint64, ok bool) {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	var total, available uint64
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSuffix(fields[0], ":") {
		case "MemTotal":
			total = v * 1024
		case "MemAvailable":
			available = v * 1024
		}
	}
	if total == 0 || available > total {
		return 0, 0, false
	}
	return total - available, total, true
}

func readUintFile(path string) (uint64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v := strings.TrimSpace(string(raw))
	if v == "" || v == "max" {
		return 0, false
	}
	n, err := strconv.ParseUint(v, 10, 64)
	return n, err == nil
}

func min(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}
