package cache_test

import (
	"errors"
	"fmt"
	"time"

	"github.com/simonjanss/rig/runtime/cache"
)

// What a cache is for: the answer is read once and served from memory until
// something says otherwise. Here that answer is what a caller may do, which is
// the read rig cares about — a join over role tables, on every request, for
// something that last changed when somebody was hired.
func ExampleMap() {
	grants := cache.NewMap[[]string](cache.MapConfig{TTL: time.Minute})

	asked := 0
	read := func() ([]string, error) {
		asked++
		return []string{"note.read", "note.write"}, nil
	}

	for range 3 {
		held, err := grants.Load("tenant-1/account-1", read)
		if err != nil {
			panic(err)
		}
		fmt.Println(held)
	}
	fmt.Println("times asked:", asked)

	// Output:
	// [note.read note.write]
	// [note.read note.write]
	// [note.read note.write]
	// times asked: 1
}

// Forget is the whole reason this package can be used over authorization at all.
// The middle value is what a time-to-live cache on its own gives you — a role
// that changed and an answer that has not noticed — and it is why a [cache.Bus]
// carries the invalidation to every replica rather than leaving each one to wait
// out its window.
func ExampleMap_Forget() {
	roles := cache.NewMap[string](cache.MapConfig{TTL: time.Minute})

	held := "member"
	read := func() (string, error) { return held, nil }

	first, _ := roles.Load("tenant-1/account-1", read)

	// Somebody is promoted. Nothing has told this process.
	held = "admin"
	stale, _ := roles.Load("tenant-1/account-1", read)

	roles.Forget("tenant-1/account-1")
	fresh, _ := roles.Load("tenant-1/account-1", read)

	fmt.Println(first, stale, fresh)

	// Output:
	// member member admin
}

// A failure is answered and never kept. A database that was unreachable for one
// request does not get to decide what somebody may do for the rest of the
// window — which would turn a blip into an outage lasting exactly as long as the
// time-to-live.
func ExampleMap_Load() {
	grants := cache.NewMap[string](cache.MapConfig{TTL: time.Minute})

	_, err := grants.Load("tenant-1/account-1", func() (string, error) {
		return "", errors.New("connection refused")
	})
	fmt.Println("first:", err)

	held, err := grants.Load("tenant-1/account-1", func() (string, error) {
		return "admin", nil
	})
	fmt.Println("then: ", held, err)

	// Output:
	// first: connection refused
	// then:  admin <nil>
}

// An entry is good until its expiry rather than up to it, and [cache.MapConfig]
// takes the clock so that the edge of the window is a thing a test can stand on
// instead of sleep towards.
func ExampleMap_Load_window() {
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	grants := cache.NewMap[string](cache.MapConfig{
		TTL: time.Minute,
		Now: func() time.Time { return at },
	})

	asked := 0
	read := func() (string, error) {
		asked++
		return fmt.Sprintf("answer %d", asked), nil
	}

	first, _ := grants.Load("tenant-1/account-1", read)

	at = at.Add(time.Minute - 1)
	inside, _ := grants.Load("tenant-1/account-1", read)

	at = at.Add(1)
	outside, _ := grants.Load("tenant-1/account-1", read)

	fmt.Println(first, "|", inside, "|", outside)

	// Output:
	// answer 1 | answer 1 | answer 2
}

// The race a cache over authorization has to survive. An invalidation lands
// while the read is still in flight, so what comes back is already out of date:
// the caller is answered, because it was true when it was read, but nothing is
// stored — an entry written here would look fresh and outlive the change,
// leaving the one request that mattered as the one the cache got wrong.
func ExampleMap_Load_raced() {
	grants := cache.NewMap[string](cache.MapConfig{TTL: time.Hour})

	answered, _ := grants.Load("tenant-1/account-1", func() (string, error) {
		grants.Forget("tenant-1/account-1")
		return "member", nil
	})

	fmt.Println("answered:    ", answered)
	fmt.Println("entries held:", grants.Len())

	// Output:
	// answered:     member
	// entries held: 0
}

// A zero time-to-live caches nothing and calls through every time. That is so a
// project turns the cache off by changing a number rather than by unpicking its
// wiring, and so a duration read from configuration needs no condition around
// it.
func ExampleNewMap() {
	off := cache.NewMap[string](cache.MapConfig{TTL: 0})

	asked := 0
	read := func() (string, error) {
		asked++
		return "admin", nil
	}
	for range 3 {
		if _, err := off.Load("tenant-1/account-1", read); err != nil {
			panic(err)
		}
	}

	fmt.Println("times asked: ", asked)
	fmt.Println("entries held:", off.Len())

	// Output:
	// times asked:  3
	// entries held: 0
}
