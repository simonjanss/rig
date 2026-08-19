package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sync"
	"time"
)

// Memory keeps objects in a map.
//
// It is for tests and for `go run`, and it is not durable — which is worth
// saying in the type rather than only in a README, because the failure mode of
// discovering it in production is losing every upload on a restart.
//
// It implements [Marker] as well as [Store], so a test can assert that a delete
// marked the object and a restore unmarked it. It deliberately does not
// implement [Signer]: there is no URL a client could usefully be sent to, and a
// method that returns an error would make "does this backend sign?" a question
// with two answers.
type Memory struct {
	mu      sync.RWMutex
	objects map[string]*object
	// now is the clock, so a test can make the restore window pass without
	// waiting. Nil means time.Now.
	now func() time.Time
}

type object struct {
	data     []byte
	info     Info
	state    State
	markedAt time.Time
}

// NewMemory builds an empty store.
func NewMemory() *Memory { return &Memory{objects: map[string]*object{}} }

// SetClock replaces the clock, for a test that needs to age an object.
func (m *Memory) SetClock(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}

func (m *Memory) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now().UTC()
}

// Put implements [Store].
func (m *Memory) Put(_ context.Context, key string, r io.Reader, _ PutOptions) (Info, error) {
	// Hashed as it is read rather than afterwards, so the bytes are only walked
	// once and a caller cannot hand over content that differs from what was
	// hashed.
	sum := sha256.New()
	buf, err := io.ReadAll(io.TeeReader(r, sum))
	if err != nil {
		return Info{}, err
	}

	info := Info{
		Size:     int64(len(buf)),
		Checksum: hex.EncodeToString(sum.Sum(nil)),
		ModTime:  m.clock(),
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.objects == nil {
		m.objects = map[string]*object{}
	}
	m.objects[key] = &object{data: buf, info: info, state: StateLive}
	return info, nil
}

// Get implements [Store].
func (m *Memory) Get(_ context.Context, key string) (io.ReadSeekCloser, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	o, ok := m.objects[key]
	if !ok {
		return nil, ErrNotFound
	}
	// A copy of the header, not of the bytes: the slice is never written again
	// after Put, and handing out a reader over it is what makes a range read
	// free.
	return nopCloser{bytes.NewReader(o.data)}, nil
}

// Stat implements [Store].
func (m *Memory) Stat(_ context.Context, key string) (Info, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	o, ok := m.objects[key]
	if !ok {
		return Info{}, ErrNotFound
	}
	return o.info, nil
}

// Delete implements [Store], and says nothing about a key already gone.
func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.objects, key)
	return nil
}

// Mark implements [Marker].
func (m *Memory) Mark(_ context.Context, key string, state State, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	o, ok := m.objects[key]
	if !ok {
		return ErrNotFound
	}
	o.state, o.markedAt = state, at
	return nil
}

// State reports what an object is marked as, for a test that wants to assert
// the mark rather than infer it.
func (m *Memory) State(key string) (State, time.Time, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	o, ok := m.objects[key]
	if !ok {
		return "", time.Time{}, false
	}
	return o.state, o.markedAt, true
}

// Keys is every key currently held, for the sweeper's tests.
func (m *Memory) Keys() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]string, 0, len(m.objects))
	for k := range m.objects {
		out = append(out, k)
	}
	return out
}

// nopCloser adds the Close a [Store] reader has to have to a bytes.Reader,
// which has everything else.
type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }

// Compile-time proof of the two interfaces Memory claims, and of the one it
// does not: a caller asking whether this backend can sign has to get no.
var (
	_ Store  = (*Memory)(nil)
	_ Marker = (*Memory)(nil)
)
