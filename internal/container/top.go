// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// maxProcs bounds how many processes of a container Top reads.
const maxProcs = 10000

// Proc is one process of a container, as docker top -o pid,stat,comm lists
// it: the name only, never a command line (which may hold secrets).
type Proc struct {
	// PID is the process id (the host's, as docker top shows it), Stat its
	// ps state ("S", "Ssl", "Z", ...), Comm its executable's name.
	PID  int
	Stat string
	Comm string
}

// Top lists the processes of the container ref (an id or a name) with one
// docker top ID -o pid,stat,comm: the header is skipped, and a line that
// isn't a pid, a state and a name fails the whole answer. Command lines are
// never asked for.
func (e Engine) Top(ctx context.Context, ref string) ([]Proc, error) {
	if err := ValidateRef(ref); err != nil {
		return nil, err
	}

	res, fail := e.run(ctx, QueryTimeout, nil, e.Args("top", ref, "-o", "pid,stat,comm"))
	if fail != nil {
		return nil, dockerError(fail)
	}

	procs, err := parseTop(res.stdout)
	if err != nil {
		return nil, api.NewError(api.CodeAttachFailed, "unexpected answer from docker top: "+err.Error(),
			"eyedbg reads a fixed set of fields; this docker may differ from the ones it was verified with (docker 24 to 26)")
	}

	return procs, nil
}

// parseTop reads docker top's table: a header whose first two columns are
// PID and STAT (any case), then one row per process.
func parseTop(out []byte) ([]Proc, error) {
	lines := strings.Split(strings.TrimRight(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n"), "\n")

	if len(lines) == 0 || lines[0] == "" {
		return nil, errors.New("no header")
	}

	head := strings.Fields(lines[0])
	if len(head) < 3 || !strings.EqualFold(head[0], "PID") || !strings.EqualFold(head[1], "STAT") {
		return nil, errors.New("the header is not PID STAT COMMAND")
	}

	if len(lines)-1 > maxProcs {
		return nil, fmt.Errorf("more than %d processes", maxProcs)
	}

	procs := make([]Proc, 0, len(lines)-1)

	for n, line := range lines[1:] {
		p, err := parseTopRow(line)
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", n+1, err)
		}

		procs = append(procs, p)
	}

	return procs, nil
}

// parseTopRow reads "<pid> <stat> <comm>", the name being the rest of the
// line (it may hold spaces).
func parseTopRow(line string) (Proc, error) {
	if strings.ContainsFunc(line, func(r rune) bool { return r != '\t' && unicode.IsControl(r) }) {
		return Proc{}, errors.New("a control character")
	}

	fields := strings.Fields(line)
	if len(fields) < 3 {
		return Proc{}, errors.New("fewer than 3 columns")
	}

	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		return Proc{}, errors.New("the process id is not a positive number")
	}

	return Proc{PID: pid, Stat: fields[1], Comm: strings.Join(fields[2:], " ")}, nil
}

// dotnetComm is the executable name of a running .NET app (and of the
// dotnet host); the kernel cuts comm at 15 bytes, which "dotnet" fits.
const dotnetComm = "dotnet"

// Strays are the processes of procs that are an earlier app: named exactly
// "dotnet" (not "dotnet-counters") and not a zombie.
func Strays(procs []Proc) []Proc {
	var strays []Proc

	for _, p := range procs {
		if p.Comm == dotnetComm && !strings.HasPrefix(p.Stat, "Z") {
			strays = append(strays, p)
		}
	}

	return strays
}

// ProbeIdle checks that the as-built container ref has what the idle
// entrypoint needs, by running 'docker exec REF tail --version' (as the
// container's own user, within [QueryTimeout]): a missing 'tail' (chiseled
// and distroless images) is INVALID_REQUEST, any other docker failure
// ATTACH_FAILED.
func (e Engine) ProbeIdle(ctx context.Context, ref string) error {
	if err := ValidateRef(ref); err != nil {
		return err
	}

	_, fail := e.run(ctx, QueryTimeout, nil, e.Args("exec", ref, "tail", "--version"))
	if fail == nil {
		return nil
	}

	if lower := strings.ToLower(fail.stderr); strings.Contains(lower, "not found") || strings.Contains(lower, "no such file") {
		return api.NewError(api.CodeInvalidRequest, "the image has no 'tail': chiseled or distroless images can't idle ("+shortStderr(fail.stderr)+")",
			"fast mode idles the container on 'tail -f /dev/null'; use an image with coreutils, or debug it as built with 'eyedbg attach dotnet --container'")
	}

	return dockerError(fail)
}
