// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package container_test

import (
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/container"
)

// injections are values that must never reach a docker argv as a flag or a
// second argument.
var injections = []string{
	"--privileged", "-v=/:/h", "-it", "--", "-", " leading", "trailing ", "with space", "new\nline", "tab\tx", "nul\x00x", "semi;colon",
	"a b", "$(id)", "`id`", "a/b", "a:b", "ü", "", "\x7f", "x\r",
}

func TestValidateRef(t *testing.T) {
	t.Parallel()

	ok := []string{"abc123def456", strings.Repeat("a", 64), "web", "My_App.1-x", "a", strings.Repeat("a", 128)}
	for _, v := range ok {
		if err := container.ValidateRef(v); err != nil {
			t.Errorf("ValidateRef(%q) = %v, want ok", v, err)
		}
	}

	bad := append([]string{strings.Repeat("a", 129), "_x", ".x", "-x", strings.Repeat("f", 65) + "!"}, injections...)
	for _, v := range bad {
		if api.CodeOf(container.ValidateRef(v)) != api.CodeInvalidRequest {
			t.Errorf("ValidateRef(%q) accepted, want INVALID_REQUEST", v)
		}
	}
}

func TestValidateContext(t *testing.T) {
	t.Parallel()

	for _, v := range []string{"default", "desktop-linux", "my.ctx+1", "a"} {
		if err := container.ValidateContext(v); err != nil {
			t.Errorf("ValidateContext(%q) = %v, want ok", v, err)
		}
	}

	for _, v := range append([]string{"_x", "x" + strings.Repeat("y", 128)}, injections...) {
		if api.CodeOf(container.ValidateContext(v)) != api.CodeInvalidRequest {
			t.Errorf("ValidateContext(%q) accepted", v)
		}
	}
}

func TestValidateHost(t *testing.T) {
	t.Parallel()

	for _, v := range []string{"unix:///var/run/docker.sock", "tcp://10.0.0.1:2376", "ssh://me@box", "npipe:////./pipe/docker_engine"} {
		if err := container.ValidateHost(v); err != nil {
			t.Errorf("ValidateHost(%q) = %v, want ok", v, err)
		}
	}

	bad := []string{"", "tcp", "--host=x://", "-H://x", "tcp://a b", "tcp://a\nb", "tcp://a\x00", "tcp://" + strings.Repeat("a", 1024), "unix://\x7f"}
	for _, v := range bad {
		if api.CodeOf(container.ValidateHost(v)) != api.CodeInvalidRequest {
			t.Errorf("ValidateHost(%q) accepted", v)
		}
	}
}

func TestValidateService(t *testing.T) {
	t.Parallel()

	for _, v := range []string{"web", "api-1", "Producer.v2", "a"} {
		if err := container.ValidateService(v); err != nil {
			t.Errorf("ValidateService(%q) = %v, want ok", v, err)
		}
	}

	for _, v := range append([]string{strings.Repeat("a", 64), "_x"}, injections...) {
		if api.CodeOf(container.ValidateService(v)) != api.CodeInvalidRequest {
			t.Errorf("ValidateService(%q) accepted", v)
		}
	}
}

func TestValidateSpec(t *testing.T) {
	t.Parallel()

	good := api.ContainerSpec{
		Engine: api.ContainerEngine{Host: "tcp://h:1"}, Ref: "web-1", PID: 7, Service: "web", Project: "my-app",
	}
	if err := container.ValidateSpec(&good); err != nil {
		t.Fatalf("ValidateSpec(good) = %v", err)
	}

	tests := map[string]func(*api.ContainerSpec){
		"ref":     func(s *api.ContainerSpec) { s.Ref = "--privileged" },
		"host":    func(s *api.ContainerSpec) { s.Engine.Host = "no scheme" },
		"context": func(s *api.ContainerSpec) { s.Engine.Context = "-x" },
		"service": func(s *api.ContainerSpec) { s.Service = "a b" },
		"project": func(s *api.ContainerSpec) { s.Project = "UPPER" },
		"pid":     func(s *api.ContainerSpec) { s.PID = -1 },
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s := good
			mutate(&s)

			if api.CodeOf(container.ValidateSpec(&s)) != api.CodeInvalidRequest {
				t.Errorf("ValidateSpec accepted a bad %s", name)
			}
		})
	}
}
