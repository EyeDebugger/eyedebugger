// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/version"
)

// Environment variables read by [OptionsFromEnv].
const (
	// EnvNoAutostart, when "1", makes clients fail instead of starting eyedbgd.
	EnvNoAutostart = "EYEDBG_NO_AUTOSTART"
	// EnvDaemonPath is the eyedbgd executable to start.
	EnvDaemonPath = "EYEDBG_DAEMON_PATH"
)

const (
	defaultStartTimeout = 5 * time.Second
	defaultStopTimeout  = 10 * time.Second
	pollInterval        = 20 * time.Millisecond
	maxLogSize          = 5 << 20
)

// StartOptions control [Connect]'s auto-start.
type StartOptions struct {
	// NoAutostart makes Connect fail if no daemon is running.
	NoAutostart bool
	// Executable is the eyedbgd to start; empty means next to the running
	// executable, else on PATH.
	Executable string
	// Timeout bounds waiting for a started daemon to accept connections.
	Timeout time.Duration
}

// OptionsFromEnv returns StartOptions from EYEDBG_NO_AUTOSTART and
// EYEDBG_DAEMON_PATH.
func OptionsFromEnv() StartOptions {
	return StartOptions{
		NoAutostart: os.Getenv(EnvNoAutostart) == "1",
		Executable:  os.Getenv(EnvDaemonPath),
	}
}

// Connect returns a client for a daemon matching info, starting one if none
// is running. A running daemon of another version is replaced if it has no
// sessions; otherwise Connect fails with [api.CodeVersionMismatch] rather
// than killing live sessions. started reports whether this call started it.
func Connect(ctx context.Context, p Paths, info version.Info, opts StartOptions) (cl *Client, started bool, err error) {
	cl, err = Dial(ctx, p, info)

	switch {
	case err == nil && compatible(cl.Hello, info):
		return cl, false, nil
	case err == nil:
		if err := replaceStale(ctx, cl, p, info); err != nil {
			return nil, false, err
		}
	case api.CodeOf(err) != api.CodeDaemonNotRunning:
		return nil, false, err
	}

	if opts.NoAutostart {
		return nil, false, api.NewError(api.CodeDaemonNotRunning, "the eyedbg daemon is not running",
			fmt.Sprintf("auto-start is disabled by %s=1; start 'eyedbgd' yourself or unset it", EnvNoAutostart))
	}

	cl, err = start(ctx, p, info, opts)
	if err != nil {
		return nil, false, err
	}

	if !compatible(cl.Hello, info) {
		_ = cl.Close()

		return nil, false, mismatch(cl.Hello, info,
			"the eyedbgd that was started is a different build than this eyedbg; install matching versions or set "+EnvDaemonPath)
	}

	return cl, true, nil
}

func compatible(h api.HelloResult, info version.Info) bool {
	return h.ProtocolVersion == api.ProtocolVersion && h.Version == info.Version && h.Commit == info.Commit
}

func mismatch(h api.HelloResult, info version.Info, hint string) error {
	return api.NewError(api.CodeVersionMismatch,
		fmt.Sprintf("eyedbgd is %s (%s, protocol %d) but eyedbg is %s (%s, protocol %d)",
			h.Version, h.Commit, h.ProtocolVersion, info.Version, info.Commit, api.ProtocolVersion), hint)
}

// replaceStale stops a daemon of another version, unless it has sessions.
func replaceStale(ctx context.Context, cl *Client, p Paths, info version.Info) error {
	defer cl.Close()

	if cl.Hello.Sessions > 0 {
		return mismatch(cl.Hello, info, fmt.Sprintf(
			"it has %d active session(s), so it was left running; finish them, or run 'eyedbg daemon stop --force' (kills their debuggees)",
			cl.Hello.Sessions))
	}

	if err := cl.Call(ctx, api.MethodDaemonStop, api.StopParams{}, nil); err != nil {
		return fmt.Errorf("stop outdated daemon: %w", err)
	}

	return WaitStopped(ctx, p, defaultStopTimeout)
}

// WaitStopped waits until no daemon holds p's lock.
func WaitStopped(ctx context.Context, p Paths, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		f, err := lockFile(p.Lock)
		if err == nil {
			_ = f.Close()

			return nil
		}

		if !errors.Is(err, ErrAlreadyRunning) {
			return err
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for eyedbgd to exit: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// start spawns a detached eyedbgd and waits until it accepts connections.
func start(ctx context.Context, p Paths, info version.Info, opts StartOptions) (*Client, error) {
	if err := p.Prepare(); err != nil {
		return nil, err
	}

	exe, err := daemonExecutable(opts.Executable)
	if err != nil {
		return nil, err
	}

	exited, err := spawn(ctx, p, exe)
	if err != nil {
		return nil, api.NewError(api.CodeDaemonStart, "start "+exe+": "+err.Error(), "")
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultStartTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var exitErr error

	for {
		cl, err := Dial(ctx, p, info)
		if err == nil {
			return cl, nil
		}

		if api.CodeOf(err) != api.CodeDaemonNotRunning {
			return nil, err
		}

		select {
		case exitErr = <-exited:
			// The spawned daemon exited. It may have lost a start race to
			// another one, so keep polling until the timeout.
			exited = nil
		case <-ctx.Done():
			msg := "eyedbgd did not start within " + timeout.String()
			if exitErr != nil {
				msg = "eyedbgd exited during start-up: " + exitErr.Error()
			}

			return nil, api.NewError(api.CodeDaemonStart, msg, "see its log: 'eyedbg daemon logs' ("+p.Log+")")
		case <-ticker.C:
		}
	}
}

// spawn starts exe detached, with stdout and stderr appended to the daemon
// log. The returned channel receives the process's exit error (nil on a
// clean exit) if it exits while this process is still running.
func spawn(ctx context.Context, p Paths, exe string) (<-chan error, error) {
	rotateLog(p.Log)

	logf, err := os.OpenFile(p.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open daemon log: %w", err)
	}
	defer logf.Close()

	// The daemon must outlive this process and ctx, so it gets a context
	// that is never canceled.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), exe)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.Dir = p.Dir // don't pin the caller's working directory
	cmd.Env = append(os.Environ(), EnvRuntimeDir+"="+p.Dir)
	detach(cmd)

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	exited := make(chan error, 1)

	go func() { exited <- cmd.Wait() }()

	return exited, nil
}

// daemonExecutable finds eyedbgd: explicit, else next to the running
// executable, else on PATH.
func daemonExecutable(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}

	name := "eyedbgd"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}

	if self, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(self), name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}

	path, err := exec.LookPath(name)
	if err != nil {
		return "", api.NewError(api.CodeDaemonStart, "cannot find "+name,
			"install it next to eyedbg or on PATH, or set "+EnvDaemonPath)
	}

	return path, nil
}

// rotateLog keeps one previous log once the current one exceeds maxLogSize.
// Failures are ignored: the log is best-effort.
func rotateLog(path string) {
	if info, err := os.Stat(path); err == nil && info.Size() > maxLogSize {
		_ = os.Rename(path, path+".1")
	}
}

// TailLog returns up to n last lines of the daemon log (all lines if n <= 0).
func TailLog(p Paths, n int) ([]string, error) {
	f, err := os.Open(p.Log)
	if err != nil {
		return nil, fmt.Errorf("open daemon log: %w", err)
	}
	defer f.Close()

	var lines []string

	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 0, 4096), api.MaxMessageSize)

	for s.Scan() {
		lines = append(lines, s.Text())
		if n > 0 && len(lines) > n {
			lines = lines[1:]
		}
	}

	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("read daemon log: %w", err)
	}

	return lines, nil
}
