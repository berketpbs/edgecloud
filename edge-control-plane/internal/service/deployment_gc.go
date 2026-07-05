package service

import (
	"context"
	"log"
	"time"
)

// deploymentRepoForGC is the subset of *repository.DeploymentRepository used
// by DeploymentGCService. Defined locally so tests are mockable.
type deploymentRepoForGC interface {
	DeleteOlderThan(ctx context.Context, retention time.Duration) (int64, error)
}

// DeploymentGCService periodically deletes old deployment rows that are not
// currently active. It follows the same pattern as LogGCService and
// WorkerGCService: immediate first sweep, then a ticker loop.
type DeploymentGCService struct {
	repo deploymentRepoForGC
}

func NewDeploymentGCService(repo deploymentRepoForGC) *DeploymentGCService {
	return &DeploymentGCService{repo: repo}
}

// Run blocks until ctx is cancelled. First sweep fires immediately.
func (s *DeploymentGCService) Run(ctx context.Context, interval, retention time.Duration) {
	if interval <= 0 || retention <= 0 {
		log.Printf("deployment_gc: invalid interval=%s retention=%s; refusing to run", interval, retention)
		return
	}

	runOnce := func() {
		if ctx.Err() != nil {
			return
		}
		deleted, err := s.repo.DeleteOlderThan(ctx, retention)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("deployment_gc: delete failed (retention=%s): %v", retention, err)
			return
		}
		if deleted > 0 {
			log.Printf("deployment_gc: deleted %d deployments older than %s", deleted, retention)
		}
	}

	runOnce()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce()
		}
	}
}
