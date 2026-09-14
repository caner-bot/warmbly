package warmup

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/warmbly/warmbly/internal/models"
	"github.com/warmbly/warmbly/internal/repository"
)

// anyPoolRepo answers the account-scoped read and panics on the pool-pinned
// one, which is the whole point: the gate must not need to know the pool.
type anyPoolRepo struct {
	repository.WarmupRepository

	health *models.WarmupParticipantHealth
}

func (r *anyPoolRepo) GetParticipantHealthForAccount(context.Context, uuid.UUID) (*models.WarmupParticipantHealth, error) {
	return r.health, nil
}

// A partner borrowed from the other tier is a member there, not in the
// sender's pool; gating it in the sender's pool rejected every one (#495).
func TestCanParticipateAnyPoolGatesTheMailboxWhereItIs(t *testing.T) {
	future := time.Now().Add(24 * time.Hour)
	cases := []struct {
		name   string
		health *models.WarmupParticipantHealth
		want   bool
		reason string
	}{
		{"a healthy free-pool mailbox passes for a premium sender", &models.WarmupParticipantHealth{PoolType: "free", HealthState: models.WarmupHealthHealthy}, true, ""},
		{"a throttled one passes with its reason", &models.WarmupParticipantHealth{PoolType: "premium", HealthState: models.WarmupHealthThrottled}, true, "throttled"},
		{"a blocked one is refused wherever it is", &models.WarmupParticipantHealth{PoolType: "free", HealthState: models.WarmupHealthBlocked, BlockedUntil: &future}, false, "blocked"},
		{"a mailbox in no pool is not a partner", nil, false, "not_in_pool"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewService(&anyPoolRepo{health: tc.health})
			ok, reason, xerr := s.CanParticipateAnyPool(context.Background(), uuid.New())
			if xerr != nil {
				t.Fatalf("unexpected error: %v", xerr)
			}
			if ok != tc.want || reason != tc.reason {
				t.Fatalf("got (%v, %q), want (%v, %q)", ok, reason, tc.want, tc.reason)
			}
		})
	}
}
