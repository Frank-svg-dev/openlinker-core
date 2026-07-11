package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/rs/zerolog/log"
)

const (
	defaultRuntimeV2MaintenanceInterval    = time.Second
	defaultRuntimeV2MaintenanceBatchSize   = 128
	defaultRuntimeV2MaintenanceCatchUpRuns = 4
)

type runtimeV2DeadlineReconcileWorker interface {
	ReconcileBatch(context.Context, int) (RuntimeV2ReconcileBatchResult, error)
}

type runtimeV2CancellationReapWorker interface {
	ReapExpiredCancellations(context.Context, int) (int, error)
}

// RuntimeV2MaintenanceWorkerConfig bounds every tick so a large stale queue
// cannot monopolize a Core process. A full batch triggers a small, bounded
// catch-up loop; the next tick continues any remaining work.
type RuntimeV2MaintenanceWorkerConfig struct {
	Interval              time.Duration
	ReconcileBatchSize    int
	CancellationBatchSize int
	MaxCatchUpBatches     int
}

type RuntimeV2MaintenanceResult struct {
	ReconcileBatches    int
	CancellationBatches int
	Reconciled          int
	Requeued            int
	TimedOut            int
	DeadLettered        int
	CancellationsReaped int
}

func normalizeRuntimeV2MaintenanceWorkerConfig(cfg RuntimeV2MaintenanceWorkerConfig) RuntimeV2MaintenanceWorkerConfig {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultRuntimeV2MaintenanceInterval
	}
	if cfg.ReconcileBatchSize <= 0 || cfg.ReconcileBatchSize > maxRuntimeV2ReconcileBatch {
		cfg.ReconcileBatchSize = defaultRuntimeV2MaintenanceBatchSize
	}
	if cfg.CancellationBatchSize <= 0 || cfg.CancellationBatchSize > maxRuntimeCancellationReapBatch {
		cfg.CancellationBatchSize = defaultRuntimeV2MaintenanceBatchSize
	}
	if cfg.MaxCatchUpBatches <= 0 || cfg.MaxCatchUpBatches > 32 {
		cfg.MaxCatchUpBatches = defaultRuntimeV2MaintenanceCatchUpRuns
	}
	return cfg
}

// RunRuntimeV2MaintenanceOnce executes lease/deadline reconciliation and
// cancellation deadline recovery independently. An error in one path does not
// suppress the other path; errors are joined after both bounded passes finish.
func RunRuntimeV2MaintenanceOnce(
	ctx context.Context,
	reconciler runtimeV2DeadlineReconcileWorker,
	cancellations runtimeV2CancellationReapWorker,
	cfg RuntimeV2MaintenanceWorkerConfig,
) (RuntimeV2MaintenanceResult, error) {
	cfg = normalizeRuntimeV2MaintenanceWorkerConfig(cfg)
	var result RuntimeV2MaintenanceResult
	var errs []error

	if reconciler == nil {
		errs = append(errs, ErrRuntimeV2ReconcilerNotConfigured)
	} else {
		for batch := 0; batch < cfg.MaxCatchUpBatches; batch++ {
			if err := ctx.Err(); err != nil {
				errs = append(errs, err)
				break
			}
			batchResult, err := reconciler.ReconcileBatch(ctx, cfg.ReconcileBatchSize)
			result.ReconcileBatches++
			result.Reconciled += batchResult.Reconciled
			result.Requeued += batchResult.Requeued
			result.TimedOut += batchResult.TimedOut
			result.DeadLettered += batchResult.DeadLettered
			if err != nil {
				errs = append(errs, err)
				break
			}
			if batchResult.Scanned < cfg.ReconcileBatchSize {
				break
			}
		}
	}

	if cancellations == nil {
		errs = append(errs, errRuntimeCancellationNotReady)
	} else {
		for batch := 0; batch < cfg.MaxCatchUpBatches; batch++ {
			if err := ctx.Err(); err != nil {
				errs = append(errs, err)
				break
			}
			reaped, err := cancellations.ReapExpiredCancellations(ctx, cfg.CancellationBatchSize)
			result.CancellationBatches++
			result.CancellationsReaped += reaped
			if err != nil {
				errs = append(errs, err)
				break
			}
			if reaped < cfg.CancellationBatchSize {
				break
			}
		}
	}

	return result, errors.Join(errs...)
}

// StartRuntimeV2MaintenanceWorker runs an immediate pass and then continues
// until shutdown. PostgreSQL remains the truth source; failures are logged and
// retried on the next tick instead of terminating the API process.
func StartRuntimeV2MaintenanceWorker(
	ctx context.Context,
	reconciler runtimeV2DeadlineReconcileWorker,
	cancellations runtimeV2CancellationReapWorker,
	cfg RuntimeV2MaintenanceWorkerConfig,
) {
	cfg = normalizeRuntimeV2MaintenanceWorkerConfig(cfg)
	run := func() {
		result, err := RunRuntimeV2MaintenanceOnce(ctx, reconciler, cancellations, cfg)
		if err != nil && ctx.Err() == nil {
			log.Error().Err(err).Msg("runtime v2 maintenance pass failed")
			return
		}
		if result.Reconciled > 0 || result.CancellationsReaped > 0 {
			log.Info().
				Int("reconciled", result.Reconciled).
				Int("requeued", result.Requeued).
				Int("timed_out", result.TimedOut).
				Int("dead_lettered", result.DeadLettered).
				Int("cancellations_reaped", result.CancellationsReaped).
				Msg("runtime v2 maintenance pass committed")
		}
	}

	run()
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}
