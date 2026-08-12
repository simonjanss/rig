package notify_test

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simonjanss/rig/examples/todo/notify"
)

// The order the two shutdown steps run in is the whole point: recording stops
// while the server is still answering, and the last batch goes out after it has
// stopped. Reversed, the messages from the requests in flight are lost.
func TestDrainingStopsRecordingAndClosingFlushes(t *testing.T) {
	var out bytes.Buffer
	n := notify.New(&out, time.Hour) // never on the timer; this is about shutdown
	n.Start()

	n.Record("created one")
	n.Record("created two")

	if err := n.StopRecording(t.Context()); err != nil {
		t.Fatal(err)
	}

	// Draining does not write anything: the server is still serving.
	if out.Len() != 0 {
		t.Errorf("draining should not flush, wrote %q", out.String())
	}
	// And what arrives afterwards is dropped rather than kept forever.
	n.Record("too late")
	if n.Pending() != 2 {
		t.Errorf("pending = %d, want the two recorded before draining", n.Pending())
	}

	if err := n.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	for _, want := range []string{"created one", "created two"} {
		if !strings.Contains(got, want) {
			t.Errorf("the last batch should contain %q, got %q", want, got)
		}
	}
	if strings.Contains(got, "too late") {
		t.Errorf("a message recorded after draining should have been dropped: %q", got)
	}
}

func TestTheTimerFlushesWhileRunning(t *testing.T) {
	var out safeBuffer
	n := notify.New(&out, 5*time.Millisecond)
	n.Start()
	t.Cleanup(func() { _ = n.Close(context.Background()) })

	n.Record("on the timer")

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "on the timer") {
		if time.Now().After(deadline) {
			t.Fatal("the loop never flushed")
		}
		time.Sleep(time.Millisecond)
	}
}

// Close comes from a defer that also covers a failure during startup, so it has
// to survive being reached twice.
func TestCloseTwice(t *testing.T) {
	n := notify.New(&bytes.Buffer{}, time.Hour)
	n.Start()

	if err := n.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := n.Close(t.Context()); err != nil {
		t.Errorf("a second close should be a no-op: %v", err)
	}
}

// safeBuffer is a bytes.Buffer the flush loop and the test can share.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}
