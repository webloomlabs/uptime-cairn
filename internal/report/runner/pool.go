package runner

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// The bounded worker pool.
//
// The exit condition for this phase is an agency sending fifty branded client
// reports on the first of the month without touching anything, and the gate on
// it is that **fifty PDFs at 09:00 must not delay a single check**. Two things
// buy that, and neither is subtle:
//
//   - A hard concurrency limit. Reports are CPU-bound in a way checks are not —
//     a PDF is layout plus compression — so an unbounded burst is the one
//     workload in this system that can starve the scheduler on a small box.
//   - A bounded queue with a refusal. A queue that grows without limit turns a
//     backlog into memory pressure and then into an OOM kill, which takes the
//     monitoring down with the reporting. Refusing the fifty-first submission
//     with a reason is a worse morning for one report and a better one for
//     everything else.
//
// Nothing here touches the check path: the pool owns its own goroutines and the
// only thing it shares with ingest is the database, which is where the load test
// looks.

// ErrBusy means the queue is full. It is a refusal rather than a failure: the
// caller is a request handler that should say so with a 503 and a retry hint.
var ErrBusy = errors.New("report queue is full")

// DefaultWorkers is deliberately small. Rendering is CPU work on a box whose
// first duty is running checks, and the difference between two workers and
// twenty is measured in check latency rather than in report throughput — fifty
// reports at two at a time is still a few minutes, and nobody is watching.
const DefaultWorkers = 2

// DefaultQueue bounds the backlog. Larger than any realistic monthly burst, so
// the refusal is a symptom of something wrong rather than of a busy morning.
const DefaultQueue = 256

// Pool runs queued reports on a fixed number of workers.
type Pool struct {
	runner  *Runner
	queue   chan model.ReportRun
	log     *slog.Logger
	workers int

	// now is injectable so a test can pin the clock. Production passes nil and
	// gets time.Now, which is the only place in the reporting subsystem that
	// reads it — everything downstream takes the instant as a parameter, which
	// is what makes a rendered artifact reproducible.
	now func() time.Time

	wg   sync.WaitGroup
	once sync.Once
}

// NewPool builds a pool. Zero or negative sizes take the defaults rather than
// producing a pool that silently never runs anything.
func NewPool(r *Runner, workers, queueSize int, log *slog.Logger) *Pool {
	if workers <= 0 {
		workers = DefaultWorkers
	}
	if queueSize <= 0 {
		queueSize = DefaultQueue
	}
	if log == nil {
		log = slog.Default()
	}
	return &Pool{
		runner:  r,
		queue:   make(chan model.ReportRun, queueSize),
		log:     log,
		workers: workers,
	}
}

// Start launches the workers and returns. They stop when ctx is cancelled.
func (p *Pool) Start(ctx context.Context) {
	p.once.Do(func() {
		for i := range p.workers {
			p.wg.Add(1)
			go p.work(ctx, i)
		}
	})
}

// Wait blocks until every worker has stopped, which is what a graceful shutdown
// needs: a run interrupted between writing a file and committing its row would
// otherwise leave an orphan on every restart rather than only on a crash.
func (p *Pool) Wait() { p.wg.Wait() }

// Submit queues a run without blocking.
//
// Without blocking, deliberately. The caller is an HTTP handler answering a
// request that the spec says returns 202 — a handler that waits for a worker
// turns "queued" into "rendered while you wait", which is a different contract
// and a much worse one at fifty reports.
func (p *Pool) Submit(run model.ReportRun) error {
	select {
	case p.queue <- run:
		return nil
	default:
		return ErrBusy
	}
}

// Depth is the current backlog, for the operator-facing count.
func (p *Pool) Depth() int { return len(p.queue) }

func (p *Pool) work(ctx context.Context, worker int) {
	defer p.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case run, ok := <-p.queue:
			if !ok {
				return
			}
			p.execute(ctx, run, worker)
		}
	}
}

func (p *Pool) execute(ctx context.Context, run model.ReportRun, worker int) {
	started := p.clock()

	if err := p.runner.Execute(ctx, run, started); err != nil {
		// Two different events wearing one error type. A run another worker
		// claimed is the pool working; anything else is a run that could not be
		// finished, and the row is left as it stands rather than guessed at —
		// the recovery pass that picks up runs stuck in `running` is a scheduler
		// concern, not this one.
		p.log.Warn("report run did not complete",
			"run_id", run.ID.String(), "worker", worker, "error", err)
		return
	}
	p.log.Info("report run finished",
		"run_id", run.ID.String(), "worker", worker,
		"duration", p.clock().Sub(started).Round(time.Millisecond))
}

func (p *Pool) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now().UTC().Truncate(time.Millisecond)
}
