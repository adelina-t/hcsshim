package main

import (
	"context"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/Microsoft/hcsshim/internal/cow"
	"github.com/Microsoft/hcsshim/internal/guestrequest"
	"github.com/Microsoft/hcsshim/internal/hcsoci"
	"github.com/Microsoft/hcsshim/internal/signals"
	"github.com/Microsoft/hcsshim/internal/uvm"
	"github.com/Microsoft/hcsshim/osversion"
	eventstypes "github.com/containerd/containerd/api/events"
	containerd_v1_types "github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/runtime"
	"github.com/containerd/containerd/runtime/v2/task"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/windows"
)

const (
	// processStopTimeout is the amount of time after signaling the process with
	// a signal expected to kill the process that the exec must wait before
	// forcibly terminating the process.
	//
	// For example, sending a SIGKILL is expected to kill a process. If the
	// process does not stop within `processStopTimeout` we will forcibly
	// terminate the process without a signal.
	processStopTimeout = time.Second * 5
)

// newHcsExec creates an exec to track the lifetime of `spec` in `c` which is
// actually created on the call to `Start()`. If `id==tid` then this is the init
// exec and the exec will also start `c` on the call to `Start()` before execing
// the process `spec.Process`.
func newHcsExec(
	ctx context.Context,
	events publisher,
	tid string,
	host *uvm.UtilityVM,
	c cow.Container,
	id, bundle string,
	isWCOW bool,
	spec *specs.Process,
	io upstreamIO) shimExec {
	logrus.WithFields(logrus.Fields{
		"tid": tid,
		"eid": id,
	}).Debug("newHcsExec")

	he := &hcsExec{
		events:      events,
		tid:         tid,
		host:        host,
		c:           c,
		id:          id,
		bundle:      bundle,
		isWCOW:      isWCOW,
		spec:        spec,
		io:          io,
		processDone: make(chan struct{}),
		state:       shimExecStateCreated,
		exitStatus:  255, // By design for non-exited process status.
		exited:      make(chan struct{}),
	}
	go he.waitForContainerExit()
	return he
}

var _ = (shimExec)(&hcsExec{})

type hcsExec struct {
	events publisher
	// tid is the task id of the container hosting this process.
	//
	// This MUST be treated as read only in the lifetime of the exec.
	tid string
	// host is the hosting VM for `c`. If `host==nil` this exec MUST be a
	// process isolated WCOW exec.
	//
	// This MUST be treated as read only in the lifetime of the exec.
	host *uvm.UtilityVM
	// c is the hosting container for this exec.
	//
	// This MUST be treated as read only in the lifetime of the exec.
	c cow.Container
	// id is the id of this process.
	//
	// This MUST be treated as read only in the lifetime of the exec.
	id string
	// bundle is the on disk path to the folder containing the `process.json`
	// describing this process. If `id==tid` the process is described in the
	// `config.json`.
	//
	// This MUST be treated as read only in the lifetime of the exec.
	bundle string
	// isWCOW is set to `true` when this process is part of a Windows OCI spec.
	//
	// This MUST be treated as read only in the lifetime of the exec.
	isWCOW bool
	// spec is the OCI Process spec that was passed in at create time. This is
	// stored because we don't actually create the process until the call to
	// `Start`.
	//
	// This MUST be treated as read only in the lifetime of the exec.
	spec *specs.Process
	// io is the upstream io connections used for copying between the upstream
	// io and the downstream io. The upstream IO MUST already be connected at
	// create time in order to be valid.
	//
	// This MUST be treated as read only in the lifetime of the exec.
	io              upstreamIO
	processDone     chan struct{}
	processDoneOnce sync.Once

	// sl is the state lock that MUST be held to safely read/write any of the
	// following members.
	sl         sync.Mutex
	state      shimExecState
	pid        int
	exitStatus uint32
	exitedAt   time.Time
	p          *hcsoci.Cmd

	// exited is a wait block which waits async for the process to exit.
	exited     chan struct{}
	exitedOnce sync.Once
}

func (he *hcsExec) ID() string {
	return he.id
}

func (he *hcsExec) Pid() int {
	he.sl.Lock()
	defer he.sl.Unlock()
	return he.pid
}

func (he *hcsExec) State() shimExecState {
	he.sl.Lock()
	defer he.sl.Unlock()
	return he.state
}

func (he *hcsExec) Status() *task.StateResponse {
	he.sl.Lock()
	defer he.sl.Unlock()

	var s containerd_v1_types.Status
	switch he.state {
	case shimExecStateCreated:
		s = containerd_v1_types.StatusCreated
	case shimExecStateRunning:
		s = containerd_v1_types.StatusRunning
	case shimExecStateExited:
		s = containerd_v1_types.StatusStopped
	}

	return &task.StateResponse{
		ID:         he.tid,
		ExecID:     he.id,
		Bundle:     he.bundle,
		Pid:        uint32(he.pid),
		Status:     s,
		Stdin:      he.io.StdinPath(),
		Stdout:     he.io.StdoutPath(),
		Stderr:     he.io.StderrPath(),
		Terminal:   he.io.Terminal(),
		ExitStatus: he.exitStatus,
		ExitedAt:   he.exitedAt,
	}
}

func copyAndLog(w io.Writer, r io.Reader, e *logrus.Entry, msg string) {
	n, err := io.Copy(w, r)
	lvl := logrus.DebugLevel
	e = e.WithField("bytes", n)
	if err != nil {
		lvl = logrus.ErrorLevel
		e = e.WithError(err)
	}
	e.Log(lvl, msg)
}

func (he *hcsExec) Start(ctx context.Context) (err error) {
	logrus.WithFields(logrus.Fields{
		"tid": he.tid,
		"eid": he.id,
	}).Debug("hcsExec::Start")

	he.sl.Lock()
	defer he.sl.Unlock()
	if he.state != shimExecStateCreated {
		return newExecInvalidStateError(he.tid, he.id, he.state, "start")
	}
	defer func() {
		if err != nil {
			he.exitFromCreatedL(1)
		}
	}()
	if he.id == he.tid {
		// This is the init exec. We need to start the container itself
		err = he.c.Start()
		if err != nil {
			return err
		}
		defer func() {
			if err != nil {
				he.c.Terminate()
				he.c.Close()
			}
		}()
	}
	cmd := &hcsoci.Cmd{
		Host:   he.c,
		Stdin:  he.io.Stdin(),
		Stdout: he.io.Stdout(),
		Stderr: he.io.Stderr(),
		Log: logrus.WithFields(logrus.Fields{
			"tid": he.tid,
			"eid": he.id,
		}),
		CopyAfterExitTimeout: time.Second * 1,
	}
	if he.isWCOW || he.id != he.tid {
		// An init exec passes the process as part of the config. We only pass
		// the spec if this is a true exec.
		cmd.Spec = he.spec
	}
	err = cmd.Start()
	if err != nil {
		return err
	}
	he.p = cmd

	// Assign the PID and transition the state.
	he.pid = he.p.Process.Pid()
	he.state = shimExecStateRunning

	// Publish the task/exec start event. This MUST happen before waitForExit to
	// avoid publishing the exit previous to the start.
	if he.id != he.tid {
		he.events(
			runtime.TaskExecStartedEventTopic,
			&eventstypes.TaskExecStarted{
				ContainerID: he.tid,
				ExecID:      he.id,
				Pid:         uint32(he.pid),
			})
	} else {
		he.events(
			runtime.TaskStartEventTopic,
			&eventstypes.TaskStart{
				ContainerID: he.tid,
				Pid:         uint32(he.pid),
			})
	}

	// wait in the background for the exit.
	go he.waitForExit()
	return nil
}

func (he *hcsExec) Kill(ctx context.Context, signal uint32) error {
	logrus.WithFields(logrus.Fields{
		"tid":    he.tid,
		"eid":    he.id,
		"signal": signal,
	}).Debug("hcsExec::Kill")

	he.sl.Lock()
	defer he.sl.Unlock()
	switch he.state {
	case shimExecStateCreated:
		he.exitFromCreatedL(1)
		return nil
	case shimExecStateRunning:
		supported := false
		if osversion.Get().Build >= osversion.RS5 {
			supported = he.host == nil || he.host.SignalProcessSupported()
		}
		var options interface{}
		var err error
		if he.isWCOW {
			var opt *guestrequest.SignalProcessOptionsWCOW
			opt, err = signals.ValidateWCOW(int(signal), supported)
			if opt != nil {
				options = opt
			}
		} else {
			var opt *guestrequest.SignalProcessOptionsLCOW
			opt, err = signals.ValidateLCOW(int(signal), supported)
			if opt != nil {
				options = opt
			}
		}
		if err != nil {
			return errors.Wrapf(errdefs.ErrFailedPrecondition, "signal %d: %v", signal, err)
		}
		var delivered bool
		if supported && options != nil {
			delivered, err = he.p.Process.Signal(options)
		} else {
			// legacy path before signals support OR if WCOW with signals
			// support needs to issue a terminate.
			delivered, err = he.p.Process.Kill()
		}
		if err != nil {
			return err
		}
		if !delivered {
			return errors.Wrapf(errdefs.ErrNotFound, "exec: '%s' in task: '%s' not found", he.id, he.tid)
		}
		return nil
	case shimExecStateExited:
		return errors.Wrapf(errdefs.ErrNotFound, "exec: '%s' in task: '%s' not found", he.id, he.tid)
	default:
		return newExecInvalidStateError(he.tid, he.id, he.state, "kill")
	}
}

func (he *hcsExec) ResizePty(ctx context.Context, width, height uint32) error {
	logrus.WithFields(logrus.Fields{
		"tid":    he.tid,
		"eid":    he.id,
		"width":  width,
		"height": height,
	}).Debug("hcsExec::ResizePty")

	he.sl.Lock()
	defer he.sl.Unlock()
	if he.state != shimExecStateRunning {
		return newExecInvalidStateError(he.tid, he.id, he.state, "resizepty")
	}
	if !he.io.Terminal() {
		return errors.Wrapf(errdefs.ErrFailedPrecondition, "exec: '%s' in task: '%s' is not a tty", he.id, he.tid)
	}

	return he.p.Process.ResizeConsole(uint16(width), uint16(height))
}

func (he *hcsExec) CloseIO(ctx context.Context, stdin bool) error {
	logrus.WithFields(logrus.Fields{
		"tid":   he.tid,
		"eid":   he.id,
		"stdin": stdin,
	}).Debug("hcsExec::CloseIO")

	// If we have any upstream IO we close the upstream connection. This will
	// unblock the `io.Copy` in the `Start()` call which will signal
	// `he.p.CloseStdin()`. If `he.io.Stdin()` is already closed this is safe to
	// call multiple times.
	he.io.CloseStdin()
	return nil
}

func (he *hcsExec) Wait(ctx context.Context) *task.StateResponse {
	logrus.WithFields(logrus.Fields{
		"tid": he.tid,
		"eid": he.id,
	}).Debug("hcsExec::Wait")

	<-he.exited
	return he.Status()
}

func (he *hcsExec) ForceExit(status int) {
	he.sl.Lock()
	defer he.sl.Unlock()
	if he.state != shimExecStateExited {
		// Avoid logging the force if we already exited gracefully
		logrus.WithFields(logrus.Fields{
			"tid":    he.tid,
			"eid":    he.id,
			"status": status,
		}).Debug("hcsExec::ForceExit")
		switch he.state {
		case shimExecStateCreated:
			he.exitFromCreatedL(status)
		case shimExecStateRunning:
			// Kill the process to unblock `he.waitForExit`
			he.p.Process.Kill()
		}
	}
}

// exitFromCreatedL transitions the shim to the exited state from the created
// state. It is the callers responsibility to hold `he.sl` for the durration of
// this transition.
//
// This call is idempotent and will not affect any state if the shim is already
// in the `shimExecStateExited` state.
//
// To transition for a created state the following must be done:
//
// 1. Issue `he.processDoneCancel` to unblock the goroutine
// `he.waitForContainerExit()``.
//
// 2. Set `he.state`, `he.exitStatus` and `he.exitedAt` to the exited values.
//
// 3. Release any upstream IO resources that were never used in a copy.
//
// 4. Close `he.exited` channel to unblock any waiters who might have called
// `Create`/`Wait`/`Start` which is a valid pattern.
//
// We DO NOT send the async `TaskExit` event because we never would have sent
// the `TaskStart`/`TaskExecStarted` event.
func (he *hcsExec) exitFromCreatedL(status int) {
	if he.state != shimExecStateExited {
		// Unblock the container exit goroutine
		he.processDoneOnce.Do(func() { close(he.processDone) })
		// Transition this exec
		he.state = shimExecStateExited
		he.exitStatus = uint32(status)
		he.exitedAt = time.Now()
		// Release all upstream IO connections (if any)
		he.io.Close()
		// Free any waiters
		he.exitedOnce.Do(func() {
			close(he.exited)
		})
	}
}

// waitForExit waits for the `he.p` to exit. This MUST only be called after a
// successful call to `Create` and MUST not be called more than once.
//
// This MUST be called via a goroutine.
//
// In the case of an exit from a running process the following must be done:
//
// 1. Wait for `he.p` to exit.
//
// 2. Issue `he.processDoneCancel` to unblock the goroutine
// `he.waitForContainerExit()` (if still running). We do this early to avoid the
// container exit also attempting to kill the process. However this race
// condition is safe and handled.
//
// 3. Capture the process exit code and set `he.state`, `he.exitStatus` and
// `he.exitedAt` to the exited values.
//
// 4. Wait for all IO to complete and release any upstream IO connections.
//
// 5. Send the async `TaskExit` to upstream listeners of any events.
//
// 6. Close `he.exited` channel to unblock any waiters who might have called
// `Create`/`Wait`/`Start` which is a valid pattern.
func (he *hcsExec) waitForExit() {
	err := he.p.Process.Wait()
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"tid":           he.tid,
			"eid":           he.id,
			logrus.ErrorKey: err,
		}).Error("hcsExec::waitForExit - Failed process Wait")
	}

	// Issue the process cancellation to unblock the container wait as early as
	// possible.
	he.processDoneOnce.Do(func() { close(he.processDone) })

	code, err := he.p.Process.ExitCode()
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"tid":           he.tid,
			"eid":           he.id,
			logrus.ErrorKey: err,
		}).Error("hcsExec::waitForExit - Failed to get ExitCode")
	} else {
		logrus.WithFields(logrus.Fields{
			"tid":      he.tid,
			"eid":      he.id,
			"exitCode": code,
		}).Debug("hcsExec::waitForExit - Exited")
	}

	he.sl.Lock()
	he.state = shimExecStateExited
	he.exitStatus = uint32(code)
	he.exitedAt = time.Now()
	he.sl.Unlock()

	// Wait for all IO copies to complete and free the resources.
	he.p.Wait()
	he.io.Close()

	// Only send the `runtime.TaskExitEventTopic` notification if this is a true
	// exec. For the `init` exec this is handled in task teardown.
	if he.tid != he.id {
		// We had a valid process so send the exited notification.
		he.events(
			runtime.TaskExitEventTopic,
			&eventstypes.TaskExit{
				ContainerID: he.tid,
				ID:          he.id,
				Pid:         uint32(he.pid),
				ExitStatus:  he.exitStatus,
				ExitedAt:    he.exitedAt,
			})
	}

	// Free any waiters.
	he.exitedOnce.Do(func() {
		close(he.exited)
	})
}

// waitForContainerExit waits for `he.c` to exit. Depending on the exec's state
// will forcibly transition this exec to the exited state and unblock any
// waiters.
//
// This MUST be called via a goroutine at exec create.
func (he *hcsExec) waitForContainerExit() {
	cexit := make(chan struct{})
	go func() {
		he.c.Wait()
		close(cexit)
	}()
	select {
	case <-cexit:
		// Container exited first. We need to force the process into the exited
		// state and cleanup any resources
		he.sl.Lock()
		switch he.state {
		case shimExecStateCreated:
			he.exitFromCreatedL(1)
		case shimExecStateRunning:
			// Kill the process to unblock `he.waitForExit`.
			he.p.Process.Kill()
		}
		he.sl.Unlock()
	case <-he.processDone:
		// Process exited first. This is the normal case do nothing because
		// `he.waitForExit` will release any waiters.
	}
}

// escapeArgs makes a Windows-style escaped command line from a set of arguments
func escapeArgs(args []string) string {
	escapedArgs := make([]string, len(args))
	for i, a := range args {
		escapedArgs[i] = windows.EscapeArg(a)
	}
	return strings.Join(escapedArgs, " ")
}
