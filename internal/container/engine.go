// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// EnvDocker names the docker executable (an absolute path); unset, docker
// is looked up on the daemon's PATH.
const EnvDocker = "EYEDBG_DOCKER"

// Bounds of one docker invocation.
const (
	// QueryTimeout bounds an inspect or a probe.
	QueryTimeout = 15 * time.Second
	// CopyTimeout bounds a docker cp.
	CopyTimeout = 60 * time.Second
	// maxStdout bounds the stdout an inspect or probe may produce.
	maxStdout = 1 << 20
	// maxStderrTail bounds the stderr kept for an error.
	maxStderrTail = 4 << 10
	// waitDelay is how long a finished docker's pipes may stay open.
	waitDelay = 2 * time.Second
	// shownStderr is how much of the stderr tail an error message shows.
	shownStderr = 400
)

// Engine is the docker CLI to run, and the engine it talks to. The zero Host
// and Context are the CLI's own default. Build one with [NewEngine].
type Engine struct {
	// Docker is the docker executable.
	Docker string
	// Host is a DOCKER_HOST value and wins over Context, a DOCKER_CONTEXT
	// value; each travels as one --flag=value argument before the
	// subcommand, never through the environment.
	Host, Context string
	// Command, when set, builds the process that runs argv (everything after
	// the docker executable) instead of Docker: the seam a test fakes docker
	// at.
	Command func(ctx context.Context, argv []string) *exec.Cmd
}

// NewEngine returns the engine for host and dockerContext (both may be
// empty): both are checked against their grammars, and the docker
// executable is $EYEDBG_DOCKER (an absolute path to an existing file), else
// docker on PATH.
func NewEngine(host, dockerContext string) (Engine, error) {
	e := Engine{Host: host, Context: dockerContext}

	if host != "" {
		if err := ValidateHost(host); err != nil {
			return Engine{}, err
		}
	}

	if dockerContext != "" {
		if err := ValidateContext(dockerContext); err != nil {
			return Engine{}, err
		}
	}

	var err error

	if e.Docker, err = findDocker(); err != nil {
		return Engine{}, err
	}

	return e, nil
}

func findDocker() (string, error) {
	if p := os.Getenv(EnvDocker); p != "" {
		if !usableFile(p) {
			return "", api.NewError(api.CodeAttachFailed, EnvDocker+" is not an absolute path to an existing file",
				"fix "+EnvDocker+" in the environment eyedbgd was started with ('eyedbg daemon stop' ends it)")
		}

		return p, nil
	}

	p, err := exec.LookPath("docker")
	if err != nil {
		return "", api.NewError(api.CodeAttachFailed, "docker was not found on the PATH of eyedbgd",
			"install docker, or set "+EnvDocker+" to its absolute path in the environment eyedbgd is started with")
	}

	return p, nil
}

// usableFile reports whether p is an absolute path to an existing regular
// file.
func usableFile(p string) bool {
	if !filepath.IsAbs(p) {
		return false
	}

	info, err := os.Stat(p) //nolint:gosec // The operator's own setting names the file to check.

	return err == nil && info.Mode().IsRegular()
}

// globalArgs are the engine flags that come before every subcommand.
func (e Engine) globalArgs() []string {
	switch {
	case e.Host != "":
		return []string{"--host=" + e.Host}
	case e.Context != "":
		return []string{"--context=" + e.Context}
	default:
		return nil
	}
}

// Args is the argv (after the docker executable) of subcommand sub with its
// arguments.
func (e Engine) Args(sub ...string) []string { return append(e.globalArgs(), sub...) }

// ExecArgs is the argv (after the docker executable) that runs program with
// args in the container with id (a full id) with its stdin open, as the
// container's own user, working directory and environment: no -e, -u, -w,
// -t or --privileged.
func (e Engine) ExecArgs(id, program string, args ...string) []string {
	return e.Args(slices.Concat([]string{"exec", "-i", id, program}, args)...)
}

// result is what a finished docker invocation left.
type result struct {
	stdout []byte
	// stderr is the end of what it wrote to stderr, control characters
	// stripped; only for an error message.
	stderr string
}

// failure is a docker invocation that didn't succeed: it didn't start, ran
// too long, wrote too much or exited non-zero.
type failure struct {
	sub    string
	stderr string
	err    error
}

func (f *failure) Error() string {
	msg := "docker " + f.sub + " failed"

	if f.err != nil {
		msg += ": " + f.err.Error()
	}

	if f.stderr != "" {
		msg += ": " + f.stderr
	}

	return msg
}

func (f *failure) Unwrap() error { return f.err }

// run runs docker with argv (after the executable), bounded by timeout, with
// stdin as its standard input (nil: none). Its stdout is read up to
// maxStdout bytes: more is a failure.
func (e Engine) run(ctx context.Context, timeout time.Duration, stdin io.Reader, argv []string) (result, *failure) {
	sub := subcommand(argv)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	if e.Command != nil {
		cmd = e.Command(ctx, argv)
	} else {
		cmd = exec.CommandContext(ctx, e.Docker, argv...) //nolint:gosec // Docker is $EYEDBG_DOCKER or PATH's docker; every argument is checked against a grammar first.
		cmd.Env = withoutEngineVars(os.Environ())
	}

	out := &capWriter{limit: maxStdout, cancel: cancel}
	tail := &tailWriter{limit: maxStderrTail}

	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, out, tail
	cmd.WaitDelay = waitDelay

	err := cmd.Run()
	res := result{stdout: out.buf.Bytes(), stderr: tail.text()}

	switch {
	case out.over:
		return res, &failure{sub: sub, err: fmt.Errorf("it wrote more than %d bytes", maxStdout)}
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return res, &failure{sub: sub, err: errors.New("it did not finish in time"), stderr: res.stderr}
	case err != nil:
		return res, &failure{sub: sub, err: exitErr(err), stderr: res.stderr}
	default:
		return res, nil
	}
}

// withoutEngineVars is env without DOCKER_HOST and DOCKER_CONTEXT: the
// engine is whatever the request named (as --host or --context), never what
// the environment eyedbgd happened to be started in says, which may be a
// different shell's.
func withoutEngineVars(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")

		return strings.EqualFold(name, "DOCKER_HOST") || strings.EqualFold(name, "DOCKER_CONTEXT")
	})
}

// exitErr is err without the executable's path.
func exitErr(err error) error {
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return fmt.Errorf("exit status %d", exit.ExitCode())
	}

	return err
}

// subcommand names what argv runs, for messages: its first element that is
// not an engine flag.
func subcommand(argv []string) string {
	for _, a := range argv {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}

	return "command"
}

// capWriter keeps at most limit bytes; past it, it fails the invocation by
// calling cancel.
type capWriter struct {
	buf    bytes.Buffer
	limit  int
	over   bool
	cancel context.CancelFunc
}

func (w *capWriter) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > w.limit {
		w.over = true
		w.cancel()

		return 0, errors.New("output too large")
	}

	return w.buf.Write(p)
}

// tailWriter keeps the last limit bytes written.
type tailWriter struct {
	buf   []byte
	limit int
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	if extra := len(w.buf) - w.limit; extra > 0 {
		w.buf = append(w.buf[:0], w.buf[extra:]...)
	}

	return len(p), nil
}

// text is the tail with control characters (newlines too) turned into
// spaces and runs of spaces squeezed.
func (w *tailWriter) text() string {
	s := strings.ToValidUTF8(string(w.buf), "?")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}

		return r
	}, s)

	return strings.Join(strings.Fields(s), " ")
}

// shortStderr is the end of a failure's stderr for a message.
func shortStderr(s string) string {
	if len(s) > shownStderr {
		s = "..." + strings.ToValidUTF8(s[len(s)-shownStderr:], "")
	}

	return s
}
