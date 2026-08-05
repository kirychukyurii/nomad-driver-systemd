package task

import (
	"context"
	"sync"
	"time"

	"github.com/hashicorp/nomad/plugins/drivers"

	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/logx"
	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/logx/semconv"
	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/systemd"
)

// unitController is the set of systemd operations a Handler needs.
//
// Implementations must bound each call themselves: a Handler passes only its own
// lifetime context, with no deadline of its own.
type unitController interface {
	StopUnit(ctx context.Context, unit string) error
	KillUnit(ctx context.Context, unit string) error
	UnitState(ctx context.Context, unit string) (systemd.UnitState, error)
	UnitExitStatus(ctx context.Context, unit string) (systemd.ExitStatus, error)
	ResourceStats(ctx context.Context, unit string) (*systemd.ResourceStats, error)
	StreamLogs(unit string, logCh chan<- *systemd.LogEntry) error
}

// Handler tracks one task's systemd unit and exposes the task's state to the
// driver.
//
// The zero value is not usable: create one with [NewHandler], call
// [Handler.Start] to begin watching the unit, and [Handler.Stop] when the task is
// finished with. State accessors ([Handler.TaskStatus], [Handler.IsRunning],
// [Handler.HasExited], [Handler.WaitCh]) are safe to call from any goroutine at
// any time, including before Start and after Stop.
//
// A Handler observes the unit; it does not own it. Stopping the Handler leaves
// the unit running, and once the task is seen to have exited it stays exited even
// if the unit is restarted out of band, because a Nomad task cannot revive.
type Handler struct {
	// Task identification
	taskID string

	// Unit is the name of the systemd unit this task is backed by. It is set at
	// construction and never changes, so it is safe to read without
	// synchronization.
	Unit string

	// units performs the actual systemd operations for this task's unit.
	units unitController

	// logCh carries the unit's journal entries from the systemd reader to the
	// goroutine writing them out to the task's stdout and stderr.
	logCh chan *systemd.LogEntry

	// wakeCh signals that the unit changed and its state should be re-read. A
	// nil channel is valid and simply never fires.
	wakeCh <-chan struct{}

	// Task state
	handle      *drivers.TaskHandle
	state       drivers.TaskState
	startedAt   time.Time
	completedAt time.Time
	exitResult  *drivers.ExitResult

	// Synchronization. exitOnce guards close(waitCh), so it is closed exactly
	// once no matter which path observes the exit first.
	stateLock sync.RWMutex
	waitCh    chan struct{}
	exitOnce  sync.Once

	// wg tracks the goroutines Start spawned, so Stop can wait for them.
	//nolint:containedctx // @kirychukyurii: bounds the Handler's goroutines, not a per-call deadline; Stop cancels it.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	logger logx.Logger
}

// NewHandler returns a Handler for the unit backing taskID, in initialState.
//
// wakeCh, if non-nil, is used to notice unit changes promptly instead of waiting
// for the periodic re-check; nil is valid and falls back to that re-check alone.
// Passing [drivers.TaskStateExited] as initialState marks the task as already
// finished, so [Handler.WaitCh] is closed from the outset - a caller recovering an
// exited task should therefore set its exit result with [Handler.SetExitResult]
// before publishing the Handler to anything that waits on it.
func NewHandler(taskID, unit string, handle *drivers.TaskHandle, units unitController, wakeCh <-chan struct{}, initialState drivers.TaskState, logger logx.Logger) *Handler {
	ctx, cancel := context.WithCancel(context.Background())
	th := &Handler{
		taskID:    taskID,
		Unit:      unit,
		handle:    handle,
		units:     units,
		wakeCh:    wakeCh,
		logCh:     make(chan *systemd.LogEntry, logChannelBufferSize),
		state:     initialState,
		startedAt: time.Now(),
		waitCh:    make(chan struct{}),
		ctx:       ctx,
		cancel:    cancel,
		logger:    logger.With(semconv.TaskID(taskID), semconv.Unit(unit)),
	}

	if initialState == drivers.TaskStateExited {
		th.closeWaitCh()
	}

	return th
}

// Start begins monitoring the task: it watches the unit's state and copies the
// unit's journal into the task's stdout and stderr.
//
// It must be called at most once, and only while the unit is still under
// management. Journal streaming is started synchronously and inline, so that the
// unit cannot be unregistered between this call and the reader being registered;
// a failure to start it is logged rather than returned, since losing logs must
// not fail the task.
func (th *Handler) Start() {
	th.logger.Debug("starting task handler")

	if err := th.units.StreamLogs(th.Unit, th.logCh); err != nil {
		th.logger.Warn("can't start log streaming; this task will have no logs", logx.Err(err))
	}

	th.wg.Add(2)

	go func() {
		defer th.wg.Done()

		th.streamLogs()
	}()
	go func() {
		defer th.wg.Done()

		th.pollTaskState()
	}()
}

// Stop stops watching the unit and blocks until the Handler's goroutines have
// finished.
//
// Callers may afterwards tear down resources those goroutines used, such as the
// task's log FIFOs and its wake channel. Stop does not stop the unit itself, and
// leaves the task's last observed state readable. It must be called at most once,
// and after it returns [Handler.StopUnit] and [Handler.KillUnit] no longer work.
func (th *Handler) Stop() {
	th.logger.Debug("stopping task handler")
	th.cancel()
	th.wg.Wait()
}

// SetExitResult records the exit result and completion time of a task that had
// already finished before this Handler existed.
//
// It is meant for recovering an exited task and does not mark a running task as
// exited; a Handler that observes its unit stop determines the exit result itself.
func (th *Handler) SetExitResult(exitResult *drivers.ExitResult, completedAt time.Time) {
	th.stateLock.Lock()
	defer th.stateLock.Unlock()

	th.exitResult = exitResult
	th.completedAt = completedAt
}

// SetStartedAt overrides the task's recorded start time.
//
// A Handler defaults to the moment it was created, which is wrong for a task
// recovered after this process restarted: use this to report when the unit
// actually became active.
func (th *Handler) SetStartedAt(t time.Time) {
	th.stateLock.Lock()
	defer th.stateLock.Unlock()

	th.startedAt = t
}

// TaskStatus returns a snapshot of the task's current status.
//
// ExitResult is nil until the task is seen to have exited, and the snapshot is
// internally consistent: a status reporting [drivers.TaskStateExited] always
// carries the task's exit result.
func (th *Handler) TaskStatus() *drivers.TaskStatus {
	th.stateLock.RLock()
	defer th.stateLock.RUnlock()

	return &drivers.TaskStatus{
		ID:          th.taskID,
		Name:        th.Unit,
		State:       th.state,
		StartedAt:   th.startedAt,
		CompletedAt: th.completedAt,
		ExitResult:  th.exitResult,
		DriverAttributes: map[string]string{
			"unit": th.Unit,
		},
	}
}

// IsRunning reports whether the task is currently running.
//
// This is not the negation of [Handler.HasExited]: a unit in a transitional
// systemd state is neither running nor exited, so both report false. Callers
// deciding whether a task may be torn down want HasExited.
func (th *Handler) IsRunning() bool {
	th.stateLock.RLock()
	defer th.stateLock.RUnlock()

	return th.state == drivers.TaskStateRunning
}

// HasExited reports whether the task has finished.
//
// This is the question teardown paths need: unlike the negation of
// [Handler.IsRunning], it is false while the unit is still activating or
// deactivating, so a task that is merely between states is not mistaken for one
// that is done.
func (th *Handler) HasExited() bool {
	th.stateLock.RLock()
	defer th.stateLock.RUnlock()

	return th.state == drivers.TaskStateExited
}

// WaitCh returns a channel that is closed once the task has exited.
//
// By the time it closes, [Handler.TaskStatus] already reports the exit result, so
// a waiter can read it without further synchronization. The channel is closed
// exactly once and is never sent on; for a task that was already exited at
// construction it is closed from the outset.
func (th *Handler) WaitCh() <-chan struct{} {
	return th.waitCh
}

// StopUnit asks systemd to stop the unit and blocks until the stop job finishes.
//
// This can legitimately take as long as the unit's own TimeoutStopSec, 90 seconds
// by default, so callers that must not wait that long should run it concurrently
// and impose their own deadline rather than expect it to return quickly. A nil
// error means the unit is down; on error the caller should escalate to
// [Handler.KillUnit]. Note that a successful stop does not immediately flip the
// task to exited - that follows once the Handler observes the unit.
func (th *Handler) StopUnit() error {
	th.logger.Info("stopping unit")

	return th.units.StopUnit(th.ctx, th.Unit)
}

// KillUnit sends SIGKILL to every process of the unit.
//
// It is the escalation for a [Handler.StopUnit] that did not finish in time. A
// nil error means the signal was delivered, not that the unit has stopped, and an
// error may simply mean there was nothing left to kill - so callers should
// confirm against the unit's state rather than treat an error as failure.
func (th *Handler) KillUnit() error {
	th.logger.Warn("force killing unit")

	return th.units.KillUnit(th.ctx, th.Unit)
}

// UnitState returns the unit's current systemd state.
//
// This reflects the unit itself, not the task state the Handler has published;
// use it to confirm what systemd believes after an operation whose result was
// inconclusive.
func (th *Handler) UnitState() (systemd.UnitState, error) {
	return th.units.UnitState(th.ctx, th.Unit)
}

// ResourceStats samples the unit's current CPU and memory usage. It reports the
// same values and the same errors as the underlying systemd client.
func (th *Handler) ResourceStats(ctx context.Context) (*systemd.ResourceStats, error) {
	return th.units.ResourceStats(ctx, th.Unit)
}

// closeWaitCh closes waitCh, at most once however many callers reach it.
func (th *Handler) closeWaitCh() {
	th.exitOnce.Do(func() { close(th.waitCh) })
}
