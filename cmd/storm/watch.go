package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gsoultan/storm/tool/bootstrap"
	"github.com/gsoultan/storm/tool/discover"
)

const (
	// pollEvery is how often the module is fingerprinted. Polling rather than
	// fsnotify: it is stdlib, it cannot miss an event, and stat-ing a few
	// hundred files costs less than the debounce that follows it.
	pollEvery = 400 * time.Millisecond
	// settleFor is how long the tree must stop changing before generating.
	// Editors write in bursts — a save can be a truncate then a write, and
	// "format on save" is a second write right after — so generating on the
	// first change would run against a half-written file.
	settleFor = 250 * time.Millisecond
)

// watch regenerates on save.
//
// The generate step cannot be removed — Go has no build hook — but REMEMBERING
// it can be. With this running, editing a model is the whole workflow: save,
// and the store is current before you have switched windows.
func watch(dir string, args []string) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	mod, err := tooldiscover.FindModule(wd)
	if err != nil {
		return err
	}
	out := dir
	if !filepath.IsAbs(out) {
		out = filepath.Join(mod.Root, dir)
	}

	fmt.Fprintf(os.Stderr, "storm: watching %s → %s\n", mod.Path, dir)
	fmt.Fprintln(os.Stderr, "storm: ctrl-c to stop")

	// Generate once up front, so a watcher started against a stale tree makes
	// it current rather than waiting for an edit that may not come.
	last, err := tooldiscover.TakeSnapshot(mod.Root, out)
	if err != nil {
		return err
	}
	ws := regenerate(dir, args)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()

	for {
		select {
		case <-stop:
			fmt.Fprintln(os.Stderr, "\nstorm: stopped")
			return nil
		case <-tick.C:
			cur, changed, ok := settle(mod.Root, out, last)
			if !ok {
				continue
			}
			// The baseline is the tree as it was BEFORE generating, never
			// after. Re-reading it afterwards absorbed any save made while
			// generate ran (seconds, for a go run), so that save was never
			// generated. The output directory is excluded from the snapshot,
			// so the generator's own writes cannot look like a change.
			last = cur
			if !ws.Relevant(changed) {
				continue
			}
			if next := regenerate(dir, args); next != nil {
				ws = next
			}
		}
	}
}

// settle polls until the tree stops changing and returns it with what changed
// since last; ok is false when nothing did. Editors write in bursts, so
// reading on the first change would see a half-written file.
func settle(root, out string, last tooldiscover.Snapshot) (tooldiscover.Snapshot, []string, bool) {
	cur, err := tooldiscover.TakeSnapshot(root, out)
	if err != nil || len(cur.Changed(last)) == 0 {
		return nil, nil, false
	}
	for {
		time.Sleep(settleFor)
		settled, err := tooldiscover.TakeSnapshot(root, out)
		if err != nil {
			continue
		}
		if len(settled.Changed(cur)) == 0 {
			break
		}
		cur = settled
	}
	return cur, cur.Changed(last), true
}

// regenerate runs one generation and reports it in one line. It never returns
// an error: a watcher that exits on a compile error in a half-typed model is
// a watcher nobody leaves running.
//
// It returns what the next change is judged against: the packages discovery
// found models in and everything they import. nil means discovery itself
// failed, and the caller keeps the set it had.
func regenerate(dir string, args []string) *tooldiscover.WatchSet {
	start := time.Now()
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "storm: %v\n", err)
		return nil
	}
	r, err := tooldiscover.Discover(wd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "storm: %s\n", err)
		return nil
	}
	ws := r.WatchSet()
	for _, s := range r.Skipped {
		if s.Actionable {
			fmt.Fprintf(os.Stderr, "storm: skipped %s.%s — %s\n", s.ImportPath, s.Name, s.Why)
		}
	}
	if err := checkUndeclarable(r); err != nil {
		fmt.Fprintf(os.Stderr, "storm: %s\n", err)
		return ws
	}

	code, err := toolbootstrap.RunWith(r, append([]string{"generate", dir}, args...), io.Discard)
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "storm: %s\n", err)
	case code != 0:
		fmt.Fprintf(os.Stderr, "storm: generate failed (%s)\n", took(start))
	default:
		fmt.Fprintf(os.Stderr, "storm: %d model(s) → %s (%s)\n", len(r.Models), dir, took(start))
	}
	return ws
}

func took(start time.Time) string {
	return time.Since(start).Round(time.Millisecond).String()
}

// errWatchNeedsDir is returned when watch is given no output directory and
// none can be assumed.
var errWatchNeedsDir = errors.New(
	"watch needs the output directory, the same one you pass to generate:\n" +
		"       storm watch internal/store")
