// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// envFakeDotnet carries the scenario (JSON) to the fake dotnet process; set,
// the test binary runs as the fake (maybeRunFakeDotnet).
const envFakeDotnet = "EYEDBG_TEST_FAKE_DOTNET"

// fakeDotnetScenario is how the fake dotnet behaves.
type fakeDotnetScenario struct {
	// Record is a file the fake writes what it saw to.
	Record string `json:"record,omitempty"`
	// Write lists the files it creates under the -o directory.
	Write []string `json:"write,omitempty"`
	// Exit is its exit code; Stdout and Stderr what it prints.
	Exit   int    `json:"exit,omitempty"`
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
}

// fakeDotnetRecord is what the fake saw.
type fakeDotnetRecord struct {
	Argv      []string `json:"argv"`
	Cwd       string   `json:"cwd"`
	Telemetry string   `json:"telemetry"`
	NoLogo    string   `json:"noLogo"`
}

// maybeRunFakeDotnet runs the fake dotnet and exits when this process is one;
// otherwise it returns at once.
func maybeRunFakeDotnet() {
	raw := os.Getenv(envFakeDotnet)
	if raw == "" {
		return
	}

	os.Exit(runFakeDotnet(raw, os.Args[1:], os.Stdout, os.Stderr))
}

func runFakeDotnet(raw string, argv []string, stdout, stderr io.Writer) int {
	var sc fakeDotnetScenario
	if err := json.Unmarshal([]byte(raw), &sc); err != nil {
		_, _ = io.WriteString(stderr, "fake dotnet: bad scenario: "+err.Error()+"\n")

		return 125
	}

	if sc.Record != "" {
		cwd, _ := os.Getwd()

		b, err := json.Marshal(fakeDotnetRecord{
			Argv: argv, Cwd: cwd, Telemetry: os.Getenv("DOTNET_CLI_TELEMETRY_OPTOUT"), NoLogo: os.Getenv("DOTNET_NOLOGO"),
		})
		if err != nil {
			return 125
		}

		if err := os.WriteFile(sc.Record, b, 0o600); err != nil {
			_, _ = io.WriteString(stderr, "fake dotnet: "+err.Error()+"\n")

			return 125
		}
	}

	out := ""

	for i, a := range argv {
		if a == "-o" && i+1 < len(argv) {
			out = argv[i+1]
		}
	}

	for _, name := range sc.Write {
		p := filepath.Join(out, name)

		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil { //nolint:gosec // A test fake writing where the build was told to.
			_, _ = io.WriteString(stderr, "fake dotnet: "+err.Error()+"\n")

			return 125
		}

		if err := os.WriteFile(p, []byte(strings.TrimSuffix(name, filepath.Ext(name))), 0o600); err != nil { //nolint:gosec // A test fake writing where the build was told to.
			_, _ = io.WriteString(stderr, "fake dotnet: "+err.Error()+"\n")

			return 125
		}
	}

	_, _ = io.WriteString(stdout, sc.Stdout)
	_, _ = io.WriteString(stderr, sc.Stderr)

	return sc.Exit
}
