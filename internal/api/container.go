// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "strings"

// MethodContainerAttach attaches to one or more containers, one session
// each (docs/adr/0020, D6). It never travels through [MethodSessionStart]:
// an older daemon would drop an unknown field of those params silently and
// attach to a host process; an unknown method fails with UNKNOWN_METHOD.
const MethodContainerAttach = "container.attach"

// PathMapping pairs a directory in a container with the host directory
// holding the same sources (docs/adr/0020, D5): Remote is an absolute
// POSIX path as the debug adapter in the container names sources, Local an
// absolute host directory.
type PathMapping struct {
	Remote string `json:"remote"`
	Local  string `json:"local"`
}

// ContainerEngine says which docker engine a container lives in; both empty
// is the docker CLI's own default. Host (a DOCKER_HOST value) wins over
// Context (a DOCKER_CONTEXT value).
type ContainerEngine struct {
	Host    string `json:"host,omitempty"`
	Context string `json:"context,omitempty"`
}

// ContainerSpec names the container to attach to.
type ContainerSpec struct {
	Engine ContainerEngine `json:"engine,omitzero"`
	// Ref is the container's id (12 to 64 hex digits) or name.
	Ref string `json:"ref"`
	// PID is the process to attach to, as numbered inside the container; 0
	// means 1.
	PID int `json:"pid,omitempty"`
	// Map is the path map ([PathMapping]); empty means the default, if the
	// container has one.
	Map []PathMapping `json:"map,omitempty"`
	// Service and Project are the member's compose names, for the result.
	Service string `json:"service,omitempty"`
	Project string `json:"project,omitempty"`
	// RequireDotnet makes a container whose pid 1 doesn't run dotnet, or one
	// in eyedbg's fast mode, a skipped member rather than a failed one: the
	// member was picked implicitly.
	RequireDotnet bool `json:"requireDotnet,omitempty"`
}

// ContainerInfo describes the container a session debugs
// ([SessionInfo.Container]).
type ContainerInfo struct {
	// ID is the full container id.
	ID   string `json:"id"`
	Name string `json:"name"`
	// Service and Project are the container's compose labels, when it has
	// them.
	Service string `json:"service,omitempty"`
	Project string `json:"project,omitempty"`
	// PID is the debugged process, as numbered inside the container (the
	// session's own pid is omitted: it is not a host process).
	PID int `json:"pid"`
	// Platform is the image's os/arch, e.g. linux/arm64.
	Platform string        `json:"platform"`
	Map      []PathMapping `json:"map,omitempty"`
	// UnhealthyAfter is how long a stop must last before the container's own
	// healthcheck says unhealthy; omitted without a healthcheck.
	UnhealthyAfter Duration `json:"unhealthyAfter,omitempty"`
}

// ContainerAttachParams are the params of [MethodContainerAttach]: attach to
// every member, each as its own session of Group, with the same breakpoints,
// exception mode, lease policy and adapter.
type ContainerAttachParams struct {
	DumpSpec

	Client      string           `json:"client,omitempty"`
	Lang        string           `json:"lang"`
	Group       string           `json:"group,omitempty"`
	Members     []ContainerSpec  `json:"members"`
	Breakpoints []BreakpointSpec `json:"breakpoints,omitempty"`
	Exceptions  ExceptionMode    `json:"exceptions,omitempty"`
	LeasePolicy LeasePolicy      `json:"leasePolicy,omitempty"`
	NoRecord    bool             `json:"noRecord,omitempty"`
	Adapter     string           `json:"adapter,omitempty"`
	// Wait is how long to wait for a first stop when there are breakpoints;
	// honored only for a single member.
	Wait Duration `json:"wait,omitempty"`
}

// ContainerMemberResult is one member's outcome: exactly one of Session,
// Error and Skipped is set.
type ContainerMemberResult struct {
	Service   string    `json:"service,omitempty"`
	Container string    `json:"container"`
	Session   *Snapshot `json:"session,omitempty"`
	Error     *Error    `json:"error,omitempty"`
	// Skipped is why an implicitly picked member isn't a candidate.
	Skipped string `json:"skipped,omitempty"`
}

// ContainerAttachResult is the result of [MethodContainerAttach]: the
// members in the order of the request.
type ContainerAttachResult struct {
	Members []ContainerMemberResult `json:"members"`
}

// maxGroupLen is the longest group name.
const maxGroupLen = 63

// CheckGroup checks a group name: lowercase letters, digits, '_' and '-',
// starting with a letter or digit, at most 63 characters (a compose project
// name).
func CheckGroup(g string) error {
	ok := g != "" && len(g) <= maxGroupLen && strings.IndexFunc(g[:1], isLowerAlnum) == 0

	for i := 0; ok && i < len(g); i++ {
		ok = isLowerAlnum(rune(g[i])) || g[i] == '_' || g[i] == '-'
	}

	if !ok {
		return NewError(CodeInvalidRequest, "invalid group name "+quoteShort(g),
			"a group name is lowercase letters, digits, '_' and '-', starting with a letter or digit, at most 63 characters")
	}

	return nil
}

func isLowerAlnum(r rune) bool { return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') }

// quoteShort is s quoted for a message, cut to 40 bytes, with control
// characters shown as spaces.
func quoteShort(s string) string {
	const maxShown = 40

	if len(s) > maxShown {
		s = s[:maxShown] + "..."
	}

	return `"` + strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}

		return r
	}, s) + `"`
}
