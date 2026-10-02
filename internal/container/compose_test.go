// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"reflect"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// psObject is a compose ps row as compose 2.26 prints it (with Project and
// a Command that must never surface).
func psObject(id, name, service, state string) string {
	return `{"Command":"\"dotnet Web.dll --secret-token=hunter2\"","CreatedAt":"2026-10-01","ID":"` + id + `","Image":"web:latest",` +
		`"Labels":"com.docker.compose.project=myapp,com.docker.compose.service=` + service + `","Name":"` + name + `","Project":"myapp",` +
		`"Service":"` + service + `","State":"` + state + `","Status":"Up 2 minutes"}`
}

func TestParseComposePS(t *testing.T) {
	t.Parallel()

	const (
		idA = "0123456789ab"
		idB = "fedcba987654"
	)

	web := ComposeService{ID: idA, Name: "myapp-web-1", Service: "web", Project: "myapp", State: "running"}
	db := ComposeService{ID: idB, Name: "myapp-db-1", Service: "db", Project: "myapp", State: "exited"}

	tests := []struct {
		name string
		out  string
		want []ComposeService
	}{
		{"nothing", "", nil},
		{"whitespace", " \n", nil},
		{"empty array", "[]\n", nil},
		{"ndjson", psObject(idA, "myapp-web-1", "web", "running") + "\n" + psObject(idB, "myapp-db-1", "db", "exited") + "\n", []ComposeService{web, db}},
		{"array", "[" + psObject(idA, "myapp-web-1", "web", "running") + "," + psObject(idB, "myapp-db-1", "db", "exited") + "]", []ComposeService{web, db}},
		{
			"no Project key: the label (compose 2.23)",
			`{"ID":"` + idA + `","Name":"myapp-web-1","Service":"web","State":"running",` +
				`"Labels":"com.docker.compose.project.config_files=/a/compose.yml,/b/com.docker.compose.project=evil,com.docker.compose.project=myapp,x=y"}`,
			[]ComposeService{web},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseComposePS([]byte(tt.out))
			if err != nil {
				t.Fatal(err)
			}

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseComposePS = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestComposeServiceHasNoCommand keeps the container's command (which may
// hold secrets) out of what is stored: the type has exactly the fields the
// commands show.
func TestComposeServiceHasNoCommand(t *testing.T) {
	t.Parallel()

	typ := reflect.TypeFor[ComposeService]()
	for f := range typ.Fields() {
		switch f.Name {
		case "ID", "Name", "Service", "Project", "State":
		default:
			t.Errorf("ComposeService has field %s", f.Name)
		}
	}

	got, err := parseComposePS([]byte(psObject("0123456789ab", "myapp-web-1", "web", "running")))
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(strings.Join([]string{got[0].ID, got[0].Name, got[0].Service, got[0].Project, got[0].State}, " "), "hunter2") {
		t.Errorf("the command leaked into %+v", got[0])
	}
}

func TestParseComposePSErrors(t *testing.T) {
	t.Parallel()

	good := psObject("0123456789ab", "myapp-web-1", "web", "running")

	tests := []struct {
		name, out string
	}{
		{"not json", "hello"},
		{"broken array", "[{"},
		{"broken line", good + "\n{broken"},
		{"missing id", strings.Replace(good, `"ID":"0123456789ab"`, `"ID":""`, 1)},
		{"short id", strings.Replace(good, "0123456789ab", "0123", 1)},
		{"upper-case id", strings.Replace(good, "0123456789ab", "0123456789AB", 1)},
		{"flag as name", strings.Replace(good, "myapp-web-1", "--privileged", 1)},
		{"flag as service", strings.ReplaceAll(good, `"web"`, `"-v=/:/h"`)},
		{"service with a space", strings.Replace(good, `"Service":"web"`, `"Service":"we b"`, 1)},
		{"no state", strings.Replace(good, `"State":"running"`, `"State":""`, 1)},
		{"no project", strings.Replace(strings.Replace(good, `"Project":"myapp"`, `"Project":""`, 1), "com.docker.compose.project=", "x=", 1)},
		{"bad project", strings.Replace(good, `"Project":"myapp"`, `"Project":"My App"`, 1)},
		{"array of strings", `["a"]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got, err := parseComposePS([]byte(tt.out)); err == nil {
				t.Errorf("parseComposePS = %+v, want an error", got)
			}
		})
	}
}

func TestComposeOptions(t *testing.T) {
	t.Parallel()

	full := ComposeOptions{Files: []string{"a.yml", "-"}, ProjectName: "my-app", ProjectDirectory: "/work/app"}

	want := []string{"compose", "--file=a.yml", "--file=-", "--project-name=my-app", "--project-directory=/work/app", "ps", "--format", "json"}
	if got := full.psArgs(); !reflect.DeepEqual(got, want) {
		t.Errorf("psArgs = %q, want %q", got, want)
	}

	if got := (ComposeOptions{}).psArgs(); !reflect.DeepEqual(got, []string{"compose", "ps", "--format", "json"}) {
		t.Errorf("zero options: psArgs = %q", got)
	}

	bad := []struct {
		name string
		o    ComposeOptions
	}{
		{"empty file", ComposeOptions{Files: []string{""}}},
		{"newline in file", ComposeOptions{Files: []string{"a\nb"}}},
		{"NUL in file", ComposeOptions{Files: []string{"a\x00b"}}},
		{"invalid utf-8", ComposeOptions{Files: []string{"a\xffb"}}},
		{"long file", ComposeOptions{Files: []string{strings.Repeat("a", maxComposePath+1)}}},
		{"many files", ComposeOptions{Files: make([]string, maxComposeFiles+1)}},
		{"tab in dir", ComposeOptions{ProjectDirectory: "a\tb"}},
		{"upper-case project", ComposeOptions{ProjectName: "MyApp"}},
		{"flag as project", ComposeOptions{ProjectName: "--privileged"}},
		{"space in project", ComposeOptions{ProjectName: "my app"}},
	}

	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if api.CodeOf(tt.o.Validate()) != api.CodeInvalidRequest {
				t.Errorf("Validate(%+v) = %v, want INVALID_REQUEST", tt.o, tt.o.Validate())
			}
		})
	}

	if err := full.Validate(); err != nil {
		t.Errorf("Validate(full) = %v", err)
	}
}
