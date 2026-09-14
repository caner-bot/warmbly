package tasks

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	warmupapp "github.com/warmbly/warmbly/internal/app/warmup"
	"github.com/warmbly/warmbly/internal/config"
	"github.com/warmbly/warmbly/internal/models"
	"github.com/warmbly/warmbly/internal/pkg/encrypt"
	"github.com/warmbly/warmbly/internal/repository"
)

// Issue #495: the thin-tier fallback gated every borrowed partner against the
// sender's pool, where it is not a member, so a thin tier never borrowed. The
// rule is now one-directional and own-tier-first, and these pin all three
// halves against the real selector.
//
//	WARMBLY_TEST_DB=postgres://warmbly:warmbly@localhost:15432/warmbly_i495?sslmode=disable \
//	  go test ./internal/tasks/ -run LiveWarmupBorrow -v

type crossTierFixture struct {
	pool      *pgxpool.Pool
	svc       *tasksService
	user, org uuid.UUID
}

func newCrossTierFixture(t *testing.T) *crossTierFixture {
	t.Helper()
	handle := liveCampaignDB(t)
	pool := handle.Pool
	requireEmptyPool(t, pool, models.WarmupPoolFreeID)
	requireEmptyPool(t, pool, models.WarmupPoolPremiumID)

	f := &crossTierFixture{pool: pool, user: uuid.New(), org: uuid.New()}
	// Registered before the first row so a failed insert leaves nothing behind.
	t.Cleanup(func() {
		c := context.Background()
		for _, s := range []struct {
			sql string
			arg any
		}{
			{`DELETE FROM warmup_tokens WHERE sender_account_id IN (SELECT id FROM email_accounts WHERE organization_id = $1)`, f.org},
			{`DELETE FROM warmup_pool_participants WHERE email_account_id IN (SELECT id FROM email_accounts WHERE organization_id = $1)`, f.org},
			{`DELETE FROM email_accounts WHERE organization_id = $1`, f.org},
			{`DELETE FROM organizations WHERE id = $1`, f.org},
			{`DELETE FROM users WHERE id = $1`, f.user},
		} {
			if _, err := pool.Exec(c, s.sql, s.arg); err != nil {
				t.Errorf("cleanup %q: %v", s.sql, err)
			}
		}
	})
	f.exec(t, `INSERT INTO users (id, email, first_name, last_name) VALUES ($1, $2, 'Borrow', 'Test')`, f.user, "borrow-"+f.user.String()[:8]+"@test.local")
	f.exec(t, `INSERT INTO organizations (id, name, slug, owner_user_id) VALUES ($1, 'Borrow Test', $2, $3)`, f.org, "borrow-"+f.org.String()[:8], f.user)

	enc, err := encrypt.NewEncrypter([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("encrypter: %v", err)
	}
	warmupRepo := repository.NewWarmupRepository(pool)
	f.svc = &tasksService{
		warmupRepo:   warmupRepo,
		emailRepo:    repository.NewEmailRepostory(handle, enc),
		warmupHealth: warmupapp.NewService(warmupRepo), // the gate only runs when this is wired
	}
	return f
}

func (f *crossTierFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("fixture %q: %v", sql[:min(60, len(sql))], err)
	}
}

// member adds a healthy mailbox to a pool; provenDays backdates its join so
// it clears the fallback's minimum age.
func (f *crossTierFixture) member(t *testing.T, poolID uuid.UUID, provenDays int) uuid.UUID {
	t.Helper()
	id := uuid.New()
	f.exec(t, `INSERT INTO email_accounts (id, user_id, organization_id, email, name, signature_plain, signature_html,
	          provider, status, campaign_limit, min_wait_time, timezone)
	      VALUES ($1, $2, $3, $4, 'Borrow', '', '', 'smtp_imap', 'active', 50, 600, 'UTC')`,
		id, f.user, f.org, "borrow-"+id.String()[:8]+"@test.local")
	f.exec(t, `INSERT INTO warmup_pool_participants (pool_id, email_account_id, participant_role, health_state, joined_at)
	      VALUES ($1, $2, 'sender_receiver', 'healthy', NOW() - make_interval(days => $3))`, poolID, id, provenDays)
	return id
}

func (f *crossTierFixture) sender(id uuid.UUID, tier string) models.Email {
	return models.Email{ID: id, OrganizationID: &f.org, WarmupPoolType: tier}
}

func TestLiveWarmupBorrowThinPremiumTierBorrowsAProvenFreeMailbox(t *testing.T) {
	f := newCrossTierFixture(t)
	sender := f.member(t, models.WarmupPoolPremiumID, 0)
	borrowed := f.member(t, models.WarmupPoolFreeID, 4)

	partner, err := f.svc.selectWarmupPartner(context.Background(), f.sender(sender, "premium"))
	if err != nil {
		t.Fatalf("a thin premium tier with one proven free mailbox produced no partner: %v", err)
	}
	if partner.ID != borrowed {
		t.Fatalf("selected %s, want the borrowed free mailbox %s", partner.ID, borrowed)
	}
}

func TestLiveWarmupBorrowFreeTierNeverBorrowsPremium(t *testing.T) {
	f := newCrossTierFixture(t)
	sender := f.member(t, models.WarmupPoolFreeID, 0)
	f.member(t, models.WarmupPoolPremiumID, 4)

	if partner, err := f.svc.selectWarmupPartner(context.Background(), f.sender(sender, "free")); err == nil {
		t.Fatalf("a free sender was handed a premium mailbox %s; free traffic must not reach paying inboxes", partner.ID)
	}
}

func TestLiveWarmupBorrowPrefersAFreshOwnTierPartner(t *testing.T) {
	f := newCrossTierFixture(t)
	sender := f.member(t, models.WarmupPoolPremiumID, 0)
	own := f.member(t, models.WarmupPoolPremiumID, 0)
	f.member(t, models.WarmupPoolFreeID, 4)

	for i := 0; i < 20; i++ {
		partner, err := f.svc.selectWarmupPartner(context.Background(), f.sender(sender, "premium"))
		if err != nil {
			t.Fatalf("pick %d: %v", i, err)
		}
		if partner.ID != own {
			t.Fatalf("pick %d chose %s over the fresh own-tier partner %s", i, partner.ID, own)
		}
	}
}

// The floor counts the other mailboxes, as the scheduler's
// CountEligibleRecipients does: a premium tier of exactly the floor including
// the sender is one short and still borrows.
func TestLiveWarmupBorrowFloorCountsTheOtherMailboxes(t *testing.T) {
	f := newCrossTierFixture(t)
	sender := f.member(t, models.WarmupPoolPremiumID, 0)
	// Every own-tier partner was used today, so only a borrowed one can be picked.
	for i := 1; i < config.WarmupPoolTierFallbackFloor; i++ {
		own := f.member(t, models.WarmupPoolPremiumID, 0)
		task := uuid.New()
		f.exec(t, `INSERT INTO tasks (id, task_type, email_account_id, status, message_id) VALUES ($1, 'warmup', $2, 'completed', '')`, task, sender)
		f.exec(t, `INSERT INTO warmup_tokens (task_id, sender_account_id, recipient_account_id) VALUES ($1, $2, $3)`, task, sender, own)
	}
	borrowed := f.member(t, models.WarmupPoolFreeID, 4)

	partner, err := f.svc.selectWarmupPartner(context.Background(), f.sender(sender, "premium"))
	if err != nil {
		t.Fatalf("a premium tier at the floor including its sender did not borrow: %v", err)
	}
	if partner.ID != borrowed {
		t.Fatalf("selected %s, want the borrowed free mailbox %s", partner.ID, borrowed)
	}
}
