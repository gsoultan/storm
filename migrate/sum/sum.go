// Package migratesum keeps a migrations directory honest: a file, storm.sum,
// that records a hash of every migration in it and one hash over all of them.
//
// A migration that has been applied somewhere is history. Editing it changes
// nothing on the databases that already ran it and something on the ones that
// have not, which is how two environments end up with different schemas from
// the same directory, and no runner notices: they record which versions ran,
// not what the files said. storm.sum makes the edit a failed check instead.
//
// The first line is the hash of the whole directory, and it is what makes two
// branches that each add a migration collide in git. File names alone do not:
// 0005_add_tags.up.sql and 0005_add_status.up.sql merge cleanly and leave two
// version 5s. Both branches rewrite the first line of storm.sum, so the merge
// conflicts, and the fix — regenerate one of them on top of the other — is
// made before the deploy rather than discovered during it. Atlas's atlas.sum
// works the same way, for the same reason.
//
// Stdlib only: the check needs no database and no driver, so it can run first
// in CI and in any tool that applies the directory.
package migratesum

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// File is the name of the integrity file inside a migrations directory.
const File = "storm.sum"

// ErrNoSum means the directory has no storm.sum yet. Nothing is wrong with
// it; it has simply never been summed.
var ErrNoSum = errors.New("no " + File + " in the migrations directory")

// Sum is a directory's integrity record: one entry per migration file, in
// name order, and the hash over all of them.
type Sum struct {
	Total   string
	Entries []Entry
}

// Compute hashes every .sql file in dir, in name order.
func Compute(dir string) (Sum, error) {
	names, err := migrations(dir)
	if err != nil {
		return Sum{}, err
	}
	s := Sum{Entries: make([]Entry, 0, len(names))}
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return Sum{}, err
		}
		s.Entries = append(s.Entries, Entry{Name: n, Hash: hash(b)})
	}
	s.Total = hash([]byte(s.body()))
	return s, nil
}

// Write computes dir's sum and writes it to dir/storm.sum.
func Write(dir string) (Sum, error) {
	s, err := Compute(dir)
	if err != nil {
		return Sum{}, err
	}
	return s, os.WriteFile(filepath.Join(dir, File), []byte(s.String()), 0o644)
}

// Read parses dir/storm.sum. It returns ErrNoSum when there is none.
func Read(dir string) (Sum, error) {
	b, err := os.ReadFile(filepath.Join(dir, File))
	if errors.Is(err, os.ErrNotExist) {
		return Sum{}, ErrNoSum
	}
	if err != nil {
		return Sum{}, err
	}
	return Parse(b)
}

// Parse reads the text String writes.
func Parse(b []byte) (Sum, error) {
	var s Sum
	sc := bufio.NewScanner(bytes.NewReader(b))
	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		if line == 1 {
			// A merge that went wrong leaves its markers here, and the first
			// line is where they land.
			if !strings.HasPrefix(text, "h1:") {
				return Sum{}, fmt.Errorf("%s: line 1 is not a directory hash (%q); "+
					"if this is a merge conflict, resolve it by regenerating the newer "+
					"migration on top of the other branch's, then run `storm sum`", File, text)
			}
			s.Total = text
			continue
		}
		name, h, ok := strings.Cut(text, " ")
		if !ok || !strings.HasPrefix(h, "h1:") {
			return Sum{}, fmt.Errorf("%s: line %d is not \"<file> h1:<hash>\": %q", File, line, text)
		}
		s.Entries = append(s.Entries, Entry{Name: name, Hash: h})
	}
	if err := sc.Err(); err != nil {
		return Sum{}, err
	}
	if line == 0 {
		return Sum{}, fmt.Errorf("%s is empty", File)
	}
	return s, nil
}

// String is the file's text.
func (s Sum) String() string { return s.Total + "\n" + s.body() }

func (s Sum) body() string {
	var b strings.Builder
	for _, e := range s.Entries {
		b.WriteString(e.Name + " " + e.Hash + "\n")
	}
	return b.String()
}

// Check compares dir against its storm.sum, and checks that no two up
// migrations share a version — which needs no sum, so it is reported even
// when there is none. It returns ErrNoSum when dir has no storm.sum and is
// otherwise consistent, and an *Error when anything disagrees.
func Check(dir string) error {
	names, err := migrations(dir)
	if err != nil {
		return err
	}
	e := &Error{Duplicates: duplicates(names)}

	recorded, err := Read(dir)
	if errors.Is(err, ErrNoSum) {
		if len(e.Duplicates) > 0 {
			return e
		}
		return ErrNoSum
	}
	if err != nil {
		return err
	}
	if hash([]byte(recorded.body())) != recorded.Total {
		e.Corrupt = true
	}
	now, err := Compute(dir)
	if err != nil {
		return err
	}
	was := map[string]string{}
	for _, x := range recorded.Entries {
		was[x.Name] = x.Hash
	}
	is := map[string]bool{}
	for _, x := range now.Entries {
		is[x.Name] = true
		h, ok := was[x.Name]
		switch {
		case !ok:
			e.Added = append(e.Added, x.Name)
		case h != x.Hash:
			e.Modified = append(e.Modified, x.Name)
		}
	}
	for _, x := range recorded.Entries {
		if !is[x.Name] {
			e.Removed = append(e.Removed, x.Name)
		}
	}
	if len(e.Modified)+len(e.Added)+len(e.Removed)+len(e.Duplicates) > 0 || e.Corrupt {
		return e
	}
	return nil
}

// migrations lists the .sql files in dir, by name.
func migrations(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// duplicates reports every up migration whose numeric version another up
// migration also has.
func duplicates(names []string) []string {
	byVersion := map[int][]string{}
	for _, n := range names {
		if !strings.HasSuffix(n, ".up.sql") {
			continue
		}
		v, ok := version(n)
		if ok {
			byVersion[v] = append(byVersion[v], n)
		}
	}
	var out []string
	for _, ns := range byVersion {
		if len(ns) > 1 {
			out = append(out, ns...)
		}
	}
	sort.Strings(out)
	return out
}

// version is the leading number of a migration's name: 12 for
// 0012_add_tags.up.sql.
func version(name string) (int, bool) {
	i := 0
	for i < len(name) && name[i] >= '0' && name[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(name) || name[i] != '_' {
		return 0, false
	}
	v, err := strconv.Atoi(name[:i])
	return v, err == nil
}

// hash is over the content with CRLF line endings read as LF, so a checkout
// that converts line endings — git's autocrlf on Windows — is not an edit.
func hash(b []byte) string {
	h := sha256.Sum256(bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")))
	return "h1:" + base64.StdEncoding.EncodeToString(h[:])
}
