package main

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/Microsoft/hcsshim/internal/shimdiag"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/runtime/v2/task"
	google_protobuf1 "github.com/gogo/protobuf/types"
	"github.com/sirupsen/logrus"
)

func beginActivity(activity string, fields logrus.Fields) *logrus.Entry {
	log := logrus.WithFields(fields)
	log.Info(activity)
	return log
}

func endActivity(log *logrus.Entry, activity string, err error) {
	if err != nil {
		log.Data[logrus.ErrorKey] = err
		log.Error(activity)
	} else {
		log.Info(activity)
	}
}

type cdevent struct {
	topic string
	event interface{}
}

var _ = (task.TaskService)(&service{})

type service struct {
	events publisher
	// tid is the original task id to be served. This can either be a single
	// task or represent the POD sandbox task id. The first call to Create MUST
	// match this id or the shim is considered to be invalid.
	//
	// This MUST be treated as readonly for the lifetime of the shim.
	tid string
	// isSandbox specifies if `tid` is a POD sandbox. If `false` the shim will
	// reject all calls to `Create` where `tid` does not match. If `true`
	// multiple calls to `Create` are allowed as long as the workload containers
	// all have the same parent task id.
	//
	// This MUST be treated as readonly for the lifetime of the shim.
	isSandbox bool

	// taskOrPod is either the `pod` this shim is tracking if `isSandbox ==
	// true` or it is the `task` this shim is tracking. If no call to `Create`
	// has taken place yet `taskOrPod.Load()` MUST return `nil`.
	taskOrPod atomic.Value

	// cl is the create lock. Since each shim MUST only track a single task or
	// POD. `cl` is used to create the task or POD sandbox. It SHOULD not be
	// taken when creating tasks in a POD sandbox as they can happen
	// concurrently.
	cl sync.Mutex
}

func (s *service) State(ctx context.Context, req *task.StateRequest) (resp *task.StateResponse, err error) {
	defer panicRecover()
	const activity = "State"
	af := logrus.Fields{
		"tid": req.ID,
		"eid": req.ExecID,
	}
	log := beginActivity(activity, af)
	defer func() {
		log.Data["status"] = resp.Status.String()
		log.Data["exitStatus"] = resp.ExitStatus
		log.Data["exitedAt"] = resp.ExitedAt
		endActivity(log, activity, err)
	}()

	r, e := s.stateInternal(ctx, req)
	return r, errdefs.ToGRPC(e)
}

func (s *service) Create(ctx context.Context, req *task.CreateTaskRequest) (resp *task.CreateTaskResponse, err error) {
	defer panicRecover()
	const activity = "Create"
	log := beginActivity(activity, logrus.Fields{
		"tid":              req.ID,
		"bundle":           req.Bundle,
		"rootfs":           req.Rootfs,
		"terminal":         req.Terminal,
		"stdin":            req.Stdin,
		"stdout":           req.Stdout,
		"stderr":           req.Stderr,
		"checkpoint":       req.Checkpoint,
		"parentcheckpoint": req.ParentCheckpoint,
	})
	defer func() {
		log.Data["pid"] = resp.Pid
		endActivity(log, activity, err)
	}()

	r, e := s.createInternal(ctx, req)
	return r, errdefs.ToGRPC(e)
}

func (s *service) Start(ctx context.Context, req *task.StartRequest) (resp *task.StartResponse, err error) {
	defer panicRecover()
	const activity = "Start"
	af := logrus.Fields{
		"tid": req.ID,
		"eid": req.ExecID,
	}
	log := beginActivity(activity, af)
	defer func() {
		log.Data["pid"] = resp.Pid
		endActivity(log, activity, err)
	}()

	r, e := s.startInternal(ctx, req)
	return r, errdefs.ToGRPC(e)
}

func (s *service) Delete(ctx context.Context, req *task.DeleteRequest) (resp *task.DeleteResponse, err error) {
	defer panicRecover()
	const activity = "Delete"
	af := logrus.Fields{
		"tid": req.ID,
		"eid": req.ExecID,
	}
	log := beginActivity(activity, af)
	defer func() {
		log.Data["pid"] = resp.Pid
		log.Data["exitStatus"] = resp.ExitStatus
		log.Data["exitedAt"] = resp.ExitedAt
		endActivity(log, activity, err)
	}()

	r, e := s.deleteInternal(ctx, req)
	return r, errdefs.ToGRPC(e)
}

func (s *service) Pids(ctx context.Context, req *task.PidsRequest) (_ *task.PidsResponse, err error) {
	defer panicRecover()
	const activity = "Pids"
	af := logrus.Fields{
		"tid": req.ID,
	}
	log := beginActivity(activity, af)
	defer func() { endActivity(log, activity, err) }()

	r, e := s.pidsInternal(ctx, req)
	return r, errdefs.ToGRPC(e)
}

func (s *service) Pause(ctx context.Context, req *task.PauseRequest) (_ *google_protobuf1.Empty, err error) {
	defer panicRecover()
	const activity = "Pause"
	af := logrus.Fields{
		"tid": req.ID,
	}
	log := beginActivity(activity, af)
	defer func() { endActivity(log, activity, err) }()

	r, e := s.pauseInternal(ctx, req)
	return r, errdefs.ToGRPC(e)
}

func (s *service) Resume(ctx context.Context, req *task.ResumeRequest) (_ *google_protobuf1.Empty, err error) {
	defer panicRecover()
	const activity = "Resume"
	af := logrus.Fields{
		"tid": req.ID,
	}
	log := beginActivity(activity, af)
	defer func() { endActivity(log, activity, err) }()

	r, e := s.resumeInternal(ctx, req)
	return r, errdefs.ToGRPC(e)
}

func (s *service) Checkpoint(ctx context.Context, req *task.CheckpointTaskRequest) (_ *google_protobuf1.Empty, err error) {
	defer panicRecover()
	const activity = "Checkpoint"
	af := logrus.Fields{
		"tid":  req.ID,
		"path": req.Path,
	}
	log := beginActivity(activity, af)
	defer func() { endActivity(log, activity, err) }()

	r, e := s.checkpointInternal(ctx, req)
	return r, errdefs.ToGRPC(e)
}

func (s *service) Kill(ctx context.Context, req *task.KillRequest) (_ *google_protobuf1.Empty, err error) {
	defer panicRecover()
	const activity = "Kill"
	af := logrus.Fields{
		"tid":    req.ID,
		"eid":    req.ExecID,
		"signal": req.Signal,
		"all":    req.All,
	}
	log := beginActivity(activity, af)
	defer func() { endActivity(log, activity, err) }()

	r, e := s.killInternal(ctx, req)
	return r, errdefs.ToGRPC(e)
}

func (s *service) Exec(ctx context.Context, req *task.ExecProcessRequest) (_ *google_protobuf1.Empty, err error) {
	defer panicRecover()
	const activity = "Exec"
	af := logrus.Fields{
		"tid":      req.ID,
		"eid":      req.ExecID,
		"terminal": req.Terminal,
		"stdin":    req.Stdin,
		"stdout":   req.Stdout,
		"stderr":   req.Stderr,
	}
	log := beginActivity(activity, af)
	defer func() { endActivity(log, activity, err) }()

	r, e := s.execInternal(ctx, req)
	return r, errdefs.ToGRPC(e)
}

func (s *service) DiagExecInHost(ctx context.Context, req *shimdiag.ExecProcessRequest) (_ *shimdiag.ExecProcessResponse, err error) {
	defer panicRecover()
	const activity = "DiagExecInHost"
	af := logrus.Fields{
		"args":     req.Args,
		"workdir":  req.Workdir,
		"terminal": req.Terminal,
		"stdin":    req.Stdin,
		"stdout":   req.Stdout,
		"stderr":   req.Stderr,
	}
	log := beginActivity(activity, af)
	defer func() { endActivity(log, activity, err) }()

	r, e := s.diagExecInHostInternal(ctx, req)
	return r, errdefs.ToGRPC(e)
}

func (s *service) ResizePty(ctx context.Context, req *task.ResizePtyRequest) (_ *google_protobuf1.Empty, err error) {
	defer panicRecover()
	const activity = "ResizePty"
	af := logrus.Fields{
		"tid":    req.ID,
		"eid":    req.ExecID,
		"width":  req.Width,
		"height": req.Height,
	}
	log := beginActivity(activity, af)
	defer func() { endActivity(log, activity, err) }()

	r, e := s.resizePtyInternal(ctx, req)
	return r, errdefs.ToGRPC(e)
}

func (s *service) CloseIO(ctx context.Context, req *task.CloseIORequest) (_ *google_protobuf1.Empty, err error) {
	defer panicRecover()
	const activity = "CloseIO"
	af := logrus.Fields{
		"tid":   req.ID,
		"eid":   req.ExecID,
		"stdin": req.Stdin,
	}
	log := beginActivity(activity, af)
	defer func() { endActivity(log, activity, err) }()

	r, e := s.closeIOInternal(ctx, req)
	return r, errdefs.ToGRPC(e)
}

func (s *service) Update(ctx context.Context, req *task.UpdateTaskRequest) (_ *google_protobuf1.Empty, err error) {
	defer panicRecover()
	const activity = "Update"
	af := logrus.Fields{
		"tid": req.ID,
	}
	log := beginActivity(activity, af)
	defer func() { endActivity(log, activity, err) }()

	r, e := s.updateInternal(ctx, req)
	return r, errdefs.ToGRPC(e)
}

func (s *service) Wait(ctx context.Context, req *task.WaitRequest) (resp *task.WaitResponse, err error) {
	defer panicRecover()
	const activity = "Wait"
	af := logrus.Fields{
		"tid": req.ID,
		"eid": req.ExecID,
	}
	log := beginActivity(activity, af)
	defer func() {
		log.Data["exitStatus"] = resp.ExitStatus
		log.Data["exitedAt"] = resp.ExitedAt
		endActivity(log, activity, err)
	}()

	r, e := s.waitInternal(ctx, req)
	return r, errdefs.ToGRPC(e)
}

func (s *service) Stats(ctx context.Context, req *task.StatsRequest) (_ *task.StatsResponse, err error) {
	defer panicRecover()
	const activity = "Stats"
	af := logrus.Fields{
		"tid": req.ID,
	}
	log := beginActivity(activity, af)
	defer func() { endActivity(log, activity, err) }()

	r, e := s.statsInternal(ctx, req)
	return r, errdefs.ToGRPC(e)
}

func (s *service) Connect(ctx context.Context, req *task.ConnectRequest) (resp *task.ConnectResponse, err error) {
	defer panicRecover()
	const activity = "Connect"
	af := logrus.Fields{
		"tid": req.ID,
	}
	log := beginActivity(activity, af)
	defer func() {
		log.Data["shimPid"] = resp.ShimPid
		log.Data["taskPid"] = resp.TaskPid
		log.Data["version"] = resp.Version
		endActivity(log, activity, err)
	}()

	r, e := s.connectInternal(ctx, req)
	return r, errdefs.ToGRPC(e)
}

func (s *service) Shutdown(ctx context.Context, req *task.ShutdownRequest) (_ *google_protobuf1.Empty, err error) {
	defer panicRecover()
	const activity = "Shutdown"
	af := logrus.Fields{
		"tid": req.ID,
		"now": req.Now,
	}
	log := beginActivity(activity, af)
	defer func() { endActivity(log, activity, err) }()

	r, e := s.shutdownInternal(ctx, req)
	return r, errdefs.ToGRPC(e)
}

func (s *service) DiagStacks(ctx context.Context, req *shimdiag.StacksRequest) (_ *shimdiag.StacksResponse, err error) {
	defer panicRecover()
	const activity = "DiagStacks"
	af := logrus.Fields{}
	log := beginActivity(activity, af)
	defer func() { endActivity(log, activity, err) }()

	buf := make([]byte, 4096)
	for {
		buf = buf[:runtime.Stack(buf, true)]
		if len(buf) < cap(buf) {
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	return &shimdiag.StacksResponse{Stacks: string(buf)}, nil
}
