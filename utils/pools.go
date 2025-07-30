package utils

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)


// Job interface
type Job interface {
	Execute(ctx context.Context) error
	GetID() string
	GetPriority() int
}

// SimpleJob
type SimpleJob struct {
	ID       string
	Priority int
	Fn       func(ctx context.Context) error
}

func (j *SimpleJob) Execute(ctx context.Context) error {
	return j.Fn(ctx)
}

func (j *SimpleJob) GetID() string {
	return j.ID
}

func (j *SimpleJob) GetPriority() int {
	return j.Priority
}

// error
var (
	ErrPoolNotStarted = fmt.Errorf("协程池未启动")
    ErrPoolStopped    = fmt.Errorf("协程池已停止")
    ErrPoolFull       = fmt.Errorf("协程池队列已满")
    ErrSubmitTimeout  = fmt.Errorf("任务提交超时")
)

type Worker struct {
	id       int
	pool     *WorkerPool
	jobQueue chan Job
	logger   *Logger
}


type WorkerPool struct {
	maxWorkers int
	jobQueue   chan Job
	workers    []*Worker
	logger     *Logger
	wg         sync.WaitGroup
	ctx        context.Context
	cancel     context.CancelFunc

	// Statistics
	activeWorkers atomic.Int64
	completedJobs atomic.Int64
	failedJobs    atomic.Int64

	// control
	started bool
	mu      sync.RWMutex
}

type WorkerStats struct {
	ActiveWorkers int   `json:"active_workers"`
    QueuedJobs    int   `json:"queued_jobs"`
    CompletedJobs int64 `json:"completed_jobs"`
}

func NewWorkerPool(maxWorkers int, logger *Logger) *WorkerPool {
	if maxWorkers <= 0 {
		maxWorkers = runtime.NumCPU() * 2
	}

	return &WorkerPool{
		maxWorkers: maxWorkers,
		jobQueue:   make(chan Job, maxWorkers*2),
		workers:    make([]*Worker, 0, maxWorkers),
		logger:     logger,
	}
}

// Start pool
func (wp *WorkerPool) Start(ctx context.Context) {
	wp.mu.Lock()
	defer wp.mu.Unlock()

	if wp.started {
		return
	}

	wp.ctx, wp.cancel = context.WithCancel(ctx)		// 用外部 ctx 派生统一控制

	wp.logger.WithField("maxWorkers", wp.maxWorkers).Info("Starting worker pool")

	// NewWorker
	for i := 0; i < wp.maxWorkers; i++ {
		worker := &Worker{
			id:       i,
			pool:     wp,
			jobQueue: wp.jobQueue,
			logger:   wp.logger,
		}
		wp.workers = append(wp.workers, worker)
		wp.wg.Add(1)
		go worker.start(wp.ctx)
	}

	wp.started = true
}

// Stop pool
func (wp *WorkerPool) Stop() {
	wp.mu.Lock()
	defer wp.mu.Unlock()

	if !wp.started {
		return
	}

	wp.logger.Info("Stop")

	close(wp.jobQueue)
	wp.cancel()
	wp.wg.Wait()
	wp.started = false

	wp.logger.WithFields(map[string]interface{}{
		"completedJobs":	wp.completedJobs.Load(),
		"failedJobs":		wp.failedJobs.Load(),
	}).Info("The coroutine pool has stopped")
}

func (wp *WorkerPool) Submit(job Job) error {
	wp.mu.RLock()
    if !wp.started {
        wp.mu.RUnlock()
        return ErrPoolNotStarted
    }
    wp.mu.RUnlock()


	select {
	case wp.jobQueue <- job:
		return nil
	case <- wp.ctx.Done():
		return ErrPoolStopped
	default:
		return ErrPoolFull
	}
}

func (wp *WorkerPool) SubmitWithTimeout(job Job, timeout time.Duration) error {
	wp.mu.RLock()
	defer wp.mu.RUnlock()

	if !wp.started {
		return ErrPoolNotStarted
	}

	select {
	case wp.jobQueue <- job:
        return nil
    case <-time.After(timeout):
        return ErrSubmitTimeout
	case <-wp.ctx.Done():
		return ErrPoolStopped
	}
}

// Get statistics  获取统计信息
func (wp *WorkerPool) GetStats() WorkerStats {
	return WorkerStats {
		ActiveWorkers: 	int(wp.activeWorkers.Load()),
        QueuedJobs:    	len(wp.jobQueue),
        CompletedJobs: 	wp.completedJobs.Load(),
	}
}

func (w *Worker) start(ctx context.Context) {
	defer w.pool.wg.Done()

	w.logger.WithField("workerID", w.id).Debug("Start worker")

	for {
		select {
		case job, ok := <-w.jobQueue:
			if !ok {
				w.logger.WithField("workerID", w.id).
					Debug("The task queue is closed and the worker exits")
				return
			}

			w.processJob(ctx, job)
		
		case <-ctx.Done():
			w.logger.WithField("workerID", w.id).
				Debug("The context is cancelled and the worker exits")
			return
		}
	}
}

func (w *Worker) processJob(ctx context.Context, job Job) {
	w.pool.activeWorkers.Add(1)
	defer w.pool.activeWorkers.Add(-1)

	startTime := time.Now()

	w.logger.WithFields(map[string]interface{}{
		"workerID":	w.id,
		"jobID":	job.GetID(),
		"priority":	job.GetPriority(),
	}).Debug("Start Job")

	// Execute the task
	err := job.Execute(ctx)

	dur := time.Since(startTime)

	if err != nil {
		w.pool.failedJobs.Add(1)
		w.logger.WithFields(map[string]interface{}{
			"workerID": w.id,
			"jobID":	job.GetID(),
			"duration":	dur,
			"error":	err,
		}).Error("Job failed")
		return
	}

	w.pool.completedJobs.Add(1)
	 w.logger.WithFields(map[string]interface{}{
        "workerID": w.id,
        "jobID":    job.GetID(),
        "duration": dur,
    }).Debug("job completed")
}