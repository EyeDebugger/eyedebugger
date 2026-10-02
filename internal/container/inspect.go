// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// Labels eyedbg reads: compose's, and the one eyedbg's own fast mode puts on
// the containers it changes (docs/adr/0021).
const (
	labelProject    = "com.docker.compose.project"
	labelService    = "com.docker.compose.service"
	labelWorkingDir = "com.docker.compose.project.working_dir"
	labelFastMode   = "dev.izzat.eyedbg.fast.override"
)

// inspectTemplate selects, from docker's inspect output, the only fields
// eyedbg reads of a container: a JSON array of
//
//	id, name, image id, path (the executable of pid 1), running, paused,
//	restarting, init, healthcheck [interval ns, timeout ns, retries, test is
//	NONE] or null, compose project, compose service, compose working dir,
//	eyedbg's fast-mode label (13 values)
//
// It never names the environment, the command, the entrypoint's arguments
// or a healthcheck's command: docker doesn't send them. Docker's template
// engine fails on a missing key, and a container may have no Healthcheck, a
// healthcheck without Interval, Timeout, Retries or Test, or no Init: every
// such key is read with index, which yields null for a missing one.
const inspectTemplate = `[{{json .Id}},{{json .Name}},{{json .Image}},{{json .Path}},` +
	`{{json .State.Running}},{{json .State.Paused}},{{json .State.Restarting}},{{json (index .HostConfig "Init")}},` +
	`{{with index .Config "Healthcheck"}}[{{json (index . "Interval")}},{{json (index . "Timeout")}},{{json (index . "Retries")}},` +
	`{{with index . "Test"}}{{eq (index . 0) "NONE"}}{{else}}false{{end}}]{{else}}null{{end}},` +
	`{{json (index .Config.Labels "` + labelProject + `")}},{{json (index .Config.Labels "` + labelService + `")}},` +
	`{{json (index .Config.Labels "` + labelWorkingDir + `")}},{{json (index .Config.Labels "` + labelFastMode + `")}}]`

// imageTemplate selects an image's os, architecture and variant.
const imageTemplate = `{{.Os}}/{{.Architecture}}/{{.Variant}}`

// inspectFields is how many values inspectTemplate emits.
const inspectFields = 13

// maxLabel bounds a label value that is kept.
const maxLabel = 4096

// Health is a container's healthcheck timing. Docker's defaults apply to a
// field the container leaves at zero.
type Health struct {
	Interval, Timeout time.Duration
	Retries           int
}

// Docker's healthcheck defaults.
const (
	defaultInterval = 30 * time.Second
	defaultTimeout  = 30 * time.Second
	defaultRetries  = 3
)

// UnhealthyAfter is how long a stopped program takes to turn the container
// unhealthy: retries x interval + timeout.
func (h Health) UnhealthyAfter() time.Duration {
	interval, timeout, retries := h.Interval, h.Timeout, h.Retries

	if interval <= 0 {
		interval = defaultInterval
	}

	if timeout <= 0 {
		timeout = defaultTimeout
	}

	if retries <= 0 {
		retries = defaultRetries
	}

	return time.Duration(retries)*interval + timeout
}

// Info is what eyedbg reads of a container.
type Info struct {
	// ID is the full container id, Name its name without the leading slash,
	// Image its image's id.
	ID, Name, Image string
	// Path is the executable of the container's main process (pid 1), as
	// docker names it; use [Info.ProcessName] to show it.
	Path string
	// Running, Paused and Restarting are the container's state flags.
	Running, Paused, Restarting bool
	// Init is whether docker runs an init process (docker-init) as pid 1.
	Init bool
	// Health is the healthcheck's timing; nil: none (or NONE).
	Health *Health
	// Project, Service and WorkingDir are the compose labels; empty when the
	// container has none, or one that fails its grammar.
	Project, Service, WorkingDir string
	// FastMode: the container carries eyedbg's fast-mode label.
	FastMode bool
}

// ProcessName is the base name of pid 1's executable, fit to show.
func (i Info) ProcessName() string {
	const maxName = 64

	name := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}

		return r
	}, strings.ToValidUTF8(path.Base(i.Path), "?"))

	if len(name) > maxName {
		name = name[:maxName]
	}

	return name
}

// UnhealthyAfter is [Health.UnhealthyAfter] of the container's healthcheck;
// zero for a container that has none.
func (i Info) UnhealthyAfter() time.Duration {
	if i.Health == nil {
		return 0
	}

	return i.Health.UnhealthyAfter()
}

// Inspect reads container ref (an id or a name) with one docker inspect.
// A missing container is INVALID_REQUEST, any other failure ATTACH_FAILED.
func (e Engine) Inspect(ctx context.Context, ref string) (Info, error) {
	out, err := e.inspectOne(ctx, ref, inspectTemplate)
	if err != nil {
		return Info{}, err
	}

	info, err := parseInspect(out)
	if err != nil {
		return Info{}, api.NewError(api.CodeAttachFailed, "unexpected answer from docker inspect: "+err.Error(),
			"eyedbg reads a fixed set of fields; this docker may differ from the ones it was verified with (docker 24 to 26)")
	}

	return info, nil
}

// ImagePlatform is the os, architecture and variant (often empty) of the
// image with id image, from one docker image inspect.
func (e Engine) ImagePlatform(ctx context.Context, image string) (goos, arch, variant string, err error) {
	if !validImageID(image) {
		return "", "", "", api.NewError(api.CodeAttachFailed, "unexpected image id "+show(image, 80)+" in docker's answer", "")
	}

	res, fail := e.run(ctx, QueryTimeout, nil, e.Args("image", "inspect", "--format", imageTemplate, "--", image))
	if fail != nil {
		return "", "", "", dockerError(fail)
	}

	parts := strings.Split(strings.TrimSpace(string(res.stdout)), "/")
	if len(parts) != 3 || !grammar(parts[0], isLowerDigit, isLowerDigit) || !grammar(parts[1], isLowerDigit, isLowerDigit) ||
		(parts[2] != "" && !grammar(parts[2], isLowerDigit, isLowerDigit)) {
		return "", "", "", api.NewError(api.CodeAttachFailed, "unexpected platform from docker image inspect: "+show(string(res.stdout), 60), "")
	}

	return parts[0], parts[1], parts[2], nil
}

func isLowerDigit(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') }

// validImageID accepts an image id as docker prints it: "sha256:" and 64 hex
// digits, or a bare hex id.
func validImageID(s string) bool {
	if hex, ok := strings.CutPrefix(s, "sha256:"); ok {
		return isFullID(hex)
	}

	return isHexID(s)
}

// dockerError is the error for a docker invocation that failed.
func dockerError(f *failure) error {
	msg := "docker " + f.sub + " failed"
	if f.err != nil {
		msg += " (" + f.err.Error() + ")"
	}

	if f.stderr != "" {
		msg += ": " + shortStderr(f.stderr)
	}

	return api.NewError(api.CodeAttachFailed, msg,
		"is docker running, and the engine the right one? (DOCKER_HOST / DOCKER_CONTEXT of the eyedbg command are used; eyedbgd runs "+EnvDocker+" or the docker on its PATH)")
}

// parseInspect decodes inspectTemplate's output, failing closed on any
// value of the wrong type or shape. A compose label that fails its grammar
// is dropped (they only label what is shown); every other field must hold.
func parseInspect(out []byte) (Info, error) {
	var fields []json.RawMessage
	if err := json.Unmarshal(out, &fields); err != nil {
		return Info{}, errors.New("not a JSON array")
	}

	if len(fields) != inspectFields {
		return Info{}, fmt.Errorf("%d values, want %d", len(fields), inspectFields)
	}

	var (
		i                      Info
		init                   *bool
		health                 []json.RawMessage
		project, service, wdir string
		fastMode               string
	)

	dst := []any{
		&i.ID, &i.Name, &i.Image, &i.Path, &i.Running, &i.Paused, &i.Restarting, &init, &health,
		&project, &service, &wdir, &fastMode,
	}

	for n, d := range dst {
		if err := json.Unmarshal(fields[n], d); err != nil {
			return Info{}, fmt.Errorf("value %d: %w", n+1, err)
		}
	}

	i.Init = init != nil && *init
	i.Name = strings.TrimPrefix(i.Name, "/")

	if !isFullID(i.ID) || !validImageID(i.Image) || ValidateRef(i.Name) != nil {
		return Info{}, errors.New("the container's id, image or name is not in docker's usual form")
	}

	var err error
	if i.Health, err = parseHealth(health); err != nil {
		return Info{}, err
	}

	if api.CheckGroup(project) == nil {
		i.Project = project
	}

	if ValidateService(service) == nil {
		i.Service = service
	}

	if wdir != "" && len(wdir) <= maxLabel && !strings.ContainsFunc(wdir, unicode.IsControl) {
		i.WorkingDir = wdir
	}

	i.FastMode = fastMode != ""

	return i, nil
}

// parseHealth reads the healthcheck values: nil for none (null), or for a
// check whose test is NONE.
func parseHealth(v []json.RawMessage) (*Health, error) {
	if v == nil {
		return nil, nil //nolint:nilnil // No healthcheck is not an error.
	}

	const healthFields = 4

	if len(v) != healthFields {
		return nil, fmt.Errorf("healthcheck has %d values, want %d", len(v), healthFields)
	}

	var (
		interval, timeout, retries *float64
		none                       bool
	)

	for n, d := range []any{&interval, &timeout, &retries, &none} {
		if err := json.Unmarshal(v[n], d); err != nil {
			return nil, fmt.Errorf("healthcheck value %d: %w", n+1, err)
		}
	}

	if none {
		return nil, nil //nolint:nilnil // A NONE test disables the check.
	}

	h := &Health{}

	// Bounded so UnhealthyAfter can't overflow.
	const maxSpan, maxRetries = float64(24 * time.Hour), 1000

	if interval != nil && *interval > 0 {
		h.Interval = time.Duration(min(*interval, maxSpan))
	}

	if timeout != nil && *timeout > 0 {
		h.Timeout = time.Duration(min(*timeout, maxSpan))
	}

	if retries != nil && *retries > 0 {
		h.Retries = int(min(*retries, maxRetries))
	}

	return h, nil
}
