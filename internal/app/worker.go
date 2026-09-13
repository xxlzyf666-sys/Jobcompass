package app

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

func (a *App) startWorkers(ctx context.Context) *sync.WaitGroup {
	wg := &sync.WaitGroup{}
	for i := 0; i < a.config.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			preparationFirst := false
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				if a.provider.Ready() {
					// Both queues share the same worker budget and alternate priority.
					preparationFirst = !preparationFirst
					if preparationFirst {
						if a.processPreparation(ctx) || a.processOne(ctx) {
							continue
						}
					} else if a.processOne(ctx) || a.processPreparation(ctx) {
						continue
					}
				}
				select {
				case <-ctx.Done():
					return
				case <-a.wake:
				case <-ticker.C:
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				now := time.Now()
				if err := a.store.RecoverLeases(ctx, now); err != nil && ctx.Err() == nil {
					slog.Error("task lease recovery failed")
				}
				a.cancelExpired(now)
				if err := a.store.Cleanup(ctx, now); err != nil && ctx.Err() == nil {
					slog.Error("data cleanup failed")
				}
			}
		}
	}()
	return wg
}

func (a *App) processOne(ctx context.Context) bool {
	job, err := a.store.Claim(ctx, time.Now(), a.config.RequestTimeout+30*time.Second)
	if err != nil {
		if job != nil {
			_ = a.store.Fail(ctx, job, "材料读取失败，请重新提交。", false, Usage{}, time.Now())
		}
		if ctx.Err() == nil {
			slog.Error("task claim failed")
		}
		return false
	}
	if job == nil {
		return false
	}
	requestCtx, cancel := context.WithTimeout(ctx, a.config.RequestTimeout)
	a.jobsMu.Lock()
	a.jobs[job.ID] = runningJob{cancel: cancel, expires: job.ExpiresAt}
	a.jobsMu.Unlock()
	defer func() { cancel(); a.jobsMu.Lock(); delete(a.jobs, job.ID); a.jobsMu.Unlock() }()
	// Recheck after registration so deletion between claim and registration is respected.
	var stillExists int
	if err = a.store.db.QueryRowContext(requestCtx, "SELECT COUNT(*) FROM diagnoses WHERE id=? AND status='running' AND worker_token=? AND expires_at>?", job.ID, job.WorkerToken, time.Now().Unix()).Scan(&stillExists); err != nil || stillExists == 0 {
		return true
	}
	report, usage, err := a.provider.Diagnose(requestCtx, job.Input)
	if ctx.Err() != nil {
		return false
	} // Leave the lease for restart recovery.
	if err == nil {
		err = validateReport(&report, job.Input)
	}
	if err != nil {
		message, kind, retryable := providerFailure(err)
		if saveErr := a.store.Fail(ctx, job, message, retryable, usage, time.Now()); saveErr != nil {
			slog.Error("task failure write failed")
		}
		slog.Warn("diagnosis attempt failed", "kind", kind, "attempt", job.Attempts)
		return true
	}
	if err = a.store.Complete(ctx, job, report, usage, time.Now()); err != nil {
		slog.Error("task result write failed")
	}
	return true
}

func (a *App) cancelJob(id string) {
	a.jobsMu.Lock()
	defer a.jobsMu.Unlock()
	for key, job := range a.jobs {
		if key == id || job.parent == id {
			job.cancel()
		}
	}
}

func (a *App) processPreparation(ctx context.Context) bool {
	provider, ok := a.provider.(PreparationProvider)
	if !ok {
		return false
	}
	task, err := a.store.ClaimPreparation(ctx, time.Now(), a.config.RequestTimeout+30*time.Second)
	if err != nil {
		if task != nil {
			_ = a.store.FailPreparation(ctx, task, "材料读取失败，请重新开始。", false, Usage{}, time.Now())
		}
		if ctx.Err() == nil {
			slog.Error("preparation task claim failed")
		}
		return false
	}
	if task == nil {
		return false
	}
	requestCtx, cancel := context.WithTimeout(ctx, a.config.RequestTimeout)
	a.jobsMu.Lock()
	a.jobs[task.ID] = runningJob{cancel: cancel, expires: task.ExpiresAt, parent: task.Parent}
	a.jobsMu.Unlock()
	defer func() { cancel(); a.jobsMu.Lock(); delete(a.jobs, task.ID); a.jobsMu.Unlock() }()
	var exists int
	if err = a.store.db.QueryRowContext(requestCtx, `SELECT COUNT(*) FROM preparation_tasks t JOIN diagnoses d ON d.id=t.diagnosis_id WHERE t.id=? AND t.status='running' AND t.worker_token=? AND d.expires_at>?`, task.ID, task.Token, time.Now().Unix()).Scan(&exists); err != nil || exists == 0 {
		return true
	}
	output, usage, err := provider.Prepare(requestCtx, task.Input)
	if ctx.Err() != nil {
		return false
	}
	if err == nil {
		err = validatePreparationOutput(&output, task.Input)
	}
	if err == nil {
		err = a.store.CompletePreparation(ctx, task, output, usage, time.Now())
	}
	if err != nil {
		message, kind, retry := providerFailure(err)
		if saveErr := a.store.FailPreparation(ctx, task, message, retry, usage, time.Now()); saveErr != nil {
			slog.Error("preparation failure write failed")
		}
		slog.Warn("preparation attempt failed", "kind", kind, "attempt", task.Attempts)
	}
	return true
}
func (a *App) cancelExpired(now time.Time) {
	a.jobsMu.Lock()
	defer a.jobsMu.Unlock()
	for _, job := range a.jobs {
		if job.expires <= now.Unix() {
			job.cancel()
		}
	}
}
