// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// exeExt is the executable suffix on this platform.
var exeExt = map[bool]string{true: ".exe"}[runtime.GOOS == "windows"]

// eyedbgModule, eyedbgdModule are the packages binariesFor builds.
const (
	eyedbgModule  = "github.com/eyedebugger/eyedebugger/cmd/eyedbg"
	eyedbgdModule = "github.com/eyedebugger/eyedebugger/cmd/eyedbgd"
)

// binOnce guards the one build every test in this package shares.
var (
	binOnce sync.Once
	binDir  string
	errBin  error
)

// binariesFor skips t in -short mode (the build is the one thing it
// skips), else returns the eyedbg and eyedbgd paths, building them once
// for the whole package.
func binariesFor(t *testing.T) (eyedbg, eyedbgd string) {
	t.Helper()

	if testing.Short() {
		t.Skip("skips building eyedbg/eyedbgd in -short")
	}

	binOnce.Do(buildBinaries)

	if errBin != nil {
		t.Fatal(errBin)
	}

	return filepath.Join(binDir, "eyedbg"+exeExt), filepath.Join(binDir, "eyedbgd"+exeExt)
}

// buildBinaries builds eyedbg and eyedbgd into a fresh temporary
// directory, once.
func buildBinaries() {
	dir, err := os.MkdirTemp("", "edbe2e")
	if err != nil {
		errBin = fmt.Errorf("create the binaries' temp dir: %w", err)

		return
	}

	//nolint:gosec // fixed arguments (module paths, no user input); this builds the binaries under test.
	cmd := exec.CommandContext(context.Background(), "go", "build", "-trimpath", "-o", dir+string(filepath.Separator), eyedbgModule, eyedbgdModule)

	if out, err := cmd.CombinedOutput(); err != nil {
		errBin = fmt.Errorf("go build %s %s: %w\n%s", eyedbgModule, eyedbgdModule, err, out)

		return
	}

	binDir = dir
}

// removeBinaries deletes what buildBinaries built, once every test has
// run (TestMain).
func removeBinaries() {
	if binDir != "" {
		_ = os.RemoveAll(binDir)
	}
}
