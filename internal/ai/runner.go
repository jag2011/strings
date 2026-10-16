package ai

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jag2011/strings/internal/store"
)

// Ways a session ends other than the model stopping by itself.
var (
	errCancelled   = errors.New("cancelled")
	errCostLimit   = errors.New("cost limit reached")
	errStepLimit   = errors.New("step limit reached")
	errSessionFull = errors.New("session full")
)

// Runner works through the queued runs, one at a time, in one goroutine.
type Runner struct {
	// LangName returns the display name of a language code.
	LangName func(code string) string
	// SessionSteps is the number of model calls after which a session ends
	// and the next one starts with a fresh context. What was done is in the
	// database, so nothing is lost.
	SessionSteps int
	// MaxSteps stops a run after that many model calls in one go, in case
	// the cost isn't reported.
	MaxSteps int

	store *store.Store
	agent Agent
	wake  chan struct{}
	done  chan struct{}

	mu        sync.Mutex
	current   int64 // the running run, 0 if none
	cancelled bool  // the running run should stop
}

func NewRunner(s *store.Store, a Agent) *Runner {
	return &Runner{
		LangName:     func(code string) string { return code },
		SessionSteps: 16,
		MaxSteps:     400,
		store:        s,
		agent:        a,
		wake:         make(chan struct{}, 1),
		done:         make(chan struct{}),
	}
}

// Start marks runs left running by an earlier process as interrupted and
// starts the worker, which stops when ctx ends. Wait waits for it.
func (r *Runner) Start(ctx context.Context) error {
	n, err := r.store.InterruptAIRuns(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		log.Printf("ai: %d %s interrupted by the last shutdown", n, plural(n, "run", "runs"))
	}
	go r.loop(ctx)
	return nil
}

// Wait returns when the worker has stopped.
func (r *Runner) Wait() { <-r.done }

// Wake tells the worker that a run was queued.
func (r *Runner) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Cancel stops a run: a queued one right away, a running one after the
// current model call.
func (r *Runner) Cancel(ctx context.Context, id int64) error {
	r.mu.Lock()
	if r.current == id {
		r.cancelled = true
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()
	ok, err := r.store.CancelQueuedAIRun(ctx, id)
	if err == nil && ok {
		err = r.store.AddAIRunEvent(ctx, id, "cancelled before it started")
	}
	return err
}

// Restart queues a stopped run again.
func (r *Runner) Restart(ctx context.Context, id int64) error {
	if err := r.store.RestartAIRun(ctx, id); err != nil {
		return err
	}
	r.Wake()
	return r.store.AddAIRunEvent(ctx, id, "queued again")
}

func (r *Runner) cancelRequested() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelled
}

func (r *Runner) loop(ctx context.Context) {
	defer close(r.done)
	for {
		run, err := r.store.StartNextAIRun(ctx)
		if err == nil {
			r.mu.Lock()
			r.current, r.cancelled = run.ID, false
			r.mu.Unlock()
			r.do(ctx, run)
			r.mu.Lock()
			r.current = 0
			r.mu.Unlock()
			continue
		}
		if !errors.Is(err, store.ErrNotFound) && ctx.Err() == nil {
			log.Printf("ai: next run: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-time.After(time.Minute):
		}
	}
}

// do runs one run and records how it ended.
func (r *Runner) do(ctx context.Context, run *store.AIRun) {
	j := &job{run: run, st: r.store, langName: r.LangName(run.Lang), offered: map[string]bool{}, failed: map[string]bool{}}
	if set, err := r.store.Settings(ctx); err == nil {
		j.sourceName = r.LangName(set["source_lang"])
	}
	j.log = func(ctx context.Context, format string, args ...any) {
		// Logged even while shutting down, so the log shows the last steps.
		if err := r.store.AddAIRunEvent(context.WithoutCancel(ctx), run.ID, fmt.Sprintf(format, args...)); err != nil {
			log.Printf("ai: run %d: %v", run.ID, err)
		}
	}
	if run.Steps > 0 {
		j.log(ctx, "started again with %s", run.Model)
	} else {
		j.log(ctx, "started with %s", run.Model)
	}
	status, summary, msg := r.work(ctx, j)
	if ctx.Err() != nil && status == store.AIFailed {
		status, msg = store.AIInterrupted, "The server stopped during the run. Start it again to continue."
	}
	bg := context.WithoutCancel(ctx)
	pending, err := r.store.AIPending(bg, run.Lang, run.UserID, run.Input)
	if err == nil {
		j.log(bg, "%s; %d %s still to do", status, len(pending), plural(len(pending), "string", "strings"))
	}
	if err := r.store.FinishAIRun(bg, run.ID, status, summary, msg); err != nil {
		log.Printf("ai: run %d: %v", run.ID, err)
	}
}

// work runs sessions until nothing is left or something stops the run. It
// returns the final status, the agent's summary and an error message.
func (r *Runner) work(ctx context.Context, j *job) (status, summary, msg string) {
	run := j.run
	cost, steps := run.Cost, 0
	for session := 1; ; session++ {
		left, err := j.left(ctx)
		if err != nil {
			return store.AIFailed, summary, err.Error()
		}
		if len(left) == 0 {
			return store.AIDone, summary, ""
		}
		prompt := "Start the job."
		if session > 1 {
			prompt = "Continue the job. Earlier work is saved; get_job tells you what's left."
		}
		j.saved = 0
		sessionSteps := 0
		text, err := r.agent.Run(ctx, Session{
			Model:  run.Model,
			System: systemPrompt(j),
			Prompt: prompt,
			Tools:  j.tools(),
			BeforeStep: func(ctx context.Context) error {
				switch {
				case r.cancelRequested():
					return errCancelled
				case cost >= run.MaxCost:
					return errCostLimit
				case steps >= r.MaxSteps:
					return errStepLimit
				case sessionSteps >= r.SessionSteps:
					return errSessionFull
				}
				return ctx.Err()
			},
			AfterStep: func(ctx context.Context, u store.AIUsage) {
				steps++
				sessionSteps++
				cost += u.Cost
				if err := r.store.AddAIRunUsage(context.WithoutCancel(ctx), run.ID, u); err != nil {
					log.Printf("ai: run %d: %v", run.ID, err)
				}
			},
		})
		switch {
		case errors.Is(err, errSessionFull):
			continue
		case errors.Is(err, errCancelled):
			return store.AICancelled, summary, ""
		case errors.Is(err, errCostLimit):
			return store.AIDone, summary, fmt.Sprintf("Stopped at the cost limit of $%.2f. Start a new run to continue.", run.MaxCost)
		case errors.Is(err, errStepLimit):
			return store.AIDone, summary, fmt.Sprintf("Stopped after %d model calls. Start it again to continue.", steps)
		case err != nil:
			return store.AIFailed, summary, err.Error()
		}
		if text != "" {
			summary = text
		}
		// The model stopped by itself. If it saved something, a fresh
		// session goes on with what's left; if not, it has given up.
		if j.saved == 0 {
			return store.AIDone, summary, ""
		}
	}
}
