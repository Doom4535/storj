// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package repairer

import (
	"context"
	"time"

	"github.com/spacemonkeygo/monkit/v3"
	"github.com/zeebo/errs"
	"go.uber.org/zap"
	"golang.org/x/sync/semaphore"

	"storj.io/common/memory"
	"storj.io/common/sync2"
	"storj.io/storj/satellite/internalpb"
	"storj.io/storj/satellite/repair/irreparable"
	"storj.io/storj/satellite/repair/queue"
	"storj.io/storj/storage"
)

// Error is a standard error class for this package.
var (
	Error = errs.Class("repairer error")
	mon   = monkit.Package()
)

// Config contains configurable values for repairer.
type Config struct {
	MaxRepair                     int           `help:"maximum segments that can be repaired concurrently" releaseDefault:"5" devDefault:"1"`
	Interval                      time.Duration `help:"how frequently repairer should try and repair more data" releaseDefault:"5m0s" devDefault:"1m0s"`
	Timeout                       time.Duration `help:"time limit for uploading repaired pieces to new storage nodes" default:"5m0s"`
	DownloadTimeout               time.Duration `help:"time limit for downloading pieces from a node for repair" default:"5m0s"`
	TotalTimeout                  time.Duration `help:"time limit for an entire repair job, from queue pop to upload completion" default:"45m"`
	MaxBufferMem                  memory.Size   `help:"maximum buffer memory (in bytes) to be allocated for read buffers" default:"4M"`
	MaxExcessRateOptimalThreshold float64       `help:"ratio applied to the optimal threshold to calculate the excess of the maximum number of repaired pieces to upload" default:"0.05"`
	InMemoryRepair                bool          `help:"whether to download pieces for repair in memory (true) or download to disk (false)" default:"false"`
}

// Service contains the information needed to run the repair service
//
// architecture: Worker
type Service struct {
	log        *zap.Logger
	queue      queue.RepairQueue
	config     *Config
	JobLimiter *semaphore.Weighted
	Loop       *sync2.Cycle
	repairer   *SegmentRepairer
	irrDB      irreparable.DB
}

// NewService creates repairing service.
func NewService(log *zap.Logger, queue queue.RepairQueue, config *Config, repairer *SegmentRepairer, irrDB irreparable.DB) *Service {
	return &Service{
		log:        log,
		queue:      queue,
		config:     config,
		JobLimiter: semaphore.NewWeighted(int64(config.MaxRepair)),
		Loop:       sync2.NewCycle(config.Interval),
		repairer:   repairer,
		irrDB:      irrDB,
	}
}

// Close closes resources.
func (service *Service) Close() error { return nil }

// WaitForPendingRepairs waits for all ongoing repairs to complete.
//
// NB: this assumes that service.config.MaxRepair will never be changed once this Service instance
// is initialized. If that is not a valid assumption, we should keep a copy of its initial value to
// use here instead.
func (service *Service) WaitForPendingRepairs() {
	// Acquire and then release the entire capacity of the semaphore, ensuring that
	// it is completely empty (or, at least it was empty at some point).
	//
	// No error return is possible here; context.Background() can't be canceled
	_ = service.JobLimiter.Acquire(context.Background(), int64(service.config.MaxRepair))
	service.JobLimiter.Release(int64(service.config.MaxRepair))
}

// Run runs the repairer service.
func (service *Service) Run(ctx context.Context) (err error) {
	defer mon.Task()(&ctx)(&err)

	// Wait for all repairs to complete
	defer service.WaitForPendingRepairs()

	return service.Loop.Run(ctx, service.processWhileQueueHasItems)
}

// processWhileQueueHasItems keeps calling process() until the queue is empty or something
// else goes wrong in fetching from the queue.
func (service *Service) processWhileQueueHasItems(ctx context.Context) error {
	for {
		err := service.process(ctx)
		if err != nil {
			if storage.ErrEmptyQueue.Has(err) {
				return nil
			}
			service.log.Error("process", zap.Error(Error.Wrap(err)))
			return err
		}
	}
}

// process picks items from repair queue and spawns a repair worker.
func (service *Service) process(ctx context.Context) (err error) {
	defer mon.Task()(&ctx)(&err)

	// wait until we are allowed to spawn a new job
	if err := service.JobLimiter.Acquire(ctx, 1); err != nil {
		return err
	}

	// IMPORTANT: this timeout must be started before service.queue.Select(), in case
	// service.queue.Select() takes some non-negligible amount of time, so that we can depend on
	// repair jobs being given up within some set interval after the time in the 'attempted'
	// column in the queue table.
	//
	// This is the reason why we are using a semaphore in this somewhat awkward way instead of
	// using a simpler sync2.Limiter pattern. We don't want this timeout to include the waiting
	// time from the semaphore acquisition, but it _must_ include the queue fetch time. At the
	// same time, we don't want to do the queue pop in a separate goroutine, because we want to
	// return from service.Run when queue fetch fails.
	ctx, cancel := context.WithTimeout(ctx, service.config.TotalTimeout)

	seg, err := service.queue.Select(ctx)
	if err != nil {
		service.JobLimiter.Release(1)
		cancel()
		return err
	}
	service.log.Debug("Retrieved segment from repair queue")

	// this goroutine inherits the JobLimiter semaphore acquisition and is now responsible
	// for releasing it.
	go func() {
		defer service.JobLimiter.Release(1)
		defer cancel()

		if err := service.worker(ctx, seg); err != nil {
			service.log.Error("repair worker failed:", zap.Error(err))
		}
	}()

	return nil
}

func (service *Service) worker(ctx context.Context, seg *internalpb.InjuredSegment) (err error) {
	defer mon.Task()(&ctx)(&err)

	workerStartTime := time.Now().UTC()

	service.log.Debug("Limiter running repair on segment")
	// note that shouldDelete is used even in the case where err is not null
	shouldDelete, err := service.repairer.Repair(ctx, string(seg.GetPath()))
	if shouldDelete {
		if irreparableErr, ok := err.(*irreparableError); ok {
			service.log.Error("segment could not be repaired! adding to irreparableDB for more attention",
				zap.Error(err))
			segmentInfo := &internalpb.IrreparableSegment{
				Path:               seg.GetPath(),
				SegmentDetail:      irreparableErr.segmentInfo,
				LostPieces:         irreparableErr.piecesRequired - irreparableErr.piecesAvailable,
				LastRepairAttempt:  time.Now().Unix(),
				RepairAttemptCount: int64(1),
			}
			if err := service.irrDB.IncrementRepairAttempts(ctx, segmentInfo); err != nil {
				service.log.Error("failed to add segment to irreparableDB! will leave in repair queue", zap.Error(err))
				shouldDelete = false
			}
		} else if err != nil {
			service.log.Error("unexpected error repairing segment!",
				zap.Error(err))
		} else {
			service.log.Debug("removing repaired segment from repair queue")
		}
		if shouldDelete {
			delErr := service.queue.Delete(ctx, seg)
			if delErr != nil {
				err = errs.Combine(err, Error.New("failed to remove segment from queue: %v", delErr))
			}
		}
	}
	if err != nil {
		return Error.Wrap(err)
	}

	repairedTime := time.Now().UTC()
	timeForRepair := repairedTime.Sub(workerStartTime)
	mon.FloatVal("time_for_repair").Observe(timeForRepair.Seconds()) //mon:locked

	insertedTime := seg.GetInsertedTime()
	// do not send metrics if segment was added before the InsertedTime field was added
	if !insertedTime.IsZero() {
		timeSinceQueued := workerStartTime.Sub(insertedTime)
		mon.FloatVal("time_since_checker_queue").Observe(timeSinceQueued.Seconds()) //mon:locked
	}

	return nil
}
