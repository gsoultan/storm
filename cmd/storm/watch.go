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

	toolbootstrap "github.com/gsoultan/storm/tool/bootstrap"
	tooldiscover "github.com/gsoultan/storm/tool/discover"
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
	regenerate(dir, args)

	last, err := tooldiscover.Fingerprint(mod.Root, out)
	if err != nil {
		return err
	}

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
			cur, err := tooldiscover.Fingerprint(mod.Root, out)
			if err != nil || cur == last {
				continue
			}
			// Wait for the burst to end before reading anything.
			for {
				time.Sleep(settleFor)
				settled, err := tooldiscover.Fingerprint(mod.Root, out)
				if err != nil || settled == cur {
					cur = settled
					break
				}
				cur = settled
			}
			last = cur
			regenerate(dir, args)
			// The generator just wrote into the tree; re-read so its own
			// output is not the next "change".
			if after, err := tooldiscover.Fingerprint(mod.Root, out); err == nil {
				last = after
			}
		}
	}
}

// regenerate runs one generation and reports it in one line. It never returns
// an error: a watcher that exits on a compile error in a half-typed model is
// a watcher nobody leaves running.
func regenerate(dir string, args []string) {
	start := time.Now()
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "storm: %v\n", err)
		return
	}
	r, err := tooldiscover.Discover(wd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "storm: %s\n", err)
		return
	}
	for _, s := range r.Skipped {
		if s.Actionable {
			fmt.Fprintf(os.Stderr, "storm: skipped %s.%s — %s\n", s.ImportPath, s.Name, s.Why)
		}
	}
	if err := checkUndeclarable(r); err != nil {
		fmt.Fprintf(os.Stderr, "storm: %s\n", err)
		return
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
}

func took(start time.Time) string {
	return time.Since(start).Round(time.Millisecond).String()
}

// errWatchNeedsDir is returned when watch is given no output directory and
// none can be assumed.
var errWatchNeedsDir = errors.New(
	"watch needs the output directory, the same one you pass to generate:\n" +
		"       storm watch internal/store")
