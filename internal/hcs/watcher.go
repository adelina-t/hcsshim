package hcs

import (
	"context"
	"fmt"
	"reflect"
	"runtime"

	"github.com/Microsoft/hcsshim/internal/logfields"
	"github.com/Microsoft/hcsshim/internal/timeout"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
)

// syscallWatcher is used as a very simple goroutine around calls into
// the platform. In some cases, we have seen HCS APIs not returning due to
// various bugs, and the goroutine making the syscall ends up not returning,
// prior to its async callback. By spinning up a syscallWatcher, it allows
// us to at least log a warning if a syscall doesn't complete in a reasonable
// amount of time.
//
// Usage is:
//
// syscallWatcher(logContext, func() {
//    err = <syscall>(args...)
// })
//

func nameForFunction(function func()) string {
	name := runtime.FuncForPC(reflect.ValueOf(function).Pointer()).Name()
	return name
}

func genUUID() string {
	id := uuid.New()
	return id.String()
}

func syscallWatcher(logContext logrus.Fields, syscallLambda func()) {
	syscallId := genUUID()
	ctx, cancel := context.WithTimeout(context.Background(), timeout.SyscallWatcher)
	defer cancel()
	go watchFunc(ctx, logContext, syscallLambda, syscallId)
	syscallLambda()
	logrus.WithFields(logContext).Warning(fmt.Sprintf("### Syscall %s ID: %s ### syscall finished", nameForFunction(syscallLambda), syscallId))
}

func watchFunc(ctx context.Context, logContext logrus.Fields, functionToWatch func(), syscallId string) {
	nameOfFunctionToWatch := nameForFunction(functionToWatch)
	logrus.WithFields(logContext).Warning(fmt.Sprintf("### Started watching syscall %s. ID: %s ", nameOfFunctionToWatch, syscallId))
	select {
	case <-ctx.Done():
		if ctx.Err() != context.Canceled {
			errorMessage := fmt.Sprintf("### Syscall %s ID %s ### Syscall did not complete within operation timeout. This may indicate a platform issue. If it appears to be making no forward progress, obtain the stacks and see if there is a syscall stuck in the platform API for a significant length of time.", nameOfFunctionToWatch, syscallId)
			logrus.WithFields(logContext).
				WithField(logfields.Timeout, timeout.SyscallWatcher).
				Warning(errorMessage)
		}
		if ctx.Err() == context.Canceled {
			errorMessage := fmt.Sprintf("### Syscall %s ID %s ### syscall canceled.", nameOfFunctionToWatch, syscallId)
			logrus.WithFields(logContext).Warning(errorMessage)
		}

	}
}
