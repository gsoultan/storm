// Package toolbootstrap writes and runs the bootstrap main that adopters used
// to keep in their own repositories.
//
// The bootstrap has to exist — storm resolves field pointers at runtime, so
// the tool must link against the models — but there was never a reason for it
// to be the developer's file. It is derived entirely from what tooldiscover
// found, so storm can write it, run it, and take it away again.
package toolbootstrap

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	tooldiscover "github.com/gsoultan/storm/tool/discover"
)

const stormPath = "github.com/gsoultan/storm"

// Run synthesizes the bootstrap, executes it with args, and removes it.
//
// It returns the child's exit code. The caller exits with it rather than
// interpreting it: every command in the tool already prints its own errors in
// one format, and re-wrapping them here would give the same failure two
// voices. Returning the code instead of calling os.Exit is what lets the
// cleanup below actually run.
func Run(r *tooldiscover.Result, args []string) (code int, err error) {
	return RunWith(r, args, os.Stdout)
}

// RunWith is Run with the child's stdout redirected.
//
// `storm watch` uses it to swallow the per-file listing: printed once it is
// informative, printed on every save it is the noise a watcher exists to
// remove. Errors still go to stderr, which is never redirected.
func RunWith(r *tooldiscover.Result, args []string, stdout io.Writer) (code int, err error) {
	src, err := Source(r)
	if err != nil {
		return 1, err
	}
	parent, err := r.ShimDir()
	if err != nil {
		return 1, err
	}

	// A dot-prefixed directory: the go tool ignores it for ./... patterns but
	// accepts it as an explicit path, so a crash that skips the cleanup below
	// leaves something inert rather than a package main that breaks the
	// adopter's build. Verified both ways, not assumed.
	dir := filepath.Join(parent, ".storm-bootstrap-"+strconv.Itoa(os.Getpid()))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 1, err
	}
	defer os.RemoveAll(dir)

	if err := os.WriteFile(filepath.Join(dir, "main.go"), src, 0o644); err != nil {
		return 1, err
	}

	rel, err := filepath.Rel(r.Module.Root, dir)
	if err != nil {
		return 1, err
	}
	cmd := exec.Command("go", append([]string{"run", "./" + filepath.ToSlash(rel)}, args...)...)
	// Always the module root, wherever the developer was standing. Output
	// directories and -out are module-relative, so letting cwd vary would make
	// the same command write to different places.
	cmd.Dir = r.Module.Root
	cmd.Stdin, cmd.Stdout = os.Stdin, stdout
	// Stderr is streamed AND kept: the child's own errors must appear as it
	// produces them, but one class of failure needs translating afterwards.
	var errBuf capped
	cmd.Stderr = io.MultiWriter(os.Stderr, &errBuf)

	switch err := cmd.Run(); {
	case err == nil:
		return 0, nil
	default:
		if hint := missingToolDep(errBuf.String()); hint != "" {
			return 1, errors.New(hint)
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), nil
		}
		return 1, fmt.Errorf("running the generated bootstrap: %w", err)
	}
}

// missingToolDep translates the one failure that discovery CAUSES.
//
// Without a bootstrap in the repository, nothing in the adopter's own source
// imports storm/tool, so `go mod tidy` never records its dependencies — and
// the first run fails with go's generic "updates to go.mod needed", pointing
// at a file the developer did not write. The fix is one command, once, and
// saying which one is the whole difference.
func missingToolDep(stderr string) string {
	switch {
	case strings.Contains(stderr, "updates to go.mod needed"),
		strings.Contains(stderr, "no required module provides package"),
		strings.Contains(stderr, "missing go.sum entry"):
		return "the bootstrap needs storm's tool package recorded in your go.mod, and nothing in\n" +
			"       your source imports it. Run this once:\n\n" +
			"           go get " + stormPath + "/tool\n"
	}
	return ""
}

// capped is an io.Writer that keeps the first 64KB. The child's stderr is
// already on the terminal; this copy exists only to be pattern-matched, and an
// unbounded buffer of someone else's output is a leak waiting for a loud
// failure.
type capped struct{ b []byte }

func (c *capped) Write(p []byte) (int, error) {
	if n := 64 << 10; len(c.b) < n {
		c.b = append(c.b, p[:min(len(p), n-len(c.b))]...)
	}
	return len(p), nil
}

func (c *capped) String() string { return string(c.b) }
