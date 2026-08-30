package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// recorder is a logger keeping every line, at debug, so that a test can ask
// what the lifecycle said rather than only what it returned.
func recorder(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// lines are the records written, decoded, in the order they were written.
func lines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()

	var out []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if raw == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			t.Fatalf("a log line is not json: %v\n%s", err, raw)
		}
		out = append(out, rec)
	}
	return out
}

// only returns the one line with this message, and fails when there is not
// exactly one.
func only(t *testing.T, buf *bytes.Buffer, msg string) map[string]any {
	t.Helper()

	var found []map[string]any
	for _, rec := range lines(t, buf) {
		if rec["msg"] == msg {
			found = append(found, rec)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one %q line, got %d in:\n%s", msg, len(found), buf.String())
	}
	return found[0]
}

// stepLine returns the line this phase wrote about its step.
func stepLine(t *testing.T, buf *bytes.Buffer, msg, phase string) map[string]any {
	t.Helper()

	for _, rec := range lines(t, buf) {
		if rec["msg"] == msg && rec["phase"] == phase {
			return rec
		}
	}
	t.Fatalf("no %q line for the %s phase in:\n%s", msg, phase, buf.String())
	return nil
}

// The line that makes "which step ate the budget" answerable. Both halves: the
// one written before a step runs is the only one a step that never returns
// leaves behind.
func TestEveryShutdownStepIsNamedBeforeAndAfterItRuns(t *testing.T) {
	var buf bytes.Buffer
	app := &App{Logger: recorder(&buf)}

	app.DrainWithin("notifications", 2*time.Second, func(context.Context) error { return nil })
	app.CloseWithin("presence", time.Second, func(context.Context) error { return nil })

	if err := app.runDrain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if err := app.runClose(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("close: %v", err)
	}

	started := stepLine(t, &buf, "shutdown step", "drain")
	if started["step"] != "notifications" {
		t.Errorf("the drain step is not named: %v", started)
	}
	if started["timeout"] == nil {
		t.Errorf("the drain step does not say what it was allowed: %v", started)
	}

	var finished int
	for _, rec := range lines(t, &buf) {
		if rec["msg"] == "shutdown step finished" {
			finished++
			if rec["in"] == nil {
				t.Errorf("a finished step does not say how long it took: %v", rec)
			}
		}
	}
	if finished != 2 {
		t.Errorf("finished lines = %d, want one per step", finished)
	}
}

// A step cut off at its deadline comes back through the same defer as one that
// returned, so the second half of the pair has to say which it was. Without the
// error, "it used its whole two seconds" and "it was still going at two
// seconds" are the same line — and telling those apart is the only reason the
// pair exists.
func TestAStepThatGaveUpSaysSoOnTheLineThatSaysItFinished(t *testing.T) {
	var buf bytes.Buffer
	app := &App{Logger: recorder(&buf)}

	// Released at the end of the test rather than never: the goroutine behind a
	// step that ignored its context is left behind by design, and a test should
	// not leave one behind for the rest of the run.
	release := make(chan struct{})
	defer close(release)
	app.CloseWithin("store", 10*time.Millisecond, func(context.Context) error {
		<-release
		return nil
	})

	if err := app.runClose(context.Background(), 5*time.Second); err == nil {
		t.Fatal("a step that ran past its own timeout should be an error")
	}

	rec := stepLine(t, &buf, "shutdown step finished", "close")
	if rec["error"] == nil {
		t.Errorf("the line reads as a clean finish for a step that gave up: %v", rec)
	}
	if rec["in"] == nil {
		t.Errorf("a step that gave up still took time, and does not say how long: %v", rec)
	}
}

// And the other way round: a step that did finish carries no error, so the
// attribute means what it says.
func TestAStepThatFinishedCarriesNoError(t *testing.T) {
	var buf bytes.Buffer
	app := &App{Logger: recorder(&buf)}

	app.CloseWithin("store", time.Second, func(context.Context) error { return nil })
	if err := app.runClose(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("close: %v", err)
	}

	if rec := stepLine(t, &buf, "shutdown step finished", "close"); rec["error"] != nil {
		t.Errorf("error = %v, want it left out entirely", rec["error"])
	}
}

// A step registered with no limit of its own takes whatever is left, so the
// line must not claim it was allowed nothing.
func TestAnUnboundedStepIsNotReportedAsHavingNoTime(t *testing.T) {
	var buf bytes.Buffer
	app := &App{Logger: recorder(&buf)}

	app.Close("store", func(context.Context) error { return nil })
	if err := app.runClose(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("close: %v", err)
	}

	if timeout := stepLine(t, &buf, "shutdown step", "close")["timeout"]; timeout != nil {
		t.Errorf("timeout = %v, want it left out entirely", timeout)
	}
}

// The steps a process registered are listed nowhere else. checkShutdown adds
// them up already; this is only whether it says so.
func TestABudgetThatFitsSaysWhatItIsMadeOf(t *testing.T) {
	var buf bytes.Buffer
	app := &App{Logger: recorder(&buf)}
	app.CloseWithin("presence", 5*time.Second, func(context.Context) error { return nil })

	if err := app.checkShutdown(context.Background(), 30*time.Second, 2*time.Second); err != nil {
		t.Fatalf("checkShutdown: %v", err)
	}

	rec := only(t, &buf, "the shutdown budget fits")
	steps, ok := rec["steps"].([]any)
	if !ok || len(steps) != 2 {
		t.Fatalf("steps = %v, want the drain delay and the one close step", rec["steps"])
	}
	if !strings.Contains(steps[1].(string), "presence") {
		t.Errorf("the registered step is not named: %v", steps)
	}
}

// SIGTERM, SIGINT and a parent that cancelled are one event to
// signal.NotifyContext. They are not to this.
func TestTheSignalThatStoppedItIsWhatTheCauseSays(t *testing.T) {
	ctx, stop := signalled(context.Background(), syscall.SIGUSR1)
	defer stop()

	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatalf("signalling this process: %v", err)
	}

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the signal did not cancel the context")
	}

	if got := stopCause(ctx); !strings.HasPrefix(got, "signal ") {
		t.Errorf("cause = %q, want it to name the signal", got)
	}
}

// Stopping is not a signal, and must not read like one.
func TestStoppingWithoutASignalSaysSo(t *testing.T) {
	ctx, stop := signalled(context.Background(), syscall.SIGUSR1)
	stop()

	<-ctx.Done()

	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", ctx.Err())
	}
	if got := stopCause(ctx); strings.HasPrefix(got, "signal ") {
		t.Errorf("cause = %q, want something that is not a signal", got)
	}
}

// A cause is what Run puts on the draining line, so it is never empty and never
// a nil dereference.
func TestACancelledContextAlwaysHasSomethingToSayAboutWhy(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errors.New("the deployment asked it to"))

	if got := stopCause(ctx); got != "the deployment asked it to" {
		t.Errorf("cause = %q", got)
	}

	plain, cancelPlain := context.WithCancel(context.Background())
	cancelPlain()
	if got := stopCause(plain); got == "" {
		t.Error("a context cancelled with no cause said nothing at all")
	}
}

// Every absent piece of build information is left out rather than logged empty,
// so what reaches the logger is always whole pairs.
func TestTheBuildLineIsWholePairs(t *testing.T) {
	attrs := buildAttrs()
	if len(attrs)%2 != 0 {
		t.Fatalf("buildAttrs = %v, which is not key-value pairs", attrs)
	}
	for i := 0; i < len(attrs); i += 2 {
		key, ok := attrs[i].(string)
		if !ok || key == "" {
			t.Errorf("attribute %d has no name: %v", i, attrs[i])
		}
		if s, ok := attrs[i+1].(string); ok && s == "" {
			t.Errorf("%s is empty and should have been left out", key)
		}
	}
}
