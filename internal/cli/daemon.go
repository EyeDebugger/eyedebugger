// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/daemon"
	"github.com/eyedebugger/eyedebugger/internal/version"
)

// daemonCallTimeout bounds each round trip to a running daemon.
const daemonCallTimeout = 10 * time.Second

func newDaemonCommand(info version.Info, g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Inspect and control the per-user eyedbgd daemon",
		Long: `Inspect and control eyedbgd, the per-user background process that owns every debug session
(docs/DESIGN.md §6).

You normally never need these commands: any eyedbg command that needs the daemon starts it
automatically, and it exits by itself after its idle timeout (default 30m) with no sessions. Use
them to check on it, to stop it, or to read its log when something goes wrong.

Set EYEDBG_NO_AUTOSTART=1 to make eyedbg fail instead of starting the daemon, and
EYEDBG_RUNTIME_DIR to move its socket, token, lock and log files (default: the user cache
directory, or $XDG_RUNTIME_DIR/eyedbg when set).

Without a subcommand, prints this help and exits 0; an unknown subcommand exits 1.`,
		Example: `  eyedbg daemon status          # is it running, and when will it exit?
  eyedbg daemon start           # make sure it is running
  eyedbg daemon stop            # stop it (refused while sessions exist)
  eyedbg daemon logs --tail 20  # last 20 lines of its log`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}

	cmd.AddCommand(
		newDaemonStartCommand(info, g),
		newDaemonStatusCommand(info, g),
		newDaemonStopCommand(info, g),
		newDaemonLogsCommand(g),
	)

	return cmd
}

func newDaemonStartCommand(info version.Info, g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the daemon if it is not running",
		Long: `Make sure eyedbgd is running and print its status. Idempotent: if a daemon of the same build
is already running, it is left alone.

If a daemon of a different build is running with no sessions, it is replaced; if it has sessions,
this fails with VERSION_MISMATCH instead of ending them. The new daemon runs detached from the
terminal, logs to its log file (see 'eyedbg daemon logs'), and exits after its idle timeout.

Blocks until the daemon accepts connections (at most 5s). Has no effect on debug sessions.
Ignores EYEDBG_NO_AUTOSTART, since starting is what was asked for. Output is the same as
'eyedbg daemon status', preceded by whether this call started it ("started": true in --json).
Exits 0 on success, 1 on failure (codes DAEMON_START_FAILED, VERSION_MISMATCH).`,
		Example: `  eyedbg daemon start
  eyedbg daemon start --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := daemon.DefaultPaths()
			if err != nil {
				return err
			}

			opts := daemon.OptionsFromEnv()
			opts.NoAutostart = false

			ctx, cancel := context.WithTimeout(cmd.Context(), daemonCallTimeout)
			defer cancel()

			cl, started, err := daemon.Connect(ctx, p, info, opts)
			if err != nil {
				return err
			}
			defer cl.Close()

			var st api.StatusResult
			if err := cl.Call(ctx, api.MethodDaemonStatus, nil, &st); err != nil {
				return err
			}

			return writeDaemonStatus(cmd.OutOrStdout(), newStatusView(&st, started, time.Now()), g.json)
		},
	}
}

func newDaemonStatusCommand(info version.Info, g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the daemon is running, and its state",
		Long: `Show whether eyedbgd is running and, if it is: pid, build, uptime, number of sessions, when it
will exit if idle, and where its files are.

Never starts the daemon and has no effect on debug sessions; returns within 10s. "Not running" is
a normal answer, not an error: it prints "eyedbgd is not running" ("running": false in --json)
and exits 0. Exits 1 only if the daemon is running but can't be queried (e.g. UNAUTHORIZED).`,
		Example: `  eyedbg daemon status
  eyedbg daemon status --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := daemon.DefaultPaths()
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), daemonCallTimeout)
			defer cancel()

			cl, err := daemon.Dial(ctx, p, info)
			if api.CodeOf(err) == api.CodeDaemonNotRunning {
				return writeDaemonStatus(cmd.OutOrStdout(), statusView{}, g.json)
			}

			if err != nil {
				return err
			}
			defer cl.Close()

			var st api.StatusResult
			if err := cl.Call(ctx, api.MethodDaemonStatus, nil, &st); err != nil {
				return err
			}

			return writeDaemonStatus(cmd.OutOrStdout(), newStatusView(&st, false, time.Now()), g.json)
		},
	}
}

func newDaemonStopCommand(info version.Info, g *globals) *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the daemon",
		Long: `Stop eyedbgd and wait until it has exited (at most 10s).

Refuses with SESSIONS_ACTIVE while debug sessions exist, unless --force, which ends them first and
kills their debuggees. Never starts the daemon: if it is not running, prints "eyedbgd is not
running" and exits 0, so it is safe to call unconditionally.

Output: "eyedbgd stopped" and the number of sessions ended ("stopped", "wasRunning" and
"endedSessions" in --json). Exits 0 on success, 1 on failure.`,
		Example: `  eyedbg daemon stop
  eyedbg daemon stop --force   # also end live debug sessions`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := daemon.DefaultPaths()
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), daemonCallTimeout)
			defer cancel()

			cl, err := daemon.Dial(ctx, p, info)
			if api.CodeOf(err) == api.CodeDaemonNotRunning {
				return writeStop(cmd.OutOrStdout(), stopOutput{}, g.json)
			}

			if err != nil {
				return err
			}

			var res api.StopResult

			err = cl.Call(ctx, api.MethodDaemonStop, api.StopParams{Force: force}, &res)
			_ = cl.Close()

			if err != nil {
				return err
			}

			if err := daemon.WaitStopped(ctx, p, daemonCallTimeout); err != nil {
				return err
			}

			return writeStop(cmd.OutOrStdout(), stopOutput{Stopped: true, WasRunning: true, EndedSessions: res.EndedSessions}, g.json)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "end live debug sessions (killing their debuggees) instead of refusing")

	return cmd
}

func newDaemonLogsCommand(g *globals) *cobra.Command {
	var tail int

	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Print the daemon's log",
		Long: `Print the last lines of eyedbgd's log file, which receives everything an auto-started daemon
writes (start-up, idle exit, rejected connections, errors). Read it when 'eyedbg daemon start'
fails or the daemon exits unexpectedly. A daemon started by hand logs to its terminal instead.

Reads a file only: never starts the daemon, never blocks, no effect on sessions. The log is kept
under 5 MiB, with one previous generation (eyedbgd.log.1). Output is the raw log lines ("path" and
"lines" in --json). Exits 1 if there is no log yet.`,
		Example: `  eyedbg daemon logs
  eyedbg daemon logs --tail 0   # the whole log`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := daemon.DefaultPaths()
			if err != nil {
				return err
			}

			lines, err := daemon.TailLog(p, tail)
			if err != nil {
				return err
			}

			return writeLogs(cmd.OutOrStdout(), p.Log, lines, g.json)
		},
	}

	cmd.Flags().IntVar(&tail, "tail", 50, "number of lines from the end to print; 0 prints the whole log")

	return cmd
}

// statusView is what daemon status/start render. The zero value means "not
// running".
type statusView struct {
	Schema      int        `json:"schema"`
	Running     bool       `json:"running"`
	Started     *bool      `json:"started,omitempty"`
	PID         int        `json:"pid,omitempty"`
	Version     string     `json:"version,omitempty"`
	Commit      string     `json:"commit,omitempty"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	Uptime      string     `json:"uptime,omitempty"`
	Sessions    int        `json:"sessions"`
	IdleTimeout string     `json:"idleTimeout,omitempty"`
	IdleExitAt  *time.Time `json:"idleExitAt,omitempty"`
	IdleExitIn  string     `json:"idleExitIn,omitempty"`
	Dir         string     `json:"dir,omitempty"`
	Socket      string     `json:"socket,omitempty"`
}

func newStatusView(st *api.StatusResult, started bool, now time.Time) statusView {
	v := statusView{
		Running:     true,
		Started:     &started,
		PID:         st.PID,
		Version:     st.Version,
		Commit:      st.Commit,
		StartedAt:   &st.StartedAt,
		Uptime:      now.Sub(st.StartedAt).Round(time.Second).String(),
		Sessions:    st.Sessions,
		IdleTimeout: time.Duration(st.IdleTimeout).String(),
		IdleExitAt:  st.IdleExitAt,
		Dir:         st.Dir,
		Socket:      st.Socket,
	}

	if st.IdleExitAt != nil {
		v.IdleExitIn = max(st.IdleExitAt.Sub(now), 0).Round(time.Second).String()
	}

	if st.IdleTimeout == 0 {
		v.IdleTimeout = "never"
	}

	return v
}

func writeDaemonStatus(w io.Writer, v statusView, asJSON bool) error {
	if asJSON {
		v.Schema = jsonSchemaVersion

		return writeJSON(w, v)
	}

	var b strings.Builder

	if !v.Running {
		b.WriteString("eyedbgd is not running\n")

		return writeText(w, b.String())
	}

	if v.Started != nil && *v.Started {
		b.WriteString("eyedbgd started\n")
	} else {
		b.WriteString("eyedbgd is running\n")
	}

	fmt.Fprintf(&b, "pid:      %d\n", v.PID)
	fmt.Fprintf(&b, "version:  %s (%s)\n", v.Version, v.Commit)
	fmt.Fprintf(&b, "uptime:   %s\n", v.Uptime)
	fmt.Fprintf(&b, "sessions: %d\n", v.Sessions)

	switch {
	case v.IdleExitIn != "":
		fmt.Fprintf(&b, "idle:     exits in %s unless a session starts (idle timeout %s)\n", v.IdleExitIn, v.IdleTimeout)
	case v.IdleTimeout == "never":
		b.WriteString("idle:     never exits by itself (idle timeout disabled)\n")
	default:
		fmt.Fprintf(&b, "idle:     exits %s after the last session ends\n", v.IdleTimeout)
	}

	fmt.Fprintf(&b, "dir:      %s\n", v.Dir)

	return writeText(w, b.String())
}

type stopOutput struct {
	Schema        int  `json:"schema"`
	Stopped       bool `json:"stopped"`
	WasRunning    bool `json:"wasRunning"`
	EndedSessions int  `json:"endedSessions"`
}

func writeStop(w io.Writer, out stopOutput, asJSON bool) error {
	if asJSON {
		out.Schema = jsonSchemaVersion

		return writeJSON(w, out)
	}

	if !out.WasRunning {
		return writeText(w, "eyedbgd is not running\n")
	}

	return writeText(w, fmt.Sprintf("eyedbgd stopped (ended %d session(s))\n", out.EndedSessions))
}

type logsOutput struct {
	Schema int      `json:"schema"`
	Path   string   `json:"path"`
	Lines  []string `json:"lines"`
}

func writeLogs(w io.Writer, path string, lines []string, asJSON bool) error {
	if asJSON {
		if lines == nil {
			lines = []string{}
		}

		return writeJSON(w, logsOutput{Schema: jsonSchemaVersion, Path: path, Lines: lines})
	}

	if len(lines) == 0 {
		return nil
	}

	return writeText(w, strings.Join(lines, "\n")+"\n")
}

func writeJSON(w io.Writer, v any) error {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return fmt.Errorf("write output: %w", err)
	}

	return nil
}

func writeText(w io.Writer, s string) error {
	if _, err := io.WriteString(w, s); err != nil {
		return fmt.Errorf("write output: %w", err)
	}

	return nil
}
