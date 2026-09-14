package tasks

import (
	"context"
	"testing"

	"github.com/google/uuid"

	warmupapp "github.com/warmbly/warmbly/internal/app/warmup"
	"github.com/warmbly/warmbly/internal/errx"
	"github.com/warmbly/warmbly/internal/models"
	"github.com/warmbly/warmbly/internal/repository"
)

type directedTaskRepo struct {
	repository.TaskRepository

	target uuid.UUID
}

func (r *directedTaskRepo) GetWarmupTask(context.Context, uuid.UUID) (*repository.WarmupTask, error) {
	return &repository.WarmupTask{TargetAccountID: &r.target}, nil
}

type directedEmailRepo struct{ repository.EmailRepository }

func (directedEmailRepo) GetByID(_ context.Context, id uuid.UUID) (*models.Email, *errx.Error) {
	return &models.Email{ID: id}, nil
}

// anyPoolGate passes the account-scoped gate and fails the test on the
// pool-pinned one: a reply-back target borrowed from the other tier is not in
// the sender's pool, and gating it there discarded every such reply (#495).
type anyPoolGate struct {
	warmupapp.Service

	t     *testing.T
	gated []uuid.UUID
}

func (g *anyPoolGate) CanParticipateAnyPool(_ context.Context, id uuid.UUID) (bool, string, *errx.Error) {
	g.gated = append(g.gated, id)
	return true, "", nil
}

func (g *anyPoolGate) CanParticipate(_ context.Context, id uuid.UUID, poolType string) (bool, string, *errx.Error) {
	g.t.Fatalf("directed target %s was gated against the sender's pool %q", id, poolType)
	return false, "", nil
}

func TestDirectedWarmupPartnerGatesTheTargetWhereItIs(t *testing.T) {
	target := uuid.New()
	gate := &anyPoolGate{t: t}
	s := &tasksService{
		taskRepo:     &directedTaskRepo{target: target},
		emailRepo:    directedEmailRepo{},
		warmupHealth: gate,
	}
	partner := s.directedWarmupPartner(context.Background(), uuid.New(), "premium")
	if partner == nil || partner.ID != target {
		t.Fatalf("directed partner = %v, want the target %s", partner, target)
	}
	if len(gate.gated) != 1 || gate.gated[0] != target {
		t.Fatalf("gated %v, want exactly the target once", gate.gated)
	}
}
