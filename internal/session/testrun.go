// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/proc"
)

// Test run bounds: the runner's output lines are read up to maxRunnerLine
// bytes, and the last runnerTailLines are kept to explain a failure.
const (
	maxRunnerLine   = 1 << 20
	runnerTailLines = 100
)

// testRun is a test session's runner ('dotnet test'): it starts a test host
// that waits for a debugger, and the session ends when the runner exits.
//
// cmd is set once, under Session.mu, when the runner started; code, tail
// and started are guarded by Session.mu too. hostPIDs and done are set
// before the session is shared.
type testRun struct {
	name string // e.g. "dotnet test", for the end reason
	cmd  *exec.Cmd
	// hostPIDs receives the first test host's pid (buffered: never
	// blocks the output pump).
	hostPIDs chan int
	// done is closed once the runner exited and its output was read.
	done chan struct{}
	code *int
	tail []string
	// started is set once the session start succeeded: from then on the
	// runner's exit ends the session.
	started bool
}

// startTest starts a session that debugs a test run. With a self-hosting
// test app (TestCommand.Launch) it runs a normal launch session, mode test.
// Otherwise the driver's test command runs (visible as a starting session,
// so 'stop' can end a long build), prints its test host's pid, and the
// session attaches to it.
func (m *Manager) startTest(ctx context.Context, c api.Client, drv Driver, p api.StartParams, policy api.LeasePolicy) (*Session, error) {
	tester, ok := drv.(Tester)
	att, canAttach := drv.(Attacher)

	if !ok || !canAttach {
		return nil, api.NewError(api.CodeInvalidRequest, p.Lang+" can't debug test runs", "use 'eyedbg start'")
	}

	if err := testOnly(p.LaunchSpec); err != nil {
		return nil, err
	}

	tc, err := tester.TestCommand(ctx, TestSpec{LaunchSpec: p.LaunchSpec, TestSpec: *p.Test})
	if err != nil {
		return nil, err
	}

	if tc.Launch != nil {
		launch := *tc.Launch
		launch.Program = tc.Program

		return m.run(ctx, m.create(ctx, c, p, policy, launch, api.ModeTest), launch, p.Breakpoints, nil)
	}

	if len(p.Args) > 0 {
		return nil, api.NewError(api.CodeInvalidRequest,
			"a test run takes no arguments after \"--\": "+strings.Join(p.Args, " "),
			"only a test app eyedbg launches itself takes them; see 'eyedbg help test'")
	}

	return m.startTestRunner(ctx, c, att, p, policy, tc)
}

// startTestRunner is startTest's runner+attach path: the driver's test
// command starts a test host (visible as a starting session, so 'stop' can
// end a long build), and the session attaches to it once it prints its pid.
func (m *Manager) startTestRunner(
	ctx context.Context, c api.Client, att Attacher, p api.StartParams, policy api.LeasePolicy, tc TestCommand,
) (*Session, error) {
	s := m.create(ctx, c, p, policy, Launch{Program: tc.Program}, api.ModeTest)
	s.run = &testRun{name: runnerName(tc), hostPIDs: make(chan int, 1), done: make(chan struct{})}

	m.add(s)
	m.saveIfLive(s)

	if err := s.startRunner(m.ctx, tc, p.Env); err != nil { //nolint:contextcheck // The runner lives as long as the session.
		m.fail(ctx, s, err)

		return nil, err
	}

	pid, err := s.waitHostPID(ctx, tc)
	if err != nil {
		m.fail(ctx, s, err)

		return nil, err
	}

	launch, err := s.prepareHost(ctx, att, pid)
	if err != nil {
		m.fail(ctx, s, err)

		return nil, err
	}

	if _, err := m.run(ctx, s, launch, p.Breakpoints, nil); err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.run.started = true
	finished := s.run.code != nil
	s.mu.Unlock()

	if finished {
		s.awaitAdapter()
		s.finishRun()
	}

	return s, nil
}

// testOnly refuses launch options a test run doesn't take.
func testOnly(spec api.LaunchSpec) error {
	return refuseOptions(spec, []string{"--program", "--cwd", "--stop-on-entry"}, "a test run takes no ",
		"it builds and runs the project's tests; set breakpoints in the tests instead")
}

// runnerName is how the end reason names the runner, e.g. "dotnet test".
func runnerName(tc TestCommand) string {
	name := strings.TrimSuffix(filepath.Base(tc.Path), ".exe")
	if len(tc.Args) > 0 {
		name += " " + tc.Args[0]
	}

	return name
}

// startRunner starts the test command in its own process group, with its
// output going to the session's, and watches it.
func (s *Session) startRunner(life context.Context, tc TestCommand, env map[string]string) error {
	cmd := exec.CommandContext(life, tc.Path, tc.Args...) //nolint:gosec // The driver's test command; arguments are argv, never a shell.
	cmd.Env = append(append(os.Environ(), tc.Env...), envList(env)...)
	cmd.Dir = tc.Dir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("test runner stdout: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("test runner stderr: %w", err)
	}

	if err := proc.StartGroup(cmd); err != nil {
		return api.NewError(api.CodeNoTestHost, "start "+tc.Program+": "+err.Error(), "")
	}

	s.mu.Lock()
	s.run.cmd = cmd
	stopped := s.state == api.StateExited
	s.mu.Unlock()

	var pumps sync.WaitGroup

	pumps.Go(func() { s.pump(stdout, "stdout", tc.HostPID) })
	pumps.Go(func() { s.pump(stderr, "stderr", nil) })

	go s.watchRunner(&pumps)

	// A stop that came before cmd was set found nothing to kill.
	if stopped {
		_ = proc.KillGroup(life, cmd)

		return s.stoppedWhileStarting()
	}

	return nil
}

// envList turns env into sorted K=V pairs.
func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		out = append(out, k+"="+env[k])
	}

	return out
}

// pump copies the runner's output lines into the session's output. With
// hostPID it reports the first test host's pid, and warns of later ones.
func (s *Session) pump(r io.Reader, category string, hostPID func(string) (int, bool)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxRunnerLine)

	first := 0

	for sc.Scan() {
		line := sc.Text()

		s.mu.Lock()
		s.appendOutputLocked(category, line+"\n")
		s.run.tail = append(s.run.tail, line)

		if len(s.run.tail) > runnerTailLines {
			s.run.tail = s.run.tail[len(s.run.tail)-runnerTailLines:]
		}
		s.mu.Unlock()

		if hostPID == nil {
			continue
		}

		pid, ok := hostPID(line)

		switch {
		case !ok || pid == first:
		case first == 0:
			first = pid
			s.run.hostPIDs <- pid
		default:
			s.mu.Lock()
			s.appendOutputLocked("stderr", fmt.Sprintf("eyedbg: test host %d is not debugged (only the first one, %d, is): "+
				"to debug one target framework, pass --framework\n", pid, first))
			s.mu.Unlock()
		}
	}

	// A line too long for the scanner: keep draining, or the runner would
	// block on a full pipe.
	_, _ = io.Copy(io.Discard, r)
}

// watchRunner waits for the runner to exit (after its output was read),
// records its exit code, and ends the session if its start finished.
func (s *Session) watchRunner(pumps *sync.WaitGroup) {
	pumps.Wait()

	code := 0

	if err := s.run.cmd.Wait(); err != nil {
		code = -1

		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			code = exitErr.ExitCode()
		}
	}

	s.mu.Lock()
	s.run.code = &code
	started := s.run.started
	s.mu.Unlock()

	close(s.run.done)

	// Before its start finished, the start's failure ends the session
	// (and the recording closes itself after the ended event).
	if started {
		s.awaitAdapter()
		s.finishRun()
		s.closeRecording()
	}
}

// awaitAdapter waits (at most shutdownTimeout) for the test host's adapter
// to go away: its host terminated with the run, so it is shutting down,
// and its last output belongs before the session's ended event.
func (s *Session) awaitAdapter() {
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()

	if client == nil {
		return
	}

	t := time.NewTimer(shutdownTimeout)
	defer t.Stop()

	select {
	case <-client.Done():
	case <-t.C:
	}
}

// finishRun ends a test session whose runner exited: its exit code is the
// session's.
func (s *Session) finishRun() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state == api.StateExited || s.run.code == nil {
		return
	}

	code, logged := *s.run.code, *s.run.code
	s.exitCode = &code
	s.log.append(api.Event{Kind: api.EventExited, ExitCode: &logged})
	s.endLocked(fmt.Sprintf("the test run finished: %s exited with code %d", s.run.name, code))
}

// waitHostPID waits for the runner to name its test host. The runner
// exiting first is the driver's failure (a build error, no tests); the
// session ending first means a client stopped it.
func (s *Session) waitHostPID(ctx context.Context, tc TestCommand) (int, error) {
	for {
		s.mu.Lock()
		exited := s.state == api.StateExited
		changed := s.changed
		s.mu.Unlock()

		if exited {
			return 0, s.stoppedWhileStarting()
		}

		select {
		case pid := <-s.run.hostPIDs:
			return pid, nil
		case <-s.run.done:
			s.mu.Lock()
			code, tail := *s.run.code, strings.Join(s.run.tail, "\n")
			stopping := s.stopReason != "" || s.state == api.StateExited
			s.mu.Unlock()

			// A stop kills the runner before it ends the session: the
			// runner's exit is then the stop's doing, not a failure.
			if stopping {
				return 0, s.stoppedWhileStarting()
			}

			return 0, tc.Failure(code, tail)
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return 0, api.NewError(api.CodeNoTestHost, "no test host waited for the debugger within "+startTimeout.String(),
					"see 'eyedbg output' for what the test run printed")
			}

			return 0, fmt.Errorf("wait for the test host: %w", ctx.Err())
		case <-changed:
		}
	}
}

// stoppedWhileStarting is the start's error when a client stopped the
// session meanwhile.
func (s *Session) stoppedWhileStarting() error {
	return api.NewError(api.CodeSessionExited, "session "+s.ID+" was stopped while starting", "")
}

// prepareHost checks the test host (the caller's own process) and returns
// how to attach to it; the session takes the driver's settings for it.
func (s *Session) prepareHost(ctx context.Context, att Attacher, pid int) (Launch, error) {
	if _, err := checkProcess(ctx, pid); err != nil {
		return Launch{}, err
	}

	launch, err := att.PrepareAttach(ctx, api.AttachSpec{PID: pid})
	if err != nil {
		return Launch{}, err
	}

	launch.Request, launch.PID = RequestAttach, pid

	s.mu.Lock()
	defer s.mu.Unlock()

	s.pid = pid
	s.excFilters, s.sideEffects = launch.ExceptionFilters, launch.SideEffects
	s.adapter, s.pauseUnsupported, s.setByEval = launch.AdapterName, launch.PauseUnsupported, launch.SetByEval
	s.exitCodeUnknown = launch.ExitCodeUnknown

	return launch, nil
}

// killRunner kills a test session's runner and everything it started,
// unless the runner has exited: then its process group is gone (its host
// was told to terminate when it detached), and its pid may belong to
// another process by now. It reports whether it sent the kill. (Between
// Wait reaping the runner and watchRunner recording its code there is a
// window of microseconds where the kill still goes out; pid reuse within
// it is not a practical risk.)
func (s *Session) killRunner(ctx context.Context) bool {
	if s.run == nil {
		return false
	}

	s.mu.Lock()
	cmd, exited := s.run.cmd, s.run.code != nil
	s.mu.Unlock()

	if cmd == nil || exited {
		return false
	}

	if err := proc.KillGroup(ctx, cmd); err != nil {
		s.logger.WarnContext(ctx, "kill the test run", slog.Any("error", err), slog.Int("pid", cmd.Process.Pid))
	}

	return true
}

// hostExitedLocked notes in the output that a test session's host exited
// (the session's own exit is the runner's).
func (s *Session) hostExitedLocked(code int) {
	s.appendOutputLocked("console", "eyedbg: the test host exited with code "+strconv.Itoa(code)+"\n")
}
