package notify_test

import (
	"strings"
	"testing"
	"time"

	"github.com/simonjanss/rig/notify"
)

// A send allowed to outlive the lease protecting its row means every slow
// message is sent twice, because the second dispatcher was right to think the
// claim had expired. Refused at construction rather than found as duplicate
// mail, which is the same argument MinClaimTTL is refused on.
func TestASendMayNotOutliveItsLease(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a send_timeout above claim_ttl was accepted")
		}
		// Both numbers, because the fix is to change one of them and the message
		// should not make the reader work out which two values disagreed.
		msg, _ := r.(string)
		for _, want := range []string{"send_timeout", "claim_ttl", "2m", "5m"} {
			if !strings.Contains(msg, want) {
				t.Errorf("the panic does not mention %q: %s", want, msg)
			}
		}
	}()

	notify.NewEngine(notify.EngineConfig{
		ClaimTTL:    2 * time.Minute,
		SendTimeout: 5 * time.Minute,
	})
}

// Equal is refused with longer, because a lease is stamped before the send it
// protects starts: a send allowed to run the whole lease ends after it.
func TestASendMayNotFillItsWholeLease(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatal("a send_timeout equal to claim_ttl was accepted")
		}
	}()

	notify.NewEngine(notify.EngineConfig{
		ClaimTTL:    2 * time.Minute,
		SendTimeout: 2 * time.Minute,
	})
}

// The inverse, in its own test rather than as an afterthought in the one above:
// a check that refused every pair would satisfy that assertion and make the
// engine unbuildable.
func TestAnOrdinaryPairIsAccepted(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("an ordinary configuration was refused: %v", r)
		}
	}()

	notify.NewEngine(notify.EngineConfig{
		ClaimTTL:    5 * time.Minute,
		SendTimeout: 30 * time.Second,
	})
}

// The zero value has to be a working configuration, because the generated wiring
// is not the only caller — anybody assembling an EngineConfig by hand gets the
// defaults, and a default pair this package refuses would be a panic on a
// configuration nobody wrote.
func TestTheDefaultsBuildAnEngine(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("the defaults do not build: %v", r)
		}
	}()

	notify.NewEngine(notify.EngineConfig{})

	if notify.DefaultSendTimeout >= notify.DefaultClaimTTL {
		t.Errorf("DefaultSendTimeout %s does not fit inside DefaultClaimTTL %s",
			notify.DefaultSendTimeout, notify.DefaultClaimTTL)
	}
}

// Every count, zeros included, for the reason the report itself gives: the
// absence of a line cannot be told from the job not running. Abandoned is the
// one that says a channel has become slow enough to matter, so a report that
// omitted it would hide exactly the condition it was added for.
func TestTheReportLineNamesEveryCount(t *testing.T) {
	t.Parallel()

	line := notify.DispatchReport{}.String()
	for _, want := range []string{
		"claimed", "sent", "failed", "retrying", "held", "digested", "released", "abandoned",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the report line does not mention %q: %s", want, line)
		}
	}
}
