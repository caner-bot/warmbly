package tasks

import (
	"context"
	"testing"

	"github.com/google/uuid"

	warmupapp "github.com/warmbly/warmbly/internal/app/warmup"
	"github.com/warmbly/warmbly/internal/models"
	"github.com/warmbly/warmbly/internal/pkg/encrypt"
	"github.com/warmbly/warmbly/internal/repository"
)

// Issue #495: a thin tier is meant to borrow the other tier's proven
// mailboxes, but every chosen partner was gated against the sender's own pool,
// where a borrowed one is not a member, so the fallback never produced a
// partner. The sender here is alone in the free pool, so the only candidate is
// the borrowed premium mailbox; before the fix selection failed outright.
//
//	WARMBLY_TEST_DB=postgres://warmbly:warmbly@localhost:15432/warmbly_i495?sslmode=disable \
//	  go test ./internal/tasks/ -run LiveWarmupThinTier -v
func TestLiveWarmupThinTierBorrowsAProvenPartnerFromTheOtherTier(t *testing.T) {
	handle := liveCampaignDB(t)
	pool := handle.Pool
	ctx := context.Background()

	var occupied int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM warmup_pool_participants WHERE pool_id = $1`, models.WarmupPoolFreeID).Scan(&occupied); err != nil {
		t.Fatalf("count free pool: %v", err)
	}
	if occupied != 0 {
		t.Skip("free pool already has participants; the sender must be alone in it")
	}

	user, org, sender, borrowed := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("fixture %q: %v", sql[:min(60, len(sql))], err)
		}
	}
	exec(`INSERT INTO users (id, email, first_name, last_name) VALUES ($1, $2, 'Borrow', 'Test')`, user, "borrow-"+user.String()[:8]+"@test.local")
	exec(`INSERT INTO organizations (id, name, slug, owner_user_id) VALUES ($1, 'Borrow Test', $2, $3)`, org, "borrow-"+org.String()[:8], user)
	for _, m := range []struct {
		id   uuid.UUID
		tier string
	}{{sender, "free"}, {borrowed, "premium"}} {
		exec(`INSERT INTO email_accounts (id, user_id, organization_id, email, name, signature_plain, signature_html,
		          provider, status, campaign_limit, min_wait_time, timezone, warmup_pool_type)
		      VALUES ($1, $2, $3, $4, 'Borrow', '', '', 'smtp_imap', 'active', 50, 600, 'UTC', $5)`,
			m.id, user, org, "borrow-"+m.id.String()[:8]+"@test.local", m.tier)
	}
	exec(`INSERT INTO warmup_pool_participants (pool_id, email_account_id, participant_role, health_state)
	      VALUES ($1, $2, 'sender_receiver', 'healthy')`, models.WarmupPoolFreeID, sender)
	// Proven: healthy, unblocked, and older than the fallback's minimum age.
	exec(`INSERT INTO warmup_pool_participants (pool_id, email_account_id, participant_role, health_state, joined_at)
	      VALUES ($1, $2, 'sender_receiver', 'healthy', NOW() - INTERVAL '4 days')`, models.WarmupPoolPremiumID, borrowed)
	t.Cleanup(func() {
		c := context.Background()
		for _, s := range []struct {
			sql string
			arg any
		}{
			{`DELETE FROM warmup_pool_participants WHERE email_account_id IN (SELECT id FROM email_accounts WHERE organization_id = $1)`, org},
			{`DELETE FROM email_accounts WHERE organization_id = $1`, org},
			{`DELETE FROM organizations WHERE id = $1`, org},
			{`DELETE FROM users WHERE id = $1`, user},
		} {
			if _, err := pool.Exec(c, s.sql, s.arg); err != nil {
				t.Errorf("cleanup %q: %v", s.sql, err)
			}
		}
	})

	enc, err := encrypt.NewEncrypter([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("encrypter: %v", err)
	}
	warmupRepo := repository.NewWarmupRepository(pool)
	svc := &tasksService{
		warmupRepo:   warmupRepo,
		emailRepo:    repository.NewEmailRepostory(handle, enc),
		warmupHealth: warmupapp.NewService(warmupRepo), // the gate only runs when this is wired
	}

	partner, err := svc.selectWarmupPartner(ctx, models.Email{ID: sender, OrganizationID: &org, WarmupPoolType: "free"})
	if err != nil {
		t.Fatalf("a thin free tier with one proven premium mailbox produced no partner: %v", err)
	}
	if partner.ID != borrowed {
		t.Fatalf("selected %s, want the borrowed premium mailbox %s", partner.ID, borrowed)
	}
}
