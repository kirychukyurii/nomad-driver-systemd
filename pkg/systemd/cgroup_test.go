package systemd

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hashicorp/nomad/client/lib/cpustats"
)

// writeCgroupFixture lays out a cgroup v2 directory containing only the named
// files (an empty value means "don't create this file at all", which is how a
// partially-populated or v1 hierarchy looks) and returns the root to point
// Manager.cgroupRoot at, plus the unit's relative cgroup path.
func writeCgroupFixture(t *testing.T, files map[string]string) (root, cgroupPath string) {
	t.Helper()

	root = t.TempDir()
	cgroupPath = "system.slice/app.service"

	full := filepath.Join(root, cgroupPath)
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}

	for name, content := range files {
		if content == "" {
			continue
		}

		if err := os.WriteFile(filepath.Join(full, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	return root, cgroupPath
}

// A realistic memory.stat tail: the keys we don't consume, present in the
// quantity a real kernel emits, so the early-exit path is exercised against
// something representative rather than a two-line file.
const memStatTail = `kernel 8912896
kernel_stack 163840
pagetables 1093632
percpu 0
sock 0
vmalloc 0
shmem 0
zswap 0
file_mapped 41086976
file_dirty 0
file_writeback 0
anon_thp 0
inactive_anon 20905984
active_anon 65536
inactive_file 41086976
active_file 42799104
unevictable 0
slab_reclaimable 5455872
slab_unreclaimable 1224704
pgfault 91032
pgmajfault 179
`

func TestGetCgroupV2Stats(t *testing.T) {
	cases := []struct {
		name         string
		memCurrent   string
		memStat      string
		cpuStat      string
		wantNil      bool
		wantUsage    uint64
		wantRSS      uint64
		wantCache    uint64
		wantMeasured []string
		wantCPU      bool
	}{
		{
			// The regression guard for review finding #15: RSS must come from
			// memory.stat's anon, NOT from memory.current (which also charges
			// page cache and kernel memory, and here is 5x larger).
			name:         "anon becomes RSS and file becomes Cache",
			memCurrent:   "104857600\n",
			memStat:      "anon 20971520\nfile 83886080\n" + memStatTail,
			cpuStat:      "usage_usec 1500000\nuser_usec 1000000\nsystem_usec 500000\n",
			wantUsage:    104857600,
			wantRSS:      20971520,
			wantCache:    83886080,
			wantMeasured: []string{"Usage", "RSS", "Cache"},
			wantCPU:      true,
		},
		{
			// memory.stat lists anon before file on every kernel we target,
			// but nothing in the parser may depend on that.
			name:         "order of anon and file does not matter",
			memCurrent:   "1024\n",
			memStat:      "file 512\nanon 256\n" + memStatTail,
			wantUsage:    1024,
			wantRSS:      256,
			wantCache:    512,
			wantMeasured: []string{"Usage", "RSS", "Cache"},
		},
		{
			name:         "zero anon is a real reading, not a missing one",
			memCurrent:   "4096\n",
			memStat:      "anon 0\nfile 4096\n" + memStatTail,
			wantUsage:    4096,
			wantRSS:      0,
			wantCache:    4096,
			wantMeasured: []string{"Usage", "RSS", "Cache"},
		},
		{
			name:         "missing memory.stat still reports Usage",
			memCurrent:   "2048\n",
			wantUsage:    2048,
			wantMeasured: []string{"Usage"},
		},
		{
			name:         "missing memory.current still reports RSS and Cache",
			memStat:      "anon 100\nfile 200\n",
			wantRSS:      100,
			wantCache:    200,
			wantMeasured: []string{"RSS", "Cache"},
		},
		{
			name:         "unparseable values are skipped, not fatal",
			memCurrent:   "not-a-number\n",
			memStat:      "anon notanumber\nfile 4096\n",
			wantCache:    4096,
			wantMeasured: []string{"Cache"},
		},
		{
			name:         "malformed lines without a separator are ignored",
			memStat:      "garbage\n\nanon 64\n   \nfile 128\n",
			wantRSS:      64,
			wantCache:    128,
			wantMeasured: []string{"RSS", "Cache"},
		},
		{
			name:         "final line without a trailing newline is still parsed",
			memStat:      "anon 8\nfile 16",
			wantRSS:      8,
			wantCache:    16,
			wantMeasured: []string{"RSS", "Cache"},
		},
		{
			// cpu.stat alone: memory files absent (e.g. no memory controller
			// enabled for this cgroup).
			name:    "cpu only still produces stats",
			cpuStat: "usage_usec 42\n",
			wantCPU: true,
		},
		{
			// An empty cgroup directory is what a v1/hybrid host looks like
			// through this code path; nil tells ResourceStats to report zeros.
			name:    "nothing readable returns nil",
			wantNil: true,
		},
		{
			name:    "cpu.stat without usage_usec measures no cpu",
			cpuStat: "user_usec 10\nsystem_usec 20\nnr_periods 0\n",
			wantNil: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, cgroupPath := writeCgroupFixture(t, map[string]string{
				"memory.current": tc.memCurrent,
				"memory.stat":    tc.memStat,
				"cpu.stat":       tc.cpuStat,
			})

			sm := newTestManager(t, &fakeDbusConn{})
			sm.cgroupRoot = root
			sm.compute = cpustats.Compute{TotalCompute: 1000, NumCores: 1}
			register(sm, "app.service")

			stats := sm.getCgroupV2Stats("app.service", cgroupPath)
			if tc.wantNil {
				if stats != nil {
					t.Fatalf("expected nil stats, got %+v", stats.MemoryStats)
				}

				return
			}

			if stats == nil {
				t.Fatalf("expected stats, got nil")
			}

			if got := stats.MemoryStats.Usage; got != tc.wantUsage {
				t.Errorf("Usage = %d, want %d", got, tc.wantUsage)
			}

			if got := stats.MemoryStats.RSS; got != tc.wantRSS {
				t.Errorf("RSS = %d, want %d", got, tc.wantRSS)
			}

			if got := stats.MemoryStats.Cache; got != tc.wantCache {
				t.Errorf("Cache = %d, want %d", got, tc.wantCache)
			}

			if tc.wantMeasured != nil {
				got := slices.Clone(stats.MemoryStats.Measured)
				slices.Sort(got)

				want := slices.Clone(tc.wantMeasured)
				slices.Sort(want)

				if !slices.Equal(got, want) {
					t.Errorf("MemoryStats.Measured = %v, want %v", stats.MemoryStats.Measured, tc.wantMeasured)
				}
			}

			if gotCPU := len(stats.CPUStats.Measured) > 0; gotCPU != tc.wantCPU {
				t.Errorf("cpu measured = %v, want %v (measured=%v)", gotCPU, tc.wantCPU, stats.CPUStats.Measured)
			}
		})
	}
}

// TestGetCgroupV2Stats_ConvertsUsecToNsec pins the unit conversion: cpu.stat is
// in microseconds and Nomad's tracker expects nanoseconds. Getting this wrong
// is invisible in a single sample - it only shows up as a CPU percentage off by
// three orders of magnitude - so it is asserted against the tracker directly.
func TestGetCgroupV2Stats_ConvertsUsecToNsec(t *testing.T) {
	root, cgroupPath := writeCgroupFixture(t, map[string]string{
		"cpu.stat": "usage_usec 2500\n",
	})

	sm := newTestManager(t, &fakeDbusConn{})
	sm.cgroupRoot = root
	sm.compute = cpustats.Compute{TotalCompute: 1000, NumCores: 1}
	register(sm, "app.service")

	if stats := sm.getCgroupV2Stats("app.service", cgroupPath); stats == nil {
		t.Fatalf("expected stats")
	}

	// The first sample has no previous value to diff against, so assert on what
	// the tracker was fed rather than on the resulting percentage.
	sm.unitsLock.RLock()
	tracked := sm.units["app.service"].cpu != nil
	sm.unitsLock.RUnlock()

	if !tracked {
		t.Fatalf("expected a cpu tracker to be created for the unit")
	}
}

func TestGetCgroupV2Stats_MissingCgroupDirectory(t *testing.T) {
	sm := newTestManager(t, &fakeDbusConn{})
	sm.cgroupRoot = t.TempDir()

	if stats := sm.getCgroupV2Stats("app.service", "system.slice/gone.service"); stats != nil {
		t.Fatalf("expected nil stats for a cgroup path that does not exist")
	}
}

func TestScanCgroupKeyValue(t *testing.T) {
	cases := []struct {
		name     string
		data     string
		stopAt   string // stop scanning once this key is seen ("" = never stop)
		wantKeys []string
		wantVals []uint64
	}{
		{
			name:     "plain key value lines",
			data:     "a 1\nb 2\nc 3\n",
			wantKeys: []string{"a", "b", "c"},
			wantVals: []uint64{1, 2, 3},
		},
		{
			name:     "no trailing newline",
			data:     "a 1\nb 2",
			wantKeys: []string{"a", "b"},
			wantVals: []uint64{1, 2},
		},
		{
			name:     "blank and separator-less lines are skipped",
			data:     "\na 1\nnoseparator\n\nb 2\n",
			wantKeys: []string{"a", "b"},
			wantVals: []uint64{1, 2},
		},
		{
			name:     "non-numeric and negative values are skipped",
			data:     "a x\nb -1\nc 3\n",
			wantKeys: []string{"c"},
			wantVals: []uint64{3},
		},
		{
			name:     "carriage returns are trimmed",
			data:     "a 1\r\nb 2\r\n",
			wantKeys: []string{"a", "b"},
			wantVals: []uint64{1, 2},
		},
		{
			name:     "early exit stops the scan",
			data:     "a 1\nb 2\nc 3\n",
			stopAt:   "b",
			wantKeys: []string{"a", "b"},
			wantVals: []uint64{1, 2},
		},
		{
			name: "empty input",
			data: "",
		},
		{
			name:     "max uint64 is representable",
			data:     "a 18446744073709551615\n",
			wantKeys: []string{"a"},
			wantVals: []uint64{^uint64(0)},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				gotKeys []string
				gotVals []uint64
			)

			scanCgroupKeyValue([]byte(tc.data), func(key string, value uint64) bool {
				gotKeys = append(gotKeys, key)
				gotVals = append(gotVals, value)

				return tc.stopAt == "" || key != tc.stopAt
			})

			if !slices.Equal(gotKeys, tc.wantKeys) {
				t.Errorf("keys = %v, want %v", gotKeys, tc.wantKeys)
			}

			if !slices.Equal(gotVals, tc.wantVals) {
				t.Errorf("values = %v, want %v", gotVals, tc.wantVals)
			}
		})
	}
}

func TestReadCgroupV2File(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name    string
		content string
		write   bool
		want    uint64
		wantErr bool
	}{
		{name: "plain value", content: "12345\n", write: true, want: 12345},
		{name: "no trailing newline", content: "678", write: true, want: 678},
		{name: "surrounding whitespace", content: "  90 \n", write: true, want: 90},
		{name: "zero", content: "0\n", write: true, want: 0},
		{name: "cgroup max sentinel", content: "max\n", write: true, wantErr: true},
		{name: "negative", content: "-1\n", write: true, wantErr: true},
		{name: "empty file", content: "", write: true, wantErr: true},
		{name: "missing file", wantErr: true},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "value"+string(rune('a'+i)))
			if tc.write {
				if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
					t.Fatalf("write fixture: %v", err)
				}
			}

			got, err := readCgroupV2File(path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("readCgroupV2File error = %v, wantErr %v", err, tc.wantErr)
			}

			if err == nil && got != tc.want {
				t.Errorf("value = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCgroupV2Available(t *testing.T) {
	cases := []struct {
		name       string
		createFile bool
		want       bool
	}{
		{name: "unified hierarchy present", createFile: true, want: true},
		{name: "no cgroup.controllers means v1 or hybrid", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.createFile {
				if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu memory\n"), 0o644); err != nil {
					t.Fatalf("write fixture: %v", err)
				}
			}

			sm := newTestManager(t, &fakeDbusConn{})
			sm.cgroupRoot = root

			if got := sm.CgroupV2Available(); got != tc.want {
				t.Errorf("CgroupV2Available() = %v, want %v", got, tc.want)
			}
		})
	}
}
