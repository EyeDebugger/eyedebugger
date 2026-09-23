// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/proc"
)

// startAttach starts a session attached to p.Attach's process, which must
// be the caller's user's.
func (m *Manager) startAttach(ctx context.Context, c api.Client, drv Driver, p api.StartParams, policy api.LeasePolicy) (*Session, error) {
	att, ok := drv.(Attacher)
	if !ok {
		return nil, api.NewError(api.CodeInvalidRequest, p.Lang+" can't attach to a running process", "use 'eyedbg start'")
	}

	if err := launchOnly(p.LaunchSpec); err != nil {
		return nil, err
	}

	info, err := checkProcess(ctx, p.Attach.PID)
	if err != nil {
		return nil, err
	}

	launch, err := att.PrepareAttach(ctx, *p.Attach)
	if err != nil {
		return nil, err
	}

	launch.Request, launch.PID, launch.Program = RequestAttach, info.PID, processName(info)

	return m.run(ctx, m.create(ctx, c, p, policy, launch, api.ModeAttach), launch, p.Breakpoints)
}

// launchOnly refuses launch options for a session that doesn't launch.
func launchOnly(spec api.LaunchSpec) error {
	return refuseOptions(spec, nil, "attaching takes no launch options: ", "the process already runs; drop them")
}

// launchOption is a launch field that sessions other than launched ones
// may refuse.
type launchOption struct {
	name string
	set  bool
}

func launchOptions(spec api.LaunchSpec) []launchOption {
	return []launchOption{
		{"--program", spec.Program != ""},
		{"--project", spec.Project != ""},
		{"program arguments", len(spec.Args) > 0},
		{"--cwd", spec.Cwd != ""},
		{"--env", len(spec.Env) > 0},
		{"--stop-on-entry", spec.StopOnEntry},
		{"--no-build", spec.NoBuild},
	}
}

// refuseOptions is INVALID_REQUEST naming the options set in spec, among
// only (nil: every one), after msg; nil when none is set.
func refuseOptions(spec api.LaunchSpec, only []string, msg, hint string) error {
	var set []string

	for _, o := range launchOptions(spec) {
		if o.set && (only == nil || slices.Contains(only, o.name)) {
			set = append(set, o.name)
		}
	}

	if len(set) == 0 {
		return nil
	}

	return api.NewError(api.CodeInvalidRequest, msg+strings.Join(set, ", "), hint)
}

// checkProcess checks that pid is a process of the caller's user: eyedbg
// attaches only to those.
func checkProcess(ctx context.Context, pid int) (proc.Info, error) {
	info, err := proc.Lookup(ctx, pid)

	switch {
	case errors.Is(err, proc.ErrNoProcess):
		return info, api.NewError(api.CodeInvalidRequest, fmt.Sprintf("no process %d", pid), "check the pid (e.g. with ps)")
	case err != nil:
		return info, api.NewError(api.CodeAttachFailed, fmt.Sprintf("can't check process %d: %v", pid, err), "")
	case !info.SameUser:
		owner := info.Owner
		if owner == "" {
			owner = "unknown"
		}

		return info, api.NewError(api.CodeAttachFailed, fmt.Sprintf("process %d belongs to another user (%s)", pid, owner),
			"eyedbg attaches only to your own processes")
	default:
		return info, nil
	}
}

// processName describes an attached process for status: never its command
// line, which may hold secrets.
func processName(info proc.Info) string {
	name := "pid " + strconv.Itoa(info.PID)
	if info.Name != "" {
		name += " (" + info.Name + ")"
	}

	return name
}

// Detach ends an attached session for client c and leaves the program
// running. Launched sessions and test runs can't be detached from ('stop'
// ends them). Like Terminate, it needs the lease while the program is live.
func (s *Session) Detach(ctx context.Context, c api.Client) error {
	switch s.mode {
	case api.ModeAttach:
	case api.ModeTest:
		return api.NewError(api.CodeInvalidRequest, "a test run can't be detached: 'eyedbg stop' ends it", "")
	default:
		return api.NewError(api.CodeInvalidRequest, "session "+s.ID+" was started, not attached: 'eyedbg stop' ends it", "")
	}

	return s.Terminate(ctx, c)
}
