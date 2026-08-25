//go:build docker

package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/auth"
	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/auth/authpg"
	"github.com/simonjanss/rig/examples/auth/internal/api"
	"github.com/simonjanss/rig/examples/auth/services/authz"
)

// flaky is a Notifier that refuses until it is told to stop, and records what it
// was handed.
type flaky struct {
	mu     sync.Mutex
	down   bool
	tokens []string
}

func (f *flaky) send(token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return errors.New("503 Service Unavailable")
	}
	f.tokens = append(f.tokens, token)
	return nil
}

func (f *flaky) SendPasswordReset(_ context.Context, _ *account.Identity, t string) error {
	return f.send(t)
}
func (f *flaky) SendEmailVerification(_ context.Context, _ *account.Identity, t string) error {
	return f.send(t)
}
func (f *flaky) SendInvitation(_ context.Context, _ *account.Identity, _ *account.Account, t string) error {
	return f.send(t)
}

func (f *flaky) recover() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = false
}

func (f *flaky) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.tokens...)
}

// The whole point of the queue, over the real SQL: a provider that is down does
// not fail the request, and the link goes out when it comes back.
func TestAResetSurvivesAProviderBeingDown(t *testing.T) {
	s := newServer(t)
	tenant := s.seed(t)

	// Its own address rather than the seeded one. Several tests here spend the
	// seeded address's password-reset budget, and the limiter counts per address,
	// so a test that borrowed it would fail for a reason it is not about.
	mailbox := s.emailOf(t, s.addAccount(t, tenant, "outage"))

	provider := &flaky{down: true}
	front, err := api.New(s.pool, api.Hooks{
		Notifier: provider,
		Mail:     auth.MailOptions{Queue: true, Retention: 30 * 24 * time.Hour},
		Grants:   authz.Grants(s.pool),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Clean, because the seeded tenant is shared between the tests in this
	// package and counting is what this one does.
	if _, err := s.pool.Exec(ctx, `DELETE FROM rig_identity_verification_delivery`); err != nil {
		t.Fatal(err)
	}

	// The request succeeds with the provider hard down, which it did not before.
	if err := front.Parts().Accounts.RequestPasswordReset(ctx, tenant, mailbox, "203.0.113.9"); err != nil {
		t.Fatalf("the request failed while the provider was down: %v", err)
	}
	if got := len(provider.sent()); got != 0 {
		t.Fatalf("%d mails went out from the request path", got)
	}
	if got := countDeliveries(t, s.pool, "Pending"); got != 1 {
		t.Fatalf("%d rows are Pending after the request, want 1", got)
	}

	// A pass while it is still down retries rather than failing.
	report, err := front.DispatchMail(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Retrying != 1 {
		t.Errorf("retrying = %d, want 1 (%s)", report.Retrying, report)
	}

	// The provider comes back. Bring the retry forward rather than waiting it out.
	provider.recover()
	if _, err := s.pool.Exec(ctx,
		`UPDATE rig_identity_verification_delivery SET deliver_at = now() - interval '1 minute'
		 WHERE state = 'Pending'`); err != nil {
		t.Fatal(err)
	}

	if report, err = front.DispatchMail(ctx); err != nil {
		t.Fatal(err)
	}
	if report.Sent != 1 {
		t.Fatalf("sent = %d, want 1 (%s)", report.Sent, report)
	}

	// And the token from the mail actually redeems, which is the assertion the
	// whole rotate-at-send-time design exists to keep true.
	sent := provider.sent()
	if len(sent) != 1 {
		t.Fatalf("the provider saw %d mails, want 1", len(sent))
	}
	if err := front.Parts().Accounts.ConfirmPasswordReset(ctx, sent[0],
		"a password from the mail that survived", "203.0.113.9"); err != nil {
		t.Errorf("the token from the queued mail does not redeem: %v", err)
	}
}

// The claim is a lease over the real statement: two dispatchers on one row send
// once between them.
func TestTwoMailDispatchersSendOnce(t *testing.T) {
	s := newServer(t)
	tenant := s.seed(t)
	ctx := context.Background()

	if _, err := s.pool.Exec(ctx, `DELETE FROM rig_identity_verification_delivery`); err != nil {
		t.Fatal(err)
	}

	mailbox := s.emailOf(t, s.addAccount(t, tenant, "raced"))

	provider := &flaky{}
	fronts := make([]*auth.Auth, 3)
	for i := range fronts {
		f, err := api.New(s.pool, api.Hooks{
			Notifier: provider,
			Mail:     auth.MailOptions{Queue: true},
			Grants:   authz.Grants(s.pool),
		})
		if err != nil {
			t.Fatal(err)
		}
		fronts[i] = f
	}

	if err := fronts[0].Parts().Accounts.RequestPasswordReset(ctx, tenant, mailbox, "203.0.113.9"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for _, f := range fronts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.DispatchMail(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if got := len(provider.sent()); got != 1 {
		t.Errorf("three dispatchers sent %d mails for one link, want 1", got)
	}
	if got := countDeliveries(t, s.pool, "Sent"); got != 1 {
		t.Errorf("%d rows are Sent, want 1", got)
	}
}

// A link withdrawn before the mail went out is never mailed, which is the thing
// the inline path could not do at all.
func TestAWithdrawnInvitationIsNotMailed(t *testing.T) {
	s := newServer(t)
	tenant := s.seed(t)
	ctx := context.Background()

	if _, err := s.pool.Exec(ctx, `DELETE FROM rig_identity_verification_delivery`); err != nil {
		t.Fatal(err)
	}

	mailbox := s.emailOf(t, s.addAccount(t, tenant, "withdrawn"))

	provider := &flaky{}
	front, err := api.New(s.pool, api.Hooks{
		Notifier: provider,
		Mail:     auth.MailOptions{Queue: true},
		Grants:   authz.Grants(s.pool),
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := front.Parts().Accounts.RequestPasswordReset(ctx, tenant, mailbox, "203.0.113.9"); err != nil {
		t.Fatal(err)
	}

	// Withdraw the link the delivery owns.
	var vid string
	if err := s.pool.QueryRow(ctx,
		`SELECT verification_id FROM rig_identity_verification_delivery LIMIT 1`).Scan(&vid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE rig_identity_verification SET revoked_at = now() WHERE id = $1`, vid); err != nil {
		t.Fatal(err)
	}

	report, err := front.DispatchMail(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Skipped != 1 {
		t.Errorf("skipped = %d, want 1 (%s)", report.Skipped, report)
	}
	if got := len(provider.sent()); got != 0 {
		t.Errorf("%d mails went out for a withdrawn link", got)
	}
}

// A queued link has no token, so nothing can reach it by one.
//
// This is the property the whole "the row holds intent, never a secret" design
// exists for: between being queued and being sent there is no plaintext anywhere,
// and the column that would hold its hash is null.
func TestAQueuedLinkCannotBeRedeemed(t *testing.T) {
	s := newServer(t)
	tenant := s.seed(t)
	ctx := context.Background()

	if _, err := s.pool.Exec(ctx, `DELETE FROM rig_identity_verification_delivery`); err != nil {
		t.Fatal(err)
	}

	// Queue one, so this asserts about a row that exists rather than about an
	// empty table.
	mailbox := s.emailOf(t, s.addAccount(t, tenant, "unsent"))

	front, err := api.New(s.pool, api.Hooks{
		Notifier: &flaky{down: true},
		Mail:     auth.MailOptions{Queue: true},
		Grants:   authz.Grants(s.pool),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := front.Parts().Accounts.RequestPasswordReset(ctx, tenant, mailbox, "203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	if got := countDeliveries(t, s.pool, "Pending"); got != 1 {
		t.Fatalf("%d rows are queued, so this proves nothing", got)
	}

	var n int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM rig_identity_verification v
		JOIN rig_identity_verification_delivery d ON d.verification_id = v.id
		WHERE d.state = 'Pending' AND v.token_hash IS NOT NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d queued links already carry a token hash, so the secret is at rest", n)
	}

	// And the store's own lookup cannot find a null-hash row by any hash.
	stores := authpg.New(s.pool)
	for _, probe := range [][]byte{nil, {}, make([]byte, 32)} {
		v, err := stores.Accounts.VerificationByHash(ctx, probe)
		if err != nil && !strings.Contains(err.Error(), "no rows") {
			t.Fatal(err)
		}
		if v != nil {
			t.Errorf("a probe of %d bytes reached verification %s", len(probe), v.ID)
		}
	}
}

// countDeliveries is how many rows are in one state, which is most of what these
// tests assert.
func countDeliveries(t *testing.T, pool *pgxpool.Pool, state string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM rig_identity_verification_delivery WHERE state = $1`,
		state).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
