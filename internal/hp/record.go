package hp

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// MaxOutputBytes bounds what we will hold and cache for a single command.
// A runaway command must not exhaust memory or fill the blob store.
const MaxOutputBytes = 8 << 20

// RunResult is one real execution.
type RunResult struct {
	Stdout     []byte
	Stderr     []byte
	ExitCode   int
	DurationMS int64
	Truncated  bool
	TimedOut   bool
}

// boundedBuffer stops accumulating past a limit but keeps counting, so we can
// tell truncated output from complete output and refuse to cache the former.
type boundedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if remaining := b.limit - b.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			b.buf.Write(p[:remaining])
			b.truncated = true
		} else {
			b.buf.Write(p)
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

// Run executes a shell command with a hard process-group timeout, teeing both
// streams to the caller's terminal while capturing them separately.
//
// Separate capture matters: replaying a hit as "cat out; cat err >&2" only
// reproduces the original faithfully if the two streams were never merged.
// Collapsing them makes served output subtly different from real output,
// which shadow verification will then flag forever.
//
// Process-group kill semantics are ported from replay_fleet_v3_rich.py:558:
// start a new session, signal the whole group on timeout, SIGTERM with a
// grace period, then SIGKILL.
func Run(command, dir string, timeout time.Duration) RunResult {
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	outBuf := &boundedBuffer{limit: MaxOutputBytes}
	errBuf := &boundedBuffer{limit: MaxOutputBytes}
	cmd.Stdout = io.MultiWriter(os.Stdout, outBuf)
	cmd.Stderr = io.MultiWriter(os.Stderr, errBuf)
	cmd.Stdin = os.Stdin

	started := time.Now()
	if err := cmd.Start(); err != nil {
		return RunResult{ExitCode: 127, Stderr: []byte(err.Error()), DurationMS: 0}
	}

	pgid := cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var timedOut bool
	if timeout > 0 {
		select {
		case <-done:
		case <-time.After(timeout):
			timedOut = true
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
				<-done
			}
		}
	} else {
		<-done
	}

	res := RunResult{
		Stdout:     outBuf.buf.Bytes(),
		Stderr:     errBuf.buf.Bytes(),
		DurationMS: time.Since(started).Milliseconds(),
		Truncated:  outBuf.truncated || errBuf.truncated,
		TimedOut:   timedOut,
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
		res.ExitCode = ws.ExitStatus()
		if ws.Signaled() {
			res.ExitCode = 128 + int(ws.Signal())
		}
	} else {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	return res
}
