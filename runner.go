package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Command statuses recorded in results.json.
const (
	StatusOK       = "ok"
	StatusFailed   = "failed"
	StatusCanceled = "canceled"
	StatusSignaled = "signaled"  // killed by a signal not sent by runbox
	StatusTimedOut = "timed_out" // exceeded --timeout and was stopped by runbox
)

// killGrace is how long a canceled command has to exit after SIGTERM before
// its process group is sent SIGKILL.
var killGrace = 5 * time.Second // a variable so tests can shorten it

var errTimeout = errors.New("timeout")

// Result describes the outcome of one command. ExitCode is -1 if the command
// could not be started or was terminated by a signal. Signal names the
// terminating signal (e.g. "TERM") and Error says why the command could not be
// started; both are empty otherwise.
type Result struct {
	Name       string `json:"name"`
	Command    string `json:"command"`
	Shell      string `json:"shell"`
	Status     string `json:"status"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
	Log        string `json:"log"`
	Signal     string `json:"signal,omitempty"`
	Error      string `json:"error,omitempty"`
}

// runAll runs every command concurrently and returns results in input order.
// A positive timeout limits how long each command may run. Cancelling ctx sends the running commands SIGTERM; closing force (or
// waiting killGrace) escalates to SIGKILL.
func runAll(ctx context.Context, force <-chan struct{}, dir, shell string, timeout time.Duration, cmds []Command, p *progress) []Result {
	results := make([]Result, len(cmds))
	var wg sync.WaitGroup
	for i, c := range cmds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.started(c.Name)
			results[i] = runOne(ctx, force, dir, shell, timeout, c)
			p.finished(results[i])
		}()
	}
	wg.Wait()
	return results
}

func runOne(ctx context.Context, force <-chan struct{}, dir, shell string, timeout time.Duration, c Command) (r Result) {
	r = Result{Name: c.Name, Command: c.Shell, Shell: shell, Log: c.Name + ".log", ExitCode: -1, Status: StatusFailed}
	start := time.Now()
	defer func() { r.DurationMS = time.Since(start).Milliseconds() }()

	log, err := os.Create(filepath.Join(dir, r.Log))
	if err != nil {
		r.Error = err.Error()
		return r
	}
	defer log.Close()

	parent := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, timeout, errTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, shell, "-c", c.Shell)
	cmd.Stdout = log // same file for both: output is interleaved in order
	cmd.Stderr = log
	// Own process group, so cancellation also reaches the shell's children.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	done := make(chan struct{})
	cmd.Cancel = func() error {
		pgid := -cmd.Process.Pid
		grace := killGrace
		go func() { // escalate to SIGKILL after the grace period or on force
			select {
			case <-time.After(grace):
			case <-force:
			case <-done:
				return
			}
			_ = syscall.Kill(pgid, syscall.SIGKILL)
		}()
		return syscall.Kill(pgid, syscall.SIGTERM)
	}

	err = cmd.Run()
	close(done)

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		r.Status, r.ExitCode = StatusOK, 0
	case parent.Err() == nil && errors.Is(context.Cause(ctx), errTimeout) && (errors.As(err, &exitErr) || errors.Is(err, ctx.Err())):
		r.Status = StatusTimedOut
		if cmd.ProcessState != nil {
			r.ExitCode = cmd.ProcessState.ExitCode()
		}
	case ctx.Err() != nil && (errors.As(err, &exitErr) || errors.Is(err, ctx.Err())):
		r.Status = StatusCanceled
		if cmd.ProcessState != nil {
			r.ExitCode = cmd.ProcessState.ExitCode()
		}
	case errors.As(err, &exitErr):
		r.ExitCode = exitErr.ExitCode()
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			r.Status, r.Signal = StatusSignaled, signalName(ws.Signal())
		}
	default: // failed to start
		r.Error = err.Error()
		fmt.Fprintf(log, "runbox: %v\n", err)
	}
	return r
}

var signalNames = map[syscall.Signal]string{
	syscall.SIGHUP: "HUP", syscall.SIGINT: "INT", syscall.SIGQUIT: "QUIT",
	syscall.SIGABRT: "ABRT", syscall.SIGKILL: "KILL", syscall.SIGSEGV: "SEGV",
	syscall.SIGPIPE: "PIPE", syscall.SIGTERM: "TERM",
}

func signalName(s syscall.Signal) string {
	if n, ok := signalNames[s]; ok {
		return n
	}
	return fmt.Sprintf("signal %d", int(s))
}
