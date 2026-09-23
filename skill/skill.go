// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package skill embeds the agent-facing usage guide, SKILL.md
// (docs/DESIGN.md §10), and installs it into an agent's skills directory.
package skill

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Name is the skill's name: the frontmatter's `name` and its directory,
// both required by the Agent Skills spec to match.
const Name = "eyedbg"

//go:embed eyedbg/SKILL.md
var markdown string

// Markdown returns the embedded SKILL.md, byte for byte.
func Markdown() string { return markdown }

// DefaultRoot returns the default skills root: Claude Code's personal
// skills directory, "<home>/.claude/skills".
func DefaultRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate the skills directory: %w", err)
	}

	return filepath.Join(home, ".claude", "skills"), nil
}

// Status is the outcome of [Install].
type Status string

const (
	// StatusInstalled means the file did not exist and was written.
	StatusInstalled Status = "installed"
	// StatusUnchanged means an identical file already existed.
	StatusUnchanged Status = "unchanged"
	// StatusReplaced means a different file existed and force replaced it.
	StatusReplaced Status = "replaced"
)

// Result is what [Install] did.
type Result struct {
	// Path is root/Name/SKILL.md.
	Path   string
	Status Status
}

// DiffersError means root/Name/SKILL.md exists and differs from the
// embedded copy, and force was not given.
type DiffersError struct {
	// Path is the file that differs.
	Path string
}

func (e *DiffersError) Error() string {
	return e.Path + " already exists and differs from this eyedbg's copy"
}

// Install writes the embedded SKILL.md to root/Name/SKILL.md, creating
// directories as needed (0o750) and writing the file 0o600. An identical
// existing file is left alone (StatusUnchanged). A different one is left
// alone and reported as [DiffersError] unless force is set, in which case
// it is replaced (StatusReplaced). The write is atomic: a temp file in the
// same directory, then a rename, which replaces a symlinked target instead
// of writing through it.
func Install(root string, force bool) (Result, error) {
	dir := filepath.Join(root, Name)
	path := filepath.Join(dir, "SKILL.md")

	result := Result{Path: path}

	existing, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		result.Status = StatusInstalled
	case err != nil:
		return Result{}, fmt.Errorf("read %s: %w", path, err)
	case bytes.Equal(existing, []byte(markdown)):
		result.Status = StatusUnchanged

		return result, nil
	case !force:
		return Result{}, &DiffersError{Path: path}
	default:
		result.Status = StatusReplaced
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return Result{}, fmt.Errorf("create %s: %w", dir, err)
	}

	if err := writeAtomic(dir, path, markdown); err != nil {
		return Result{}, err
	}

	return result, nil
}

// writeAtomic writes content to path via a temp file in dir, then renames
// it into place.
func writeAtomic(dir, path, content string) error {
	tmp, err := os.CreateTemp(dir, ".skill-*.tmp")
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("write %s: %w", path, err)
	}

	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("write %s: %w", path, err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}
