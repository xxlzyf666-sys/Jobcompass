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
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				if a.provider.Ready() && a.processOne(ctx) {
					continue
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
	if job, ok := a.jobs[id]; ok {
		job.cancel()
	}
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
