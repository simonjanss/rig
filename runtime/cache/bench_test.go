package cache_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/simonjanss/rig/runtime/cache"
)

// grant stands in for what an authorization cache actually holds: two short
// slices of keys. Benchmarking a cache of ints would measure the map and not the
// thing.
type grant struct {
	roles       []string
	permissions []string
}

func aGrant() grant {
	return grant{
		roles:       []string{"owner", "admin"},
		permissions: []string{"note.read", "note.write", "note.delete", "note.read.all"},
	}
}

// BenchmarkLoadHit is the number that matters: what a cached read costs against
// the two database round trips it replaces. Anything in the tens of nanoseconds
// has won by four orders of magnitude, so the interesting figure is allocations
// per operation rather than time.
func BenchmarkLoadHit(b *testing.B) {
	m := cache.NewMap[grant](cache.MapConfig{TTL: time.Hour})
	held := aGrant()
	load := func() (grant, error) { return held, nil }

	if _, err := m.Load("tenant/account", load); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	for b.Loop() {
		if _, err := m.Load("tenant/account", load); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLoadMiss is the overhead a cache adds when it does not have the
// answer, which is what it costs on the paths it does not help: the clock, the
// read lock and the lookup that found nothing, then the write lock and the
// store.
//
// A key it does not hold every iteration, which takes some arranging. Reusing
// one key would measure a hit from the second iteration on, and formatting a
// fresh one would measure fmt — so the keys are built up front and the map is
// cleared on the wrap, which costs one pass over the set every four thousand
// operations and is not visible in the number.
func BenchmarkLoadMiss(b *testing.B) {
	m := cache.NewMap[grant](cache.MapConfig{TTL: time.Hour})
	held := aGrant()
	load := func() (grant, error) { return held, nil }

	keys := make([]string, 4096)
	for i := range keys {
		keys[i] = fmt.Sprintf("tenant/account-%d", i)
	}

	i := 0
	b.ReportAllocs()
	for b.Loop() {
		if _, err := m.Load(keys[i], load); err != nil {
			b.Fatal(err)
		}
		if i++; i == len(keys) {
			i = 0
			m.Clear()
		}
	}
}

// BenchmarkLoadDisabled is the other end of it: what a zero time-to-live costs,
// which is the price of leaving [cache.Map.Load] at a call site in a project
// that has turned the cache off. One comparison, and the point is that turning
// it off by changing a number is free rather than nearly free.
func BenchmarkLoadDisabled(b *testing.B) {
	m := cache.NewMap[grant](cache.MapConfig{TTL: 0})
	held := aGrant()
	load := func() (grant, error) { return held, nil }

	b.ReportAllocs()
	for b.Loop() {
		if _, err := m.Load("tenant/account", load); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLoadHitParallel is why these benchmarks exist, and it found something.
// A cache read takes a process-wide read lock, and a server's whole point is
// concurrency, so this is where a single RWMutex would say if it were the wrong
// structure. It is, in the strict sense — on an M4, `-cpu=1,2,4,8,10`:
//
//	1 core    39.65 ns/op    ~25.2M ops/s
//	2 cores   48.82 ns/op    ~20.5M ops/s
//	4 cores   93.84 ns/op    ~10.7M ops/s
//	10 cores  95.10 ns/op    ~10.5M ops/s
//
// Throughput *falls* as cores are added. That is the reader count inside
// sync.RWMutex: one cache line, atomically incremented by every reader on every
// read, bouncing between cores. Sharding the map by key hash would fix it.
//
// It is deliberately not sharded, and the numbers are why. The floor is ten
// million reads a second, against a cached read whose whole job is to replace two
// database round trips — call it 400µs. At 95ns this is four thousand times
// cheaper than the thing it replaces, and a server would have to be serving
// millions of requests a second before the lock was the narrowest part of
// anything. Sharding buys throughput nobody has asked for, and costs either a
// MaxEntries that quietly means per-shard or a shared counter that puts a
// contended write back on the miss path.
//
// So this is measured, not assumed, and it is written down here so that whoever
// finds a workload where ten million is not enough knows both that the ceiling is
// real and what to do about it.
func BenchmarkLoadHitParallel(b *testing.B) {
	m := cache.NewMap[grant](cache.MapConfig{TTL: time.Hour})
	held := aGrant()
	load := func() (grant, error) { return held, nil }

	// Several keys rather than one, because one key on one map entry is a
	// friendlier access pattern than any real workload has.
	keys := make([]string, 64)
	for i := range keys {
		keys[i] = fmt.Sprintf("tenant/account-%d", i)
		if _, err := m.Load(keys[i], load); err != nil {
			b.Fatal(err)
		}
	}

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if _, err := m.Load(keys[i%len(keys)], load); err != nil {
				b.Fatal(err)
			}
			i++
		}
	})
}

// BenchmarkForget is the write side. Invalidations are rare by construction — a
// role changes a few times a month — so this is here to show that the cost of
// being correct about them is not on the read path.
func BenchmarkForget(b *testing.B) {
	m := cache.NewMap[grant](cache.MapConfig{TTL: time.Hour})
	held := aGrant()
	load := func() (grant, error) { return held, nil }
	if _, err := m.Load("tenant/account", load); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	for b.Loop() {
		m.Forget("tenant/account")
	}
}
