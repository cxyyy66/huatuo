// Copyright 2026 The HuaTuo Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package symbol

import (
	"debug/elf"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	largeSymtabCount = 560_000
	largeDynsymCount = 56_000
	largeFuncCount   = largeSymtabCount*2/5 + largeDynsymCount*3/5
)

// largeSymName mirrors symName in testdata/gen_large_symtab.go; keep in sync.
func largeSymName(i int) string {
	length := 100 + (i*7)%401
	name := make([]byte, length)
	copy(name, fmt.Sprintf("f%07d_", i))
	for j := 9; j < length; j++ {
		name[j] = 'a' + byte((i+j)%26)
	}
	return string(name)
}

func largeSymPCs() []uint64 {
	return []uint64{
		0x1000 + 1*0x10 + 8,
		0x1000 + 100_000*0x10 + 8,
		0x1000 + 500_000*0x10 + 8,
	}
}

func largeSymWants() []string {
	return []string{largeSymName(1), largeSymName(100_000), largeSymName(500_000)}
}

// TestLargeSymtabResolve verifies a ~200MB production-like symbol table
// fixture: named functions resolve on demand, and resolver cache residency
// stays bounded because only requested PCs materialize symbol names.
func TestLargeSymtabResolve(t *testing.T) {
	path := filepath.Join("testdata", "large_symtab_200m.elf")
	f, err := elf.Open(path)
	if err != nil {
		t.Fatalf("elf.Open(%q): %v", path, err)
	}
	defer f.Close()

	pcs := largeSymPCs()
	want := largeSymWants()
	limits := ELFSymbolLimits{
		MaxMetadataBytes: 256 << 20,
		MaxSymbolCount:   largeSymtabCount + largeDynsymCount,
		MaxNameBytes:     4 << 20,
		MaxNameLength:    1 << 20,
	}

	// Simulate resolver cache residency: one cache entry per resolved module.
	// On-demand parsing retains only the symbols covering requested PCs.
	caches := make(map[string]symbols)
	var before, after runtime.MemStats
	beforeRSS := readVmHWM(t)
	runtime.ReadMemStats(&before)
	start := time.Now()
	for _, entry := range []string{"exe", "lib-1", "lib-2"} {
		syms, err := elfSymbolsForPCs(f, pcs, limits)
		if err != nil {
			t.Fatalf("elfSymbolsForPCs: %v", err)
		}
		caches[entry] = syms
		t.Logf("cache %s retained: %d symbols, cumulative peakRSS delta %d MiB", entry, len(syms), (readVmHWM(t)-beforeRSS)>>20)
	}
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	afterRSS := readVmHWM(t)
	t.Logf("large symtab (current): resolved %d addresses in %s; totalAlloc %d MiB, heapAlloc %d MiB, sys %d MiB, peakRSS delta %d MiB",
		len(want),
		elapsed,
		(after.TotalAlloc-before.TotalAlloc)>>20,
		after.HeapAlloc>>20,
		after.Sys>>20,
		(afterRSS-beforeRSS)>>20,
	)
	for _, syms := range caches {
		for i, pc := range pcs {
			if name := syms.resolve(pc); name != want[i] {
				t.Errorf("pc 0x%x: resolved %q, want %q", pc, name, want[i])
			}
		}
	}

	benchmark := testing.Benchmark(func(b *testing.B) {
		for range b.N {
			syms, err := elfSymbolsForPCs(f, pcs, limits)
			if err != nil {
				b.Fatalf("elfSymbolsForPCs: %v", err)
			}
			for i, pc := range pcs {
				if name := syms.resolve(pc); name != want[i] {
					b.Fatalf("pc 0x%x: resolved %q, want %q", pc, name, want[i])
				}
			}
		}
	})
	allocated := benchmark.AllocedBytesPerOp()
	allocs := benchmark.AllocsPerOp()
	t.Logf("large symtab parse: %d B/op (%d MiB), %d allocs/op, %d ns/op", allocated, allocated>>20, allocs, benchmark.NsPerOp())
	if allocated >= 512<<20 {
		t.Errorf("large symtab parse allocated %d B/op; want < %d", allocated, 512<<20)
	}
	if allocs >= 1000 {
		t.Errorf("large symtab parse made %d allocs/op; want < 1000 (full symbol materialization would make millions)", allocs)
	}
}

// readVmHWM returns the process peak RSS high-water mark in bytes.
func readVmHWM(t *testing.T) uint64 {
	t.Helper()
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatalf("read /proc/self/status: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "VmHWM:"); ok {
			var kib uint64
			if _, err := fmt.Sscanf(strings.TrimSpace(rest), "%d kB", &kib); err != nil {
				t.Fatalf("parse VmHWM %q: %v", rest, err)
			}
			return kib << 10
		}
	}
	t.Fatal("VmHWM not found in /proc/self/status")
	return 0
}
