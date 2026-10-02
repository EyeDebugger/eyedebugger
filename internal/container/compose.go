// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// ComposeTimeout bounds one docker compose ps: compose loads the project's
// files first.
const ComposeTimeout = 30 * time.Second

// Limits of the compose flags that reach docker compose.
const (
	// maxComposePath bounds a --file or --project-directory value.
	maxComposePath = 4096
	// maxComposeFiles bounds how many --file flags are passed.
	maxComposeFiles = 16
)

// labelPrefixProject is the compose project label as it starts in the
// comma-joined Labels string of compose ps.
const labelPrefixProject = labelProject + "="

// ComposeOptions choose the compose project that ComposePS lists: the
// global flags of docker compose itself, each passed as one --flag=value
// argument. All empty is compose's own default (the current directory's
// compose files and COMPOSE_* variables).
type ComposeOptions struct {
	// Files are --file values.
	Files []string
	// ProjectName is a --project-name value.
	ProjectName string
	// ProjectDirectory is a --project-directory value.
	ProjectDirectory string
}

// ComposeService is one container of a compose project, as compose ps
// lists it. Only these fields are read: ps also prints each container's
// command, ports and labels, and none of that is stored or shown.
type ComposeService struct {
	// ID is the container id as ps prints it (short or full), Name its name.
	ID, Name string
	// Service and Project are the compose names.
	Service, Project string
	// State is the container's state ("running", "exited", ...).
	State string
}

// Validate checks every value that reaches docker compose's argv; none may
// be empty, hold a control character or exceed its bound.
func (o ComposeOptions) Validate() error {
	if len(o.Files) > maxComposeFiles {
		return api.NewError(api.CodeInvalidRequest, fmt.Sprintf("more than %d -f flags", maxComposeFiles), "")
	}

	for _, f := range o.Files {
		if err := checkComposePath("-f", f); err != nil {
			return err
		}
	}

	if o.ProjectDirectory != "" {
		if err := checkComposePath("--project-directory", o.ProjectDirectory); err != nil {
			return err
		}
	}

	if o.ProjectName != "" {
		if err := api.CheckGroup(o.ProjectName); err != nil {
			return api.NewError(api.CodeInvalidRequest, "invalid project name "+show(o.ProjectName, 40),
				"a compose project name is lowercase letters, digits, '_' and '-', starting with a letter or digit")
		}
	}

	return nil
}

func checkComposePath(flag, v string) error {
	if v == "" || len(v) > maxComposePath || strings.ContainsFunc(v, unicode.IsControl) || !utf8.ValidString(v) {
		return api.NewError(api.CodeInvalidRequest, "invalid "+flag+" value "+show(v, 40), "a file or directory path without control characters")
	}

	return nil
}

// psArgs is the argv (after the docker executable and engine flags) of the
// one compose call eyedbg makes: ps, never config (which inlines env_file
// values).
func (o ComposeOptions) psArgs() []string {
	args := []string{"compose"}

	for _, f := range o.Files {
		args = append(args, "--file="+f)
	}

	if o.ProjectName != "" {
		args = append(args, "--project-name="+o.ProjectName)
	}

	if o.ProjectDirectory != "" {
		args = append(args, "--project-directory="+o.ProjectDirectory)
	}

	return append(args, "ps", "--format", "json")
}

// ComposePS lists the containers of a compose project with one 'docker
// compose ps --format json', run in the caller's working directory and
// environment (compose finds its files there). Never 'compose config':
// that inlines env_file values. A missing project is INVALID_REQUEST, any
// other docker failure ATTACH_FAILED.
func (e Engine) ComposePS(ctx context.Context, o ComposeOptions) ([]ComposeService, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}

	res, fail := e.run(ctx, ComposeTimeout, nil, e.Args(o.psArgs()...))
	if fail != nil {
		return nil, composeError(fail)
	}

	list, err := parseComposePS(res.stdout)
	if err != nil {
		return nil, api.NewError(api.CodeAttachFailed, "unexpected answer from docker compose ps: "+err.Error(),
			"eyedbg reads a fixed set of fields; this compose may differ from the ones it was verified with (compose 2.23 to 2.26)")
	}

	return list, nil
}

// composeError is the error for a docker compose ps that failed.
func composeError(f *failure) error {
	if strings.Contains(strings.ToLower(f.stderr), "no configuration file") {
		return api.NewError(api.CodeInvalidRequest, "docker compose found no compose file here",
			"run it in the project's directory, or name the project with -f FILE, -p NAME or --project-directory DIR")
	}

	msg := "docker compose ps failed"
	if f.err != nil {
		msg += " (" + f.err.Error() + ")"
	}

	if f.stderr != "" {
		msg += ": " + shortStderr(f.stderr)
	}

	return api.NewError(api.CodeAttachFailed, msg,
		"is docker running with the compose plugin ('docker compose version'), and the project right? (name it with -f FILE, -p NAME or --project-directory DIR)")
}

// psRow is the part of a ps row that is read. Compose prints PascalCase keys
// ("ID", "Service"); encoding/json matches them to these tags regardless of
// case.
type psRow struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Service string `json:"service"`
	Project string `json:"project"`
	State   string `json:"state"`
	// Labels is a comma-joined list of every label; only the project label
	// is taken from it, when the row has no Project (compose before 2.24).
	Labels string `json:"labels"`
}

// parseComposePS decodes the output of compose ps --format json: a JSON
// array (older compose) or one object per line (newer), nothing at all for
// a project with no container. A row missing a required field, or holding
// one that fails its grammar, fails the whole parse.
func parseComposePS(out []byte) ([]ComposeService, error) {
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return nil, nil
	}

	var rows []psRow

	if out[0] == '[' {
		if err := json.Unmarshal(out, &rows); err != nil {
			return nil, errors.New("not a JSON array of containers")
		}
	} else {
		dec := json.NewDecoder(bytes.NewReader(out))

		for {
			var row psRow

			err := dec.Decode(&row)
			if errors.Is(err, io.EOF) {
				break
			}

			if err != nil {
				return nil, errors.New("not one JSON object per line")
			}

			rows = append(rows, row)
		}
	}

	if len(rows) == 0 {
		return nil, nil
	}

	list := make([]ComposeService, 0, len(rows))

	for n, r := range rows {
		s, err := r.service()
		if err != nil {
			return nil, fmt.Errorf("container %d: %w", n+1, err)
		}

		list = append(list, s)
	}

	return list, nil
}

// service is the validated row.
func (r psRow) service() (ComposeService, error) {
	if !isHexID(r.ID) {
		return ComposeService{}, errors.New("its ID is not a container id")
	}

	if ValidateRef(r.Name) != nil {
		return ComposeService{}, errors.New("its Name is not a container name")
	}

	if ValidateService(r.Service) != nil {
		return ComposeService{}, errors.New("its Service is not a compose service name")
	}

	if r.State == "" || len(r.State) > 32 || strings.ContainsFunc(r.State, unicode.IsControl) {
		return ComposeService{}, errors.New("its State is missing")
	}

	project := r.Project
	if project == "" {
		project = projectLabel(r.Labels)
	}

	if api.CheckGroup(project) != nil {
		return ComposeService{}, errors.New("its compose project is missing or not a valid project name")
	}

	return ComposeService{ID: r.ID, Name: r.Name, Service: r.Service, Project: project, State: r.State}, nil
}

// projectLabel is the value of the compose project label in a comma-joined
// Labels string, "" when absent. Other labels' values may hold commas
// (config_files does), so only an element that starts with the exact label
// key counts.
func projectLabel(labels string) string {
	for part := range strings.SplitSeq(labels, ",") {
		if v, ok := strings.CutPrefix(part, labelPrefixProject); ok {
			return v
		}
	}

	return ""
}
