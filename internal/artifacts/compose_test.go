// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package artifacts

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/adapters"
)

// mkdirs creates the directories (0750) and returns the first.
func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()

	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()

	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks: %v", err)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)

	return err == nil
}

func mustRead(t *testing.T, path string) string {
	t.Helper()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return string(b)
}

func realTemp(t *testing.T) string {
	t.Helper()

	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	return d
}

func TestComposeDir(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv(adapters.EnvHome, home)

	dir, err := Dir(Compose)
	if err != nil || dir != filepath.Join(home, "compose") {
		t.Fatalf("Dir(Compose) = %q, %v", dir, err)
	}

	if info, err := os.Lstat(dir); err != nil || !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o700) {
		t.Errorf("Lstat = %v, %v", info, err)
	}

	// Compose files are not dumps: Prune of the kind touches nothing.
	if removed := Prune(dir, Compose, time.Now().Add(100*24*time.Hour), time.Hour, 0, ""); len(removed) != 0 {
		t.Errorf("Prune removed %v", removed)
	}
}

func TestProjectDir(t *testing.T) {
	t.Parallel()

	root := realTemp(t)

	dir, err := ProjectDir(root, "my-app_1")
	if err != nil || dir != filepath.Join(root, "my-app_1") {
		t.Fatalf("ProjectDir = %q, %v", dir, err)
	}

	if info, err := os.Lstat(dir); err != nil || !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o700) {
		t.Errorf("Lstat = %v, %v", info, err)
	}

	// Again: the same directory.
	if again, err := ProjectDir(root, "my-app_1"); err != nil || again != dir {
		t.Errorf("second call = %q, %v", again, err)
	}

	for _, bad := range []string{"", "..", ".", "a/b", `a\b`, "My-App", "a b", "-x", "a\n", "x" + string(rune(0)), "../x", "a.b"} {
		if d, err := ProjectDir(root, bad); err == nil {
			t.Errorf("ProjectDir(%q) = %q, want an error", bad, d)
		}
	}

	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Errorf("a refused name left something in the root: %v", entries)
	}
}

func TestProjectDirRefusesASymlinkAndAFile(t *testing.T) {
	t.Parallel()

	root := realTemp(t)
	outside := realTemp(t)

	symlink(t, outside, filepath.Join(root, "linked"))
	writeFile(t, filepath.Join(root, "file"), "x")

	for _, name := range []string{"linked", "file"} {
		if d, err := ProjectDir(root, name); err == nil {
			t.Errorf("ProjectDir(%q) = %q, want an error", name, d)
		}
	}

	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("something was created through the symlink: %v", entries)
	}
}

func TestServiceDir(t *testing.T) {
	t.Parallel()

	root := realTemp(t)

	pd, err := ProjectDir(root, "p")
	if err != nil {
		t.Fatal(err)
	}

	dir, err := ServiceDir(pd, "Web-1_a.b")
	if err != nil || dir != filepath.Join(pd, "services", "Web-1_a.b") {
		t.Fatalf("ServiceDir = %q, %v", dir, err)
	}

	if runtime.GOOS != "windows" {
		// The service directory is mounted: a non-root user in the container
		// must traverse it, whatever the umask.
		if info, _ := os.Stat(dir); info.Mode().Perm() != 0o755 {
			t.Errorf("service dir mode = %v, want 0755", info.Mode().Perm())
		}

		if info, _ := os.Stat(filepath.Join(pd, "services")); info.Mode().Perm() != 0o700 {
			t.Errorf("services dir mode = %v, want 0700", info.Mode().Perm())
		}
	}

	// A narrower mode left by an earlier run is made right.
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // A narrow mode left by an earlier run.
			t.Fatal(err)
		}

		if _, err := ServiceDir(pd, "Web-1_a.b"); err != nil {
			t.Fatal(err)
		}

		if info, _ := os.Stat(dir); info.Mode().Perm() != 0o755 {
			t.Errorf("service dir mode = %v, want 0755 again", info.Mode().Perm())
		}
	}

	for _, bad := range []string{"", "..", ".", "a/b", `a\b`, "-x", "a b", "a\n", "../x", "_x", ".x", string(make([]byte, 64))} {
		if d, err := ServiceDir(pd, bad); err == nil {
			t.Errorf("ServiceDir(%q) = %q, want an error", bad, d)
		}
	}
}

func TestServiceDirRefusesSymlinks(t *testing.T) {
	t.Parallel()

	root := realTemp(t)
	outside := realTemp(t)

	pd, err := ProjectDir(root, "p")
	if err != nil {
		t.Fatal(err)
	}

	// services/ itself a symlink out.
	symlink(t, outside, filepath.Join(pd, "services"))

	if d, err := ServiceDir(pd, "web"); err == nil {
		t.Errorf("ServiceDir through a symlinked services directory = %q", d)
	}

	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("something was created through the symlink: %v", entries)
	}

	if err := os.Remove(filepath.Join(pd, "services")); err != nil {
		t.Fatal(err)
	}

	// A service directory that is a symlink, and one that is a file.
	mkdirs(t, filepath.Join(pd, "services"))
	symlink(t, outside, filepath.Join(pd, "services", "linked"))
	writeFile(t, filepath.Join(pd, "services", "file"), "x")

	for _, name := range []string{"linked", "file"} {
		if d, err := ServiceDir(pd, name); err == nil {
			t.Errorf("ServiceDir(%q) = %q, want an error", name, d)
		}
	}

	// The project directory itself a symlink.
	link := filepath.Join(root, "alias")
	symlink(t, pd, link)

	if d, err := ServiceDir(link, "web"); err == nil {
		t.Errorf("ServiceDir in a symlinked project directory = %q", d)
	}
}

func TestCaseAliases(t *testing.T) {
	t.Parallel()

	dir := realTemp(t)
	mkdirs(t, filepath.Join(dir, "Web"), filepath.Join(dir, "api"))

	tests := []struct {
		name    string
		service string
		wantErr bool
	}{
		{"the same name", "Web", false},
		{"a different name", "worker", false},
		{"differs only in case", "web", true},
		{"differs only in case, the other way", "API", true},
		{"a longer name", "web2", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := checkAlias(dir, tt.service); (err != nil) != tt.wantErr {
				t.Errorf("checkAlias(%q) = %v, wantErr %v", tt.service, err, tt.wantErr)
			}
		})
	}
}

func TestCheckServiceNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		names   []string
		fold    bool
		wantErr bool
	}{
		{"distinct", []string{"web", "api"}, true, false},
		{"case twins where case folds", []string{"Web", "web"}, true, true},
		{"case twins where case matters", []string{"Web", "web"}, false, false},
		{"the same name twice", []string{"web", "web"}, true, false},
		{"three, one pair", []string{"a", "B", "b"}, true, true},
		{"none", nil, true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := checkFold(tt.names, tt.fold); (err != nil) != tt.wantErr {
				t.Errorf("checkFold = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}

	// The exported check follows the host.
	err := CheckServiceNames([]string{"Web", "web"})
	if want := runtime.GOOS == "darwin" || runtime.GOOS == "windows"; (err != nil) != want {
		t.Errorf("CheckServiceNames on %s = %v", runtime.GOOS, err)
	}
}

func TestStages(t *testing.T) {
	t.Parallel()

	root := realTemp(t)
	pd, err := ProjectDir(root, "p")
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}

	for range 20 {
		st, err := NewStage(pd)
		if err != nil {
			t.Fatal(err)
		}

		if filepath.Dir(st) != pd || !isStageName(filepath.Base(st)) || seen[st] {
			t.Fatalf("stage = %q (seen %v)", st, seen[st])
		}

		seen[st] = true

		if info, err := os.Lstat(st); err != nil || !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o700) {
			t.Fatalf("Lstat(%s) = %v, %v", st, info, err)
		}
	}

	if _, err := NewStage(filepath.Join(pd, "nowhere")); err == nil {
		t.Error("NewStage in a directory that doesn't exist")
	}
}

func TestStageNames(t *testing.T) {
	t.Parallel()

	for name, want := range map[string]bool{
		"stage-0123abcd": true, "stage-00000000": true,
		"stage-0123abc": false, "stage-0123abcde": false, "stage-0123ABCD": false, "stage-0123abcg": false,
		"stage-": false, "stage": false, "services": false, "Stage-0123abcd": false, "stage-0123abcd/": false,
		"stage-../../x": false, "x-stage-0123abcd": false,
	} {
		if got := isStageName(name); got != want {
			t.Errorf("isStageName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestRemoveStage(t *testing.T) {
	t.Parallel()

	root := realTemp(t)
	outside := realTemp(t)
	writeFile(t, filepath.Join(outside, "keep"), "x")

	pd, err := ProjectDir(root, "p")
	if err != nil {
		t.Fatal(err)
	}

	st, err := NewStage(pd)
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(st, "svc", "a.dll"), "x")
	symlink(t, outside, filepath.Join(st, "svc", "out-link"))
	symlink(t, filepath.Join(outside, "keep"), filepath.Join(st, "file-link"))

	if err := RemoveStage(pd, st); err != nil {
		t.Fatal(err)
	}

	if exists(st) {
		t.Error("the stage is still there")
	}

	if mustRead(t, filepath.Join(outside, "keep")) != "x" {
		t.Error("a symlink in the stage led the removal out")
	}

	// Gone already: fine.
	if err := RemoveStage(pd, st); err != nil {
		t.Errorf("removing a stage twice: %v", err)
	}
}

func TestRemoveStageRefusals(t *testing.T) {
	t.Parallel()

	root := realTemp(t)
	outside := realTemp(t)
	writeFile(t, filepath.Join(outside, "keep"), "x")

	pd, err := ProjectDir(root, "p")
	if err != nil {
		t.Fatal(err)
	}

	mkdirs(t, filepath.Join(pd, "services"), filepath.Join(pd, "stage-0123abcd", "sub"))
	symlink(t, outside, filepath.Join(pd, "stage-aaaaaaaa"))
	writeFile(t, filepath.Join(pd, "stage-bbbbbbbb"), "a file")

	tests := []struct {
		name  string
		stage string
	}{
		{"not a stage name", filepath.Join(pd, "services")},
		{"elsewhere", filepath.Join(outside, "stage-0123abcd")},
		{"nested", filepath.Join(pd, "stage-0123abcd", "sub")},
		{"the project dir", pd},
		{"a symlink with a stage name", filepath.Join(pd, "stage-aaaaaaaa")},
		{"a file with a stage name", filepath.Join(pd, "stage-bbbbbbbb")},
		{"dotdot", pd + string(filepath.Separator) + ".." + string(filepath.Separator) + "p" + string(filepath.Separator) + "stage-0123abcd"},
		{"empty", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := RemoveStage(pd, tt.stage); err == nil {
				t.Errorf("RemoveStage(%q) = nil, want an error", tt.stage)
			}
		})
	}

	if !exists(filepath.Join(pd, "stage-0123abcd", "sub")) || !exists(filepath.Join(pd, "services")) || mustRead(t, filepath.Join(outside, "keep")) != "x" {
		t.Error("a refused removal removed something")
	}
}

func TestPruneStages(t *testing.T) {
	t.Parallel()

	root := realTemp(t)
	outside := realTemp(t)
	writeFile(t, filepath.Join(outside, "keep"), "x")

	pd, err := ProjectDir(root, "p")
	if err != nil {
		t.Fatal(err)
	}

	mine := filepath.Join(pd, "stage-11111111")

	mkdirs(t, mine, filepath.Join(pd, "stage-22222222", "svc"), filepath.Join(pd, "services", "web"), filepath.Join(pd, "stage-notahexid"), filepath.Join(pd, "artifacts"))
	writeFile(t, filepath.Join(pd, "stage-33333333"), "a file named like a stage")
	writeFile(t, filepath.Join(pd, "override.yml"), "x")
	symlink(t, outside, filepath.Join(pd, "stage-44444444"))

	removed := PruneStages(pd, mine)
	if !slices.Equal(removed, []string{"stage-22222222"}) {
		t.Errorf("removed %v, want only stage-22222222", removed)
	}

	for _, kept := range []string{
		mine, filepath.Join(pd, "services", "web"), filepath.Join(pd, "stage-notahexid"), filepath.Join(pd, "artifacts"),
		filepath.Join(pd, "stage-33333333"), filepath.Join(pd, "override.yml"), filepath.Join(pd, "stage-44444444"),
	} {
		if !exists(kept) {
			t.Errorf("%s was removed", kept)
		}
	}

	if mustRead(t, filepath.Join(outside, "keep")) != "x" {
		t.Error("the symlink named like a stage led the removal out")
	}

	// With nothing to keep, "mine" goes too.
	if removed := PruneStages(pd, ""); !slices.Equal(removed, []string{"stage-11111111"}) {
		t.Errorf("removed %v", removed)
	}

	// A project directory that isn't there: nothing, no panic.
	if removed := PruneStages(filepath.Join(root, "nope"), ""); removed != nil {
		t.Errorf("removed %v", removed)
	}
}

func TestPruneServices(t *testing.T) {
	t.Parallel()

	root := realTemp(t)
	outside := realTemp(t)
	writeFile(t, filepath.Join(outside, "keep"), "x")

	pd, err := ProjectDir(root, "p")
	if err != nil {
		t.Fatal(err)
	}

	sv := filepath.Join(pd, "services")

	for _, s := range []string{"web", "api", "gone", "gone2"} {
		writeFile(t, filepath.Join(sv, s, "app.dll"), s)
	}

	// Foreign: names a service can't have, files, symlinks.
	writeFile(t, filepath.Join(sv, "has space", "x"), "x")
	writeFile(t, filepath.Join(sv, ".hidden", "x"), "x")
	writeFile(t, filepath.Join(sv, "readme"), "a file with a service-like name")
	symlink(t, outside, filepath.Join(sv, "linked"))

	removed := PruneServices(pd, map[string]bool{"web": true, "api": true})
	slices.Sort(removed)

	if !slices.Equal(removed, []string{"gone", "gone2"}) {
		t.Errorf("removed %v, want gone and gone2", removed)
	}

	for _, kept := range []string{"web", "api", "has space", ".hidden", "readme", "linked"} {
		if !exists(filepath.Join(sv, kept)) {
			t.Errorf("services/%s was removed", kept)
		}
	}

	if mustRead(t, filepath.Join(outside, "keep")) != "x" {
		t.Error("a symlink under services/ led the removal out")
	}

	// A false entry is not a keep.
	if removed := PruneServices(pd, map[string]bool{"web": true, "api": false}); !slices.Equal(removed, []string{"api"}) {
		t.Errorf("removed %v, want api", removed)
	}

	// Nothing referenced: everything real and named like a service goes.
	if removed := PruneServices(pd, nil); !slices.Equal(removed, []string{"web"}) {
		t.Errorf("removed %v, want web", removed)
	}

	// No services directory: nothing.
	if removed := PruneServices(filepath.Join(root, "p2"), nil); removed != nil {
		t.Errorf("removed %v", removed)
	}
}

func TestPruneServicesThroughASymlinkedServicesDir(t *testing.T) {
	t.Parallel()

	root := realTemp(t)
	outside := realTemp(t)
	mkdirs(t, filepath.Join(outside, "web"))

	pd, err := ProjectDir(root, "p")
	if err != nil {
		t.Fatal(err)
	}

	symlink(t, outside, filepath.Join(pd, "services"))

	if removed := PruneServices(pd, nil); removed != nil || !exists(filepath.Join(outside, "web")) {
		t.Errorf("removed %v through a symlinked services directory", removed)
	}
}

func TestInKeep(t *testing.T) {
	t.Parallel()

	keep := map[string]bool{"Web": true, "off": false}

	tests := []struct {
		name string
		fold bool
		want bool
	}{
		{"Web", false, true},
		{"web", false, false},
		{"web", true, true},
		{"WEB", true, true},
		{"off", true, false},
		{"api", true, false},
		{"Web2", true, false},
	}

	for _, tt := range tests {
		if got := inKeep(keep, tt.name, tt.fold); got != tt.want {
			t.Errorf("inKeep(%q, fold=%v) = %v, want %v", tt.name, tt.fold, got, tt.want)
		}
	}
}

func TestRemoveProjectDir(t *testing.T) {
	t.Parallel()

	root := realTemp(t)
	outside := realTemp(t)
	writeFile(t, filepath.Join(outside, "keep"), "x")

	pd, err := ProjectDir(root, "p")
	if err != nil {
		t.Fatal(err)
	}

	other, err := ProjectDir(root, "other")
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(pd, "services", "web", "a.dll"), "x")
	writeFile(t, filepath.Join(pd, "override.yml"), "x")
	symlink(t, outside, filepath.Join(pd, "artifacts-link"))

	if err := RemoveProjectDir(root, "p"); err != nil {
		t.Fatal(err)
	}

	if exists(pd) {
		t.Error("the project directory is still there")
	}

	if !exists(other) || mustRead(t, filepath.Join(outside, "keep")) != "x" {
		t.Error("something else was removed")
	}

	if err := RemoveProjectDir(root, "p"); err != nil {
		t.Errorf("removing a project directory that is gone: %v", err)
	}
}

func TestRemoveProjectDirRefusals(t *testing.T) {
	t.Parallel()

	root := realTemp(t)
	outside := realTemp(t)
	writeFile(t, filepath.Join(outside, "keep"), "x")
	writeFile(t, filepath.Join(root, "afile"), "x")
	symlink(t, outside, filepath.Join(root, "linked"))
	mkdirs(t, filepath.Join(root, "p"))

	for _, name := range []string{"", "..", ".", "a/b", "../p", "P", "a b", "linked", "afile", filepath.Join("p", "..")} {
		if err := RemoveProjectDir(root, name); err == nil && name != "" {
			// "linked" and "afile" are valid names but not real directories.
			t.Errorf("RemoveProjectDir(%q) = nil, want an error", name)
		} else if name == "" && err == nil {
			t.Error("RemoveProjectDir of an empty name = nil")
		}
	}

	if mustRead(t, filepath.Join(outside, "keep")) != "x" || !exists(filepath.Join(root, "afile")) || !exists(filepath.Join(root, "p")) {
		t.Error("a refused removal removed something")
	}

	// A root that is a symlink.
	link := filepath.Join(t.TempDir(), "rootlink")
	symlink(t, root, link)

	if err := RemoveProjectDir(link, "p"); err == nil || !exists(filepath.Join(root, "p")) {
		t.Errorf("removal through a symlinked root: %v", err)
	}
}

func TestLock(t *testing.T) {
	t.Parallel()

	pd := realTemp(t)

	unlock, err := Lock(pd)
	if err != nil {
		t.Fatal(err)
	}

	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(filepath.Join(pd, lockName)); info.Mode().Perm() != 0o600 {
			t.Errorf("lock mode = %v", info.Mode().Perm())
		}
	}

	if _, err := Lock(pd); !errors.Is(err, ErrLocked) {
		t.Fatalf("a second Lock = %v, want ErrLocked", err)
	}

	unlock()

	if exists(filepath.Join(pd, lockName)) {
		t.Fatal("unlock left the lock")
	}

	unlock() // twice is fine

	again, err := Lock(pd)
	if err != nil {
		t.Fatalf("Lock after unlock: %v", err)
	}

	again()
}

func TestLockIsExclusive(t *testing.T) {
	t.Parallel()

	pd := realTemp(t)

	const racers = 16

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []func()
		losers  int
	)

	for range racers {
		wg.Go(func() {
			unlock, err := Lock(pd)

			mu.Lock()
			defer mu.Unlock()

			switch {
			case err == nil:
				winners = append(winners, unlock)
			case errors.Is(err, ErrLocked):
				losers++
			default:
				t.Errorf("Lock: %v", err)
			}
		})
	}

	wg.Wait()

	if len(winners) != 1 || losers != racers-1 {
		t.Errorf("%d winners and %d losers, want 1 and %d", len(winners), losers, racers-1)
	}

	for _, u := range winners {
		u()
	}
}

func TestLockStaleness(t *testing.T) {
	t.Parallel()

	pd := realTemp(t)
	path := filepath.Join(pd, lockName)
	now := time.Now()

	first, err := lock(pd, now)
	if err != nil {
		t.Fatal(err)
	}

	// Fresh (just under an hour): busy.
	if err := os.Chtimes(path, now, now.Add(-59*time.Minute)); err != nil {
		t.Fatal(err)
	}

	if _, err := lock(pd, now); !errors.Is(err, ErrLocked) {
		t.Fatalf("a 59 minute old lock was taken: %v", err)
	}

	// Stale: taken over once.
	if err := os.Chtimes(path, now, now.Add(-61*time.Minute)); err != nil {
		t.Fatal(err)
	}

	second, err := lock(pd, now)
	if err != nil {
		t.Fatalf("a 61 minute old lock was not taken over: %v", err)
	}

	// The new holder is as exclusive as any.
	if _, err := lock(pd, now); !errors.Is(err, ErrLocked) {
		t.Fatalf("a lock just taken over was taken again: %v", err)
	}

	// The old holder's unlock must not remove the new holder's lock.
	first()

	if !exists(path) {
		t.Fatal("a stale holder's unlock removed the new holder's lock")
	}

	second()

	if exists(path) {
		t.Error("the new holder's unlock left the lock")
	}

	// No stale-lock leftovers in the directory.
	entries, _ := os.ReadDir(pd)
	if len(entries) != 0 {
		t.Errorf("leftovers: %v", entries)
	}
}

// TestLockIdentityIsContent pins what tells one lock from another: what it
// holds, not its file identity. A lock rewritten in place keeps its inode,
// as a lock taken over may get its predecessor's (ext4 reuses a freed inode
// at once), and must not be removed by the first holder's unlock.
func TestLockIdentityIsContent(t *testing.T) {
	t.Parallel()

	pd := realTemp(t)
	path := filepath.Join(pd, lockName)

	unlock, err := Lock(pd)
	if err != nil {
		t.Fatal(err)
	}

	line := mustRead(t, path)
	if !regexp.MustCompile(`^\d+ [0-9a-f]{32}\n$`).MatchString(line) {
		t.Fatalf("lock content = %q, want \"<pid> <token>\\n\"", line)
	}

	// Another run's lock in the same file (the inode stays).
	other := "1 " + strings.Repeat("ab", 16) + "\n"
	if err := os.WriteFile(path, []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}

	unlock()

	if !exists(path) || mustRead(t, path) != other {
		t.Fatal("unlock removed a lock that isn't its own")
	}

	if entries, _ := os.ReadDir(pd); len(entries) != 1 {
		t.Errorf("leftovers: %v", entries)
	}

	// Two runs' tokens differ.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	again, err := Lock(pd)
	if err != nil {
		t.Fatal(err)
	}
	defer again()

	if mustRead(t, path) == line {
		t.Error("two locks hold the same line")
	}
}

// TestLockTakeover covers which stale locks are taken over: only a
// complete lock line, which is what identifies the lock being removed.
func TestLockTakeover(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		link    bool // a symlink instead of a file
		taken   bool
	}{
		{name: "this version's", content: "42 " + strings.Repeat("0f", 16) + "\n", taken: true},
		{name: "an older version's pid only", content: "42\n", taken: true},
		{name: "empty (a run died writing it)", content: ""},
		{name: "unfinished line", content: "42 0f0f"},
		{name: "two lines", content: "42\n43\n"},
		{name: "too long", content: strings.Repeat("x", lockReadMax) + "\n"},
		{name: "a symlink", link: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pd := realTemp(t)
			path := filepath.Join(pd, lockName)

			if tt.link {
				target := filepath.Join(realTemp(t), "target")
				writeFile(t, target, "42\n")
				symlink(t, target, path)
			} else {
				writeFile(t, path, tt.content)
			}

			checkTakeover(t, pd, tt.content, tt.taken)
		})
	}
}

// checkTakeover takes pd's lock when every lock is stale, and checks it was
// taken over or not, as taken says.
func checkTakeover(t *testing.T, pd, content string, taken bool) {
	t.Helper()

	path := filepath.Join(pd, lockName)

	// Every lock in pd is stale by then (a symlink's own times can't be set
	// portably).
	unlock, err := lock(pd, time.Now().Add(2*LockMaxAge))
	if !taken {
		if err == nil {
			unlock()
			t.Fatal("taken over")
		}

		if !errors.Is(err, ErrLocked) {
			t.Fatalf("lock = %v, want ErrLocked", err)
		}

		if !exists(path) {
			t.Error("the lock was removed")
		}

		return
	}

	if err != nil {
		t.Fatalf("not taken over: %v", err)
	}

	if mustRead(t, path) == content {
		t.Error("the stale lock is still there")
	}

	unlock()

	if entries, _ := os.ReadDir(pd); len(entries) != 0 {
		t.Errorf("leftovers: %v", entries)
	}
}

// TestRemoveLockChecksWhatItMoves covers removeLock's checks: a lock that
// doesn't hold what is wanted is left in place, untouched.
func TestRemoveLockChecksWhatItMoves(t *testing.T) {
	t.Parallel()

	pd := realTemp(t)
	path := filepath.Join(pd, lockName)
	writeFile(t, path, "1 aa\n")

	root, err := os.OpenRoot(pd)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if removeLock(root, []byte("1 bb\n")) {
		t.Fatal("removed a lock holding something else")
	}

	if mustRead(t, path) != "1 aa\n" {
		t.Fatal("the lock changed")
	}

	if entries, _ := os.ReadDir(pd); len(entries) != 1 {
		t.Errorf("leftovers: %v", entries)
	}

	if !removeLock(root, []byte("1 aa\n")) || exists(path) {
		t.Fatal("the matching lock was not removed")
	}

	if removeLock(root, []byte("1 aa\n")) {
		t.Error("removed a lock that isn't there")
	}
}

func TestLockRefusals(t *testing.T) {
	t.Parallel()

	root := realTemp(t)
	outside := realTemp(t)
	link := filepath.Join(root, "linked")

	symlink(t, outside, link)

	if _, err := Lock(link); err == nil {
		t.Error("Lock through a symlinked project directory")
	}

	if exists(filepath.Join(outside, lockName)) {
		t.Error("a lock was created through the symlink")
	}

	if _, err := Lock(filepath.Join(root, "nowhere")); err == nil {
		t.Error("Lock in a directory that doesn't exist")
	}

	// A symlink where the lock goes is not followed: it counts as a lock.
	pd := realTemp(t)
	writeFile(t, filepath.Join(outside, "target"), "x")
	symlink(t, filepath.Join(outside, "target"), filepath.Join(pd, lockName))

	if _, err := Lock(pd); !errors.Is(err, ErrLocked) {
		t.Errorf("Lock with a symlink in its place = %v", err)
	}

	if mustRead(t, filepath.Join(outside, "target")) != "x" {
		t.Error("the symlink was written through")
	}
}
