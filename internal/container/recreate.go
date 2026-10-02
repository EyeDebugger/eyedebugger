// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// Bounds of docker compose up.
const (
	// UpWaitTimeout is how long 'up --wait' waits for health (restore).
	UpWaitTimeout = 180 * time.Second
	// upSlack is how long past that (or past nothing, without --wait) the
	// call may take: compose pulls nothing here (--no-build, images exist)
	// but stops a container for up to its grace period.
	upSlack = 60 * time.Second
	// maxUpServices bounds the services of one call.
	maxUpServices = maxOverrideServices
)

// ProjectRef names a compose project the way the container was created: by
// the project name and directory, and the compose files and env files it was
// created from.
type ProjectRef struct {
	// Name is the compose project (--project-name), WorkDir its working
	// directory (--project-directory, absolute).
	Name, WorkDir string
	// Files are the compose files (--file, each absolute, at least one):
	// the container's own config_files label, never the CLI's discovery,
	// which any -f would switch off.
	Files []string
	// EnvFiles are --env-file values; made absolute.
	EnvFiles []string
}

// validate checks every value that reaches docker compose's argv.
func (r ProjectRef) validate() error {
	if err := api.CheckGroup(r.Name); err != nil {
		return api.NewError(api.CodeInvalidRequest, "invalid project name "+show(r.Name, 40),
			"a compose project name is lowercase letters, digits, '_' and '-', starting with a letter or digit")
	}

	if err := checkComposePath("--project-directory", r.WorkDir); err != nil || !filepath.IsAbs(r.WorkDir) {
		return api.NewError(api.CodeInvalidRequest, "invalid project directory "+show(r.WorkDir, 80), "the compose project's directory, as an absolute path")
	}

	if len(r.Files) == 0 || len(r.Files) > maxComposeFiles {
		return api.NewError(api.CodeInvalidRequest, fmt.Sprintf("a compose project needs 1 to %d compose files", maxComposeFiles), "")
	}

	for _, f := range r.Files {
		if !plainAbsPath(f) {
			return api.NewError(api.CodeInvalidRequest, "invalid compose file "+show(f, 80), "the container's own compose files are absolute paths")
		}
	}

	return ValidateEnvFiles(r.EnvFiles)
}

// ValidateEnvFiles checks --env-file values the way [Engine.ComposeUp] does, so
// a caller can refuse them before it changes anything.
func ValidateEnvFiles(files []string) error {
	if len(files) > maxComposeFiles {
		return api.NewError(api.CodeInvalidRequest, fmt.Sprintf("more than %d --env-file flags", maxComposeFiles), "")
	}

	for _, f := range files {
		if err := checkComposePath("--env-file", f); err != nil {
			return err
		}
	}

	return nil
}

// Validate checks every value of r that reaches docker compose's argv, as
// [Engine.ComposeUp] does before it runs, so a caller can refuse a project
// before it changes anything.
func (r ProjectRef) Validate() error { return r.validate() }

// upArgs is the argv (after the docker executable and engine flags) of
// compose up for services: detached, only those services (--no-deps),
// recreated, nothing built; override (when not "") as the last compose file;
// with wait, held until healthy. Every value is one --flag=value argument.
func (r ProjectRef) upArgs(override string, services []string, wait bool) ([]string, error) {
	envFiles := make([]string, 0, len(r.EnvFiles))

	for _, f := range r.EnvFiles {
		abs, err := filepath.Abs(f)
		if err != nil {
			return nil, fmt.Errorf("make %s absolute: %w", f, err)
		}

		envFiles = append(envFiles, abs)
	}

	args := []string{"compose", "--project-name=" + r.Name, "--project-directory=" + r.WorkDir}

	for _, f := range r.Files {
		args = append(args, "--file="+f)
	}

	if override != "" {
		args = append(args, "--file="+override)
	}

	for _, f := range envFiles {
		args = append(args, "--env-file="+f)
	}

	args = append(args, "up", "--detach", "--no-deps", "--force-recreate", "--no-build")

	if wait {
		args = append(args, "--wait", "--wait-timeout="+strconv.Itoa(int(UpWaitTimeout/time.Second)))
	}

	return append(args, services...), nil
}

// ComposeUp recreates services of the compose project ref with one
// 'docker compose up --detach --no-deps --force-recreate --no-build'
// (docs/adr/0021, D16): from ref's files and, when override is not "", that
// file after them; with wait, also --wait (a restore: the services come back
// healthy; entering fast mode passes false, since an idle container is
// unhealthy). It never runs compose config, down, rm, build or pull. The
// environment is the caller's (compose interpolates it). A failure is
// ATTACH_FAILED with compose's stderr tail (at most 4 KiB, control
// characters stripped); compose's stdout is neither kept nor shown.
func (e Engine) ComposeUp(ctx context.Context, ref ProjectRef, override string, services []string, wait bool) error {
	if err := ref.validate(); err != nil {
		return err
	}

	if override != "" && (!plainAbsPath(override) || slices.Contains(ref.Files, override)) {
		return api.NewError(api.CodeInvalidRequest, "invalid override file "+show(override, 80), "")
	}

	if len(services) == 0 || len(services) > maxUpServices {
		return api.NewError(api.CodeInvalidRequest, fmt.Sprintf("compose up needs 1 to %d services", maxUpServices), "")
	}

	for _, s := range services {
		if err := ValidateService(s); err != nil {
			return err
		}
	}

	args, err := ref.upArgs(override, services, wait)
	if err != nil {
		return err
	}

	timeout := UpWaitTimeout + upSlack
	if !wait {
		timeout = 2 * upSlack
	}

	if _, fail := e.run(ctx, timeout, nil, e.Args(args...)); fail != nil {
		msg := "docker compose up failed"
		if fail.err != nil {
			msg += " (" + fail.err.Error() + ")"
		}

		if fail.stderr != "" {
			msg += ": " + fail.stderr
		}

		return api.NewError(api.CodeAttachFailed, msg,
			"see 'docker compose ps' for the services' state; 'eyedbg compose restore SERVICE' goes back to the image's own entrypoint")
	}

	return nil
}
