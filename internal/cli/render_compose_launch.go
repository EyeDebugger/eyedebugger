// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// Text and JSON renderers of 'compose launch' and 'compose restore'.

// launchedMember is one service of 'compose launch' in the output: the shape of
// 'compose attach”s member, plus what the launch did.
type launchedMember struct {
	attachedMember

	Fast *launchFast `json:"fast,omitempty"`
}

// launchFast is what a launch did to a service that now runs.
type launchFast struct {
	// Project is the project file's path below the compose directory.
	Project string `json:"project,omitempty"`
	// BuildLog is the build's log; empty when nothing was built (--no-build).
	BuildLog string `json:"buildLog,omitempty"`
	// Override is the override file the service's container was created with.
	Override string `json:"override"`
	// Recreated: the container was recreated (the service entered fast mode).
	Recreated  bool  `json:"recreated"`
	DurationMs int64 `json:"durationMs,omitempty"`
	// Carried is how many breakpoints came over from the service's old
	// sessions; Dropped how many couldn't (the file is gone).
	Carried int `json:"carried"`
	Dropped int `json:"dropped,omitempty"`
}

// launchView is what the text says besides the members.
type launchView struct {
	Override, BuildLog string
	// Recreated: some launched service's container was recreated.
	Recreated bool
	// Launched are the services that now run; Down those whose app was
	// stopped by the run and that did not come back.
	Launched, Down []string
}

// launchedCount is how many members have a session.
func launchedCount(members []launchedMember) int {
	n := 0

	for i := range members {
		if members[i].Session != nil {
			n++
		}
	}

	return n
}

// fastModeNote says once what fast mode changes about a service's life.
const fastModeNote = "note: in fast mode a service's app runs only while its eyedbg session runs it: 'eyedbg stop' (or 'compose stop') kills " +
	"the app and leaves the container idle and, after its healthcheck's interval, unhealthy, until the next 'eyedbg compose launch' " +
	"or 'eyedbg compose restore'. Its output is in 'eyedbg output -s ID' and 'eyedbg compose events --kind output', not in 'docker compose logs'."

// recreatedNote says what a recreate read.
const recreatedNote = "note: recreated with this shell's compose environment (variables and --env-file files); " +
	"whatever the old containers wrote outside volumes is gone."

// writeComposeLaunch renders the result of 'compose launch'.
func writeComposeLaunch(w io.Writer, group string, members []launchedMember, v launchView, asJSON bool, base string) error {
	if asJSON {
		return writeJSON(w, struct {
			Schema  int              `json:"schema"`
			Group   string           `json:"group"`
			Members []launchedMember `json:"members"`
		}{jsonSchemaVersion, group, members})
	}

	var b strings.Builder

	n := launchedCount(members)
	fmt.Fprintf(&b, "group %s: launched %d of %d service(s)\n", group, n, len(members))

	rows := make([][]string, len(members))
	plain := make([]attachedMember, len(members))

	for i := range members {
		rows[i] = launchRow(&members[i])
		plain[i] = members[i].attachedMember
	}

	writeTable(&b, 2, rows)

	if len(v.Down) > 0 {
		b.WriteString("down: " + strings.Join(v.Down, ", ") + ": the app was stopped by this run and did not come back; 'eyedbg compose launch " +
			strings.Join(v.Down, " ") + "' tries again ('--no-build' relaunches the last build)\n")
	}

	if n == 0 {
		return writeText(w, b.String())
	}

	writeAttachMaps(&b, plain)
	writeAttachBreakpoints(&b, plain, base)

	b.WriteString("override: " + v.Override + "\n")

	if v.BuildLog != "" {
		b.WriteString("build log: " + v.BuildLog + "\n")
	}

	if v.Recreated {
		b.WriteString(recreatedNote + "\n")
	}

	b.WriteString(fastModeNote + "\n")
	b.WriteString("undo with: eyedbg compose restore " + strings.Join(v.Launched, " ") + "\n")
	b.WriteString("next: eyedbg compose wait -g " + group + "  |  eyedbg compose bp add FILE:LINE -g " + group + "  |  eyedbg compose events --kind output -g " + group +
		"  |  eyedbg compose stop -g " + group + "\n")

	return writeText(w, b.String())
}

// launchRow is a member's line of the launch table.
func launchRow(m *launchedMember) []string {
	name := firstNonEmpty(m.Service, m.Container)

	switch {
	case m.Session != nil:
		return []string{name, m.Session.ID, "launched", launchDetail(m)}
	case m.Skipped != "":
		return []string{name, "-", rowSkipped, m.Skipped}
	case m.Error != nil:
		return []string{name, "-", rowFailed, errorLine(m.Error)}
	default:
		return []string{name, "-", rowNoAnswer, ""}
	}
}

// launchDetail is what happened to a service that now runs: "built in 2.1s ->
// recreated -> launched (2 breakpoints carried); container NAME".
func launchDetail(m *launchedMember) string {
	var steps []string

	if f := m.Fast; f != nil {
		switch {
		case f.BuildLog != "":
			steps = append(steps, "built in "+formatSeconds(f.DurationMs))
		default:
			steps = append(steps, "from the last build")
		}

		if f.Recreated {
			steps = append(steps, "recreated")
		}
	}

	detail := strings.Join(append(steps, "launched"), " -> ")

	if f := m.Fast; f != nil && f.Carried > 0 {
		detail += " (" + countOf(f.Carried, "breakpoint") + " carried"
		if f.Dropped > 0 {
			detail += ", " + strconv.Itoa(f.Dropped) + " dropped: its file is gone or outside the map"
		}

		detail += ")"
	} else if f != nil && f.Dropped > 0 {
		detail += " (" + countOf(f.Dropped, "carried breakpoint") + " dropped: the file is gone or outside the map)"
	}

	detail += "; container " + m.Container

	if c := m.Session.Container; c != nil && c.UnhealthyAfter > 0 {
		detail += "; its healthcheck fails after " + time.Duration(c.UnhealthyAfter).String() + " of a stop"
	}

	return detail
}

// countOf is "1 thing" or "N things".
func countOf(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}

	return strconv.Itoa(n) + " " + noun + "s"
}

// restoredMember is one service of 'compose restore' in the output.
type restoredMember struct {
	Service   string `json:"service"`
	Container string `json:"container,omitempty"`
	Restored  bool   `json:"restored"`
	// Sessions are the ids of the eyedbg sessions the restore ended.
	Sessions []string   `json:"sessions,omitempty"`
	Skipped  string     `json:"skipped,omitempty"`
	Error    *api.Error `json:"error,omitempty"`
}

// writeComposeRestore renders the result of 'compose restore'.
func writeComposeRestore(w io.Writer, group string, members []restoredMember, removed, asJSON bool) error {
	if asJSON {
		return writeJSON(w, struct {
			Schema  int              `json:"schema"`
			Group   string           `json:"group"`
			Members []restoredMember `json:"members"`
			Removed bool             `json:"removed"`
		}{jsonSchemaVersion, group, members, removed})
	}

	if len(members) == 0 {
		return writeText(w, "group "+group+": no service is in fast mode\n")
	}

	var b strings.Builder

	n := 0

	for i := range members {
		if members[i].Restored {
			n++
		}
	}

	fmt.Fprintf(&b, "group %s: restored %d of %d service(s)\n", group, n, len(members))

	rows := make([][]string, len(members))

	for i := range members {
		rows[i] = restoreRow(&members[i])
	}

	writeTable(&b, 2, rows)

	if n > 0 {
		b.WriteString("note: recreated as built, with this shell's compose environment (variables and --env-file files); " +
			"whatever the old containers wrote outside volumes is gone.\n")
	}

	if removed {
		b.WriteString("eyedbg's files for this project were removed: no service is in fast mode.\n")
	}

	return writeText(w, b.String())
}

// restoreRow is a member's line of the restore table.
func restoreRow(m *restoredMember) []string {
	switch {
	case m.Restored:
		detail := "recreated as built"
		if len(m.Sessions) > 0 {
			detail += "; ended " + countOf(len(m.Sessions), "session") + " (" + strings.Join(m.Sessions, ", ") + ")"
		}

		return []string{m.Service, "restored", detail}
	case m.Skipped != "":
		return []string{m.Service, rowSkipped, m.Skipped}
	case m.Error != nil:
		return []string{m.Service, rowFailed, errorLine(m.Error)}
	default:
		return []string{m.Service, rowNoAnswer, ""}
	}
}
