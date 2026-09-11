package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/examples/linearlite/internal/generated/api"
	"github.com/simonjanss/rig/examples/linearlite/internal/services/authz"
)

// The seed's fixed identifiers, so the README can show a sign-in that works,
// the tests can log in without reading anything back, and OnRegistered knows
// which tenant to invite a stranger into.
const (
	SeedTenantID = "00000000-0000-0000-0000-000000000001"
	SeedEmail    = "demo@linearlite.dev"
	SeedEmail2   = "alex@linearlite.dev"
)

// seed creates the demo tenant, two people to sign in as, the level roles,
// and a board's worth of items.
//
// Idempotent — every statement is an upsert or a lookup — so `linearlite seed`
// twice is not an error. The passwords go through the account service, outside
// the transaction, because a hash is not a field somebody sets: it is the same
// argon2id, the same policy and the same auth-log entry the endpoints use.
//
// Two people rather than one, because the notification story needs them: a
// notification goes to an item's stakeholders minus whoever made the change,
// so with one account there is never anybody to tell.
func Seed(ctx context.Context, pool *pgxpool.Pool) error {
	tenantID := uuid.MustParse(SeedTenantID)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// allowed_email_domains stays NULL, deliberately: OnRegistered provisions
	// whoever signs up into this tenant, Provision honours the domain list,
	// and strangers arrive with addresses nobody can predict.
	if _, err := tx.Exec(ctx, `
		INSERT INTO rig_tenant (id, created_at, name, slug, is_active)
		VALUES ($1, now(), 'LinearLite', 'linearlite', true)
		ON CONFLICT (id) DO NOTHING`,
		tenantID); err != nil {
		return fmt.Errorf("tenant: %w", err)
	}

	demo, err := person(ctx, tx, tenantID, SeedEmail, "Demo", "Owner")
	if err != nil {
		return err
	}
	alex, err := person(ctx, tx, tenantID, SeedEmail2, "Alex", "Basic")
	if err != nil {
		return err
	}

	// The three level roles and their grants, then each person attached to the
	// role their level implies — the same calls tenant creation and the
	// registration hook make, so there is one policy however a tenant or an
	// account came to exist.
	keys := append(api.PermissionKeys(), authz.AuthKeys()...)
	if err := authz.SeedRoles(ctx, tx, tenantID, keys); err != nil {
		return err
	}
	if err := authz.AttachRole(ctx, tx, tenantID, demo, "Owner"); err != nil {
		return err
	}
	if err := authz.AttachRole(ctx, tx, tenantID, alex, "Basic"); err != nil {
		return err
	}

	if err := seedBoard(ctx, tx, tenantID, demo, alex); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}

	// Nothing is set up for either of them to sign in with, and there is
	// nothing to set up: they type their address, a code is mailed, and they
	// type it back.
	fmt.Printf("seeded tenant %s: sign in as %s (or %s) — ask for a code\n",
		SeedTenantID, SeedEmail, SeedEmail2)
	return nil
}

// person finds or creates an identity and its account in the tenant, and
// answers the account id — which is what roles attach to and todos point at.
func person(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, email, name, level string) (uuid.UUID, error) {
	identityID := uuid.New()
	err := tx.QueryRow(ctx, `
		SELECT id FROM rig_identity
		 WHERE lower(email_address) = lower($1) AND deleted_at IS NULL`,
		email).Scan(&identityID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if _, err := tx.Exec(ctx, `
			INSERT INTO rig_identity (id, created_at, email_address, display_name, is_active)
			VALUES ($1, now(), $2, $3, true)`,
			identityID, email, name); err != nil {
			return uuid.Nil, fmt.Errorf("identity %s: %w", email, err)
		}
	case err != nil:
		return uuid.Nil, fmt.Errorf("identity %s: %w", email, err)
	}

	accountID := uuid.New()
	err = tx.QueryRow(ctx, `
		SELECT id FROM rig_account
		 WHERE tenant_id = $1 AND identity_id = $2 AND deleted_at IS NULL`,
		tenantID, identityID).Scan(&accountID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if _, err := tx.Exec(ctx, `
			INSERT INTO rig_account (id, tenant_id, identity_id, created_at, email_address,
			                         display_name, role, time_zone, is_active)
			VALUES ($1, $2, $3, now(), $4, $5, $6, 'Europe/Stockholm', true)`,
			accountID, tenantID, identityID, email, name, level); err != nil {
			return uuid.Nil, fmt.Errorf("account %s: %w", email, err)
		}
	case err != nil:
		return uuid.Nil, fmt.Errorf("account %s: %w", email, err)
	}
	return accountID, nil
}

// seedBoard writes a board that already tells the example's story: items in
// every column, both people involved, and one in the trash for the restore
// demo.
//
// Plain INSERTs with fixed identifiers, so a re-run changes nothing. What is
// deliberately not seeded is edit history: snapshots are written by the
// generated writer as part of an update, not by a trigger, so history appears
// the moment somebody edits an item in the running application — which is a
// better demonstration than pre-faked rows anyway.
func seedBoard(ctx context.Context, tx pgx.Tx, tenantID, demo, alex uuid.UUID) error {
	items := []struct {
		n        int
		title    string
		desc     string
		status   string
		priority string
		creator  uuid.UUID
		assignee *uuid.UUID
		deleted  bool
	}{
		{1, "Sketch the board layout", "Columns for each status, cards draggable between them.", "done", "high", demo, &demo, false},
		{2, "Wire up live sync", "The board should update without a reload when anybody changes anything.", "done", "high", demo, &alex, false},
		{3, "Design the item detail panel", "Title, description, status, assignee, attachments, history.", "in_progress", "normal", demo, &alex, false},
		{4, "Import the backlog from the old tracker", "There is a CSV export; the import job under import/ reads it.", "in_progress", "normal", alex, &demo, false},
		{5, "Notification toasts", "A quiet toast when somebody else touches one of your items.", "todo", "normal", alex, &demo, false},
		{6, "Empty-column affordance", "A column with nothing in it should still look droppable.", "todo", "low", demo, nil, false},
		{7, "Write the README walkthrough", "Register, accept the invitation, drag a card, restore this very item.", "backlog", "normal", demo, nil, false},
		{8, "Dark mode", "The board is dark; the auth screens should match.", "backlog", "low", alex, nil, false},
		{9, "Spike: offline support", "Decided against for the demo — restore me from the trash to disagree.", "canceled", "low", demo, nil, true},
	}

	for _, it := range items {
		// Deterministic per seed slot, so a re-run hits the conflict and moves
		// on rather than growing the board.
		id := uuid.MustParse(fmt.Sprintf("00000000-0000-0000-0000-0000000001%02d", it.n))

		deletedAt, deletedBy := "NULL", "NULL"
		if it.deleted {
			deletedAt, deletedBy = "now()", "$7"
		}
		q := fmt.Sprintf(`
			INSERT INTO todo (id, tenant_id, title, description, status, priority,
			                  assignee_account_id, created_at, created_by_account_id,
			                  deleted_at, deleted_by_account_id)
			VALUES ($1, $2, $3, $4, $5, $6, $8, now() - interval '1 hour' * $9, $7, %s, %s)
			ON CONFLICT (id) DO NOTHING`, deletedAt, deletedBy)

		if _, err := tx.Exec(ctx, q,
			id, tenantID, it.title, it.desc, it.status, it.priority,
			it.creator, it.assignee, len(items)-it.n); err != nil {
			return fmt.Errorf("todo %q: %w", it.title, err)
		}
	}
	return nil
}
