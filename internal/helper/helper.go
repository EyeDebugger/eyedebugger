// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package helper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/proc"
)

// Timeouts of the runner itself; a call's own bound is its ctx.
const (
	// DefaultHelloTimeout bounds a helper's start: process, runtime and
	// hello.
	DefaultHelloTimeout = 30 * time.Second
	// WaitDelay is how long a helper gets to exit after its stdin closed
	// before it is killed.
	WaitDelay = 2 * time.Second
)

// MethodHello is the first request of every helper protocol.
const MethodHello = "hello"

// Spec says how to start a side helper.
type Spec struct {
	// Name names the helper in messages, e.g. "the .NET helper".
	Name string
	// Path is the executable; Args follow it.
	Path string
	Args []string
	// Env is added to eyedbg's own environment.
	Env []string
	// Protocol is the protocol version the caller speaks; the helper's
	// hello must answer the same.
	Protocol int
	// HelloTimeout bounds the start (process, runtime, hello); 0 means
	// DefaultHelloTimeout.
	HelloTimeout time.Duration
	// RuntimeMissing, if set, tells from the exit code of a helper that
	// exited before hello whether the runtime it needs is missing, and
	// returns the error to report then (HELPER_NOT_FOUND); nil otherwise.
	RuntimeMissing func(exitCode int) *api.Error
}

// HelloParams are the hello request's params.
type HelloParams struct {
	Protocol int `json:"protocol"`
}

// Hello is the hello result: the helper's protocol, the eyedbg version it
// was built with, and the runtime it runs on.
type Hello struct {
	Protocol int    `json:"protocol"`
	Version  string `json:"version"`
	Runtime  string `json:"runtime"`
}

// NotifyFunc handles a notification that arrives while a call runs; an
// error ends the call with that error.
type NotifyFunc func(method string, params json.RawMessage) error

// errClosed is returned by a call that Close interrupted.
var errClosed = errors.New("helper closed")

// message is one line read from the helper: a JSON document, or why
// reading stopped (io.EOF at the end of its output).
type message struct {
	raw json.RawMessage
	err error
}

// Helper is a running side helper. Its methods are not safe for concurrent
// use, except Close, which may interrupt a call from another goroutine.
type Helper struct {
	spec   Spec
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    *api.Conn // writes requests to stdin
	stdout *os.File  // read end of the helper's stdout
	stderr *tail
	hello  Hello
	nextID int64

	msgs     chan message  // the reader's lines, in order; closed when it stops
	closing  chan struct{} // closed by Close: the reader stops delivering
	exited   chan struct{} // closed once cmd.Wait has returned
	waitErr  error         // cmd.Wait's result; read only after <-exited
	readDone chan struct{} // closed when the reader goroutine has returned

	closeOnce sync.Once
	closeErr  error
}

// Start starts the helper and says hello. ctx bounds the helper's whole
// life: when it is done, the helper's stdin closes, and it is killed
// WaitDelay later if it hasn't exited. Close the helper when done with it.
func Start(ctx context.Context, spec Spec) (*Helper, error) {
	h, err := spawn(ctx, spec)
	if err != nil {
		return nil, err
	}

	helloTimeout := spec.HelloTimeout
	if helloTimeout <= 0 {
		helloTimeout = DefaultHelloTimeout
	}

	helloCtx, cancel := context.WithTimeout(ctx, helloTimeout)
	defer cancel()

	var hello Hello

	err = h.Stream(helloCtx, MethodHello, HelloParams{Protocol: spec.Protocol}, nil, &hello)

	switch {
	case err == nil && hello.Protocol != spec.Protocol:
		err = api.NewError(api.CodeHelperMismatch,
			fmt.Sprintf("%s speaks protocol %d; this eyedbg speaks %d", spec.Name, hello.Protocol, spec.Protocol),
			"use eyedbg with the helpers/ directory from the same release archive")
	case err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded):
		err = h.failure(fmt.Sprintf("%s didn't start within %s", spec.Name, helloTimeout))
	case err != nil && api.CodeOf(err) != "" && !isRunnerCode(api.CodeOf(err)):
		// The helper answered hello with an error.
		err = api.NewError(api.CodeHelperFailed, fmt.Sprintf("%s refused hello: %s", spec.Name, err.Error()), "")
	}

	if err != nil {
		_ = h.Close() //nolint:contextcheck // Close is bounded by WaitDelay and must run even when ctx is done.

		return nil, err
	}

	h.hello = hello

	return h, nil
}

// spawn starts the helper's process and the goroutines reading its output
// and waiting for it.
func spawn(ctx context.Context, spec Spec) (*Helper, error) {
	cmd := exec.CommandContext(ctx, spec.Path, spec.Args...) //nolint:gosec // The path comes from the helper's lookup (next to eyedbg, or an absolute $EYEDBG_*_HELPER); arguments are argv, never a shell.
	cmd.Env = append(os.Environ(), spec.Env...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe for %s: %w", spec.Name, err)
	}

	// Our own pipe rather than cmd.StdoutPipe: Wait would close that one
	// as soon as the helper exits, possibly before its last line is read.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe for %s: %w", spec.Name, err)
	}

	errTail := newTail()
	cmd.Stdout = stdoutW
	cmd.Stderr = errTail
	cmd.Cancel = stdin.Close
	cmd.WaitDelay = WaitDelay

	prepare(cmd)

	err = proc.StartGroup(cmd)

	_ = stdoutW.Close() // the helper has its copy (or failed to start)

	if err != nil {
		_ = stdoutR.Close()
		_ = stdin.Close()

		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// A missing file is fs.ErrNotExist from the start itself, or
		// exec.ErrNotFound from the lookup exec.Command does first — on
		// Windows even for a full path, which it tries with each PATHEXT.
		code := api.CodeHelperFailed
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, exec.ErrNotFound) {
			code = api.CodeHelperNotFound
		}

		return nil, api.NewError(code, fmt.Sprintf("start %s: %v", spec.Name, err), "")
	}

	h := &Helper{
		spec:     spec,
		cmd:      cmd,
		stdin:    stdin,
		out:      api.NewConn(nil, stdin),
		stdout:   stdoutR,
		stderr:   errTail,
		msgs:     make(chan message),
		closing:  make(chan struct{}),
		exited:   make(chan struct{}),
		readDone: make(chan struct{}),
	}

	go h.read()
	go h.wait()

	return h, nil
}

// read delivers the helper's output line by line until it ends, fails, or
// Close stops it.
func (h *Helper) read() {
	defer close(h.readDone)
	defer close(h.msgs)

	conn := api.NewConn(h.stdout, nil)

	for {
		var raw json.RawMessage

		err := conn.Read(&raw)

		if h.isClosing() {
			return
		}

		select {
		case h.msgs <- message{raw: raw, err: err}:
		case <-h.closing:
			return
		}

		if err != nil {
			return
		}
	}
}

// isClosing reports whether Close has begun.
func (h *Helper) isClosing() bool {
	select {
	case <-h.closing:
		return true
	default:
		return false
	}
}

func (h *Helper) wait() {
	h.waitErr = h.cmd.Wait()
	close(h.exited)
}

// Hello returns the helper's hello.
func (h *Helper) Hello() Hello { return h.hello }

// PID returns the helper's process id.
func (h *Helper) PID() int { return h.cmd.Process.Pid }

// Call makes a call without notifications and decodes its result into
// result (nil to discard it). ctx bounds the call: when it is done, Call
// returns ctx's error and the helper should be closed.
func (h *Helper) Call(ctx context.Context, method string, params, result any) error {
	return h.Stream(ctx, method, params, nil, result)
}

// Stream makes a call, passing each notification that arrives before its
// result to onNotify, in order, then decodes the result into result.
// Errors: the helper's own (*api.Error, as it sent them); HELPER_FAILED
// when it exits, breaks the protocol or is closed first; ctx's error when
// ctx is done — which wins over any other.
func (h *Helper) Stream(ctx context.Context, method string, params any, onNotify NotifyFunc, result any) error {
	h.nextID++
	id := h.nextID

	req, err := api.NewRequest(id, method, params)
	if err != nil {
		return err
	}

	// A write that fails means the helper is gone or not reading; what it
	// said before going, or its exit, is read below.
	_ = h.out.Write(req)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-h.closing:
			return errClosed
		case m, ok := <-h.msgs:
			if !ok || h.isClosing() {
				return errClosed
			}

			done, err := h.handle(ctx, id, m, onNotify, result)
			if done {
				return err
			}
		}
	}
}

// handle processes one message of call id; done when the call is over.
func (h *Helper) handle(ctx context.Context, id int64, m message, onNotify NotifyFunc, result any) (bool, error) {
	if m.err != nil {
		if ctx.Err() != nil {
			return true, ctx.Err()
		}

		if errors.Is(m.err, io.EOF) {
			return true, h.exitedEarly(ctx)
		}

		return true, h.protocolError(m.err.Error())
	}

	var probe struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}

	if err := json.Unmarshal(m.raw, &probe); err != nil {
		return true, h.protocolError("not a JSON object")
	}

	if probe.Method != "" {
		if onNotify == nil || (len(probe.ID) > 0 && string(probe.ID) != "null") {
			return true, h.protocolError(fmt.Sprintf("unexpected message %.100q", probe.Method))
		}

		if err := onNotify(probe.Method, probe.Params); err != nil {
			return true, err
		}

		return false, nil
	}

	var resp api.Response
	if err := json.Unmarshal(m.raw, &resp); err != nil || string(probe.ID) != strconv.FormatInt(id, 10) {
		got := string(probe.ID)
		if got == "" {
			got = "without an id"
		}

		return true, h.protocolError(fmt.Sprintf("an answer to request %.20s, expected %d", got, id))
	}

	err := resp.Decode(result)
	if e, ok := errors.AsType[*api.Error](err); ok {
		// The helper's message may name a process: keep it off the terminal.
		return true, api.NewError(e.Code, sanitizeLine(e.Message), sanitizeLine(e.Hint))
	}

	if err != nil {
		return true, h.protocolError(err.Error())
	}

	return true, nil
}

// exitedEarly is the error for a helper whose output ended before it
// answered: its exit status and the end of its stderr.
func (h *Helper) exitedEarly(ctx context.Context) error {
	timer := time.NewTimer(WaitDelay)
	defer timer.Stop()

	select {
	case <-h.exited:
	case <-timer.C:
		return h.failure(h.spec.Name + " closed its output before answering")
	case <-ctx.Done():
		return ctx.Err()
	}

	status := "without a status"
	code := -1

	if exitErr, ok := errors.AsType[*exec.ExitError](h.waitErr); ok {
		status = exitErr.String()
		code = exitErr.ExitCode()
	} else if h.waitErr == nil {
		status, code = "exit status 0", 0
	}

	if h.hello.Protocol == 0 && h.spec.RuntimeMissing != nil {
		if e := h.spec.RuntimeMissing(code); e != nil {
			return withStderr(e, h.stderr.Sanitized())
		}
	}

	return h.failure(fmt.Sprintf("%s exited (%s) before answering", h.spec.Name, status))
}

func (h *Helper) protocolError(detail string) error {
	return h.failure(fmt.Sprintf("%s broke the protocol: %s", h.spec.Name, sanitizeLine(detail)))
}

// failure is HELPER_FAILED with message and the end of the helper's stderr.
func (h *Helper) failure(message string) error {
	return withStderr(api.NewError(api.CodeHelperFailed, message,
		"check that the helpers/ directory next to eyedbg comes from the same release"), h.stderr.Sanitized())
}

// withStderr appends the helper's stderr, indented, to e's message.
func withStderr(e *api.Error, stderr string) *api.Error {
	if stderr == "" {
		return e
	}

	return api.NewError(e.Code, e.Message+"; its stderr:\n  "+strings.ReplaceAll(stderr, "\n", "\n  "), e.Hint)
}

// isRunnerCode reports whether code is one this package reports itself,
// rather than one the helper sent.
func isRunnerCode(code api.Code) bool {
	return code == api.CodeHelperFailed || code == api.CodeHelperNotFound || code == api.CodeHelperMismatch
}

// Close closes the helper's stdin, which cancels a running call, waits up
// to WaitDelay for it to exit, then kills its process group. It returns
// once the helper has exited and every goroutine Start began has returned.
// Only a kill is reported as an error. Close is idempotent.
func (h *Helper) Close() error {
	h.closeOnce.Do(func() {
		close(h.closing)
		_ = h.stdin.Close()

		timer := time.NewTimer(WaitDelay)
		defer timer.Stop()

		select {
		case <-h.exited:
		case <-timer.C:
			if err := proc.KillGroup(context.Background(), h.cmd); err != nil {
				h.closeErr = fmt.Errorf("kill %s: %w", h.spec.Name, err)
			} else {
				h.closeErr = fmt.Errorf("%s didn't exit within %s of its stdin closing; killed", h.spec.Name, WaitDelay)
			}

			<-h.exited
		}

		// The helper is gone: its output reaches EOF, or this unblocks it.
		_ = h.stdout.Close()
		<-h.readDone
	})

	return h.closeErr
}
