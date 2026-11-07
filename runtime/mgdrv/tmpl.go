// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package mgdrv

import (
	"errors"
	"strconv"
	"sync"
)

// template is a command compiled from its JSON text: the fixed parts already
// BSON, with the arguments' places marked.
//
// The text is what a generated package hands the Executor port, so it is the
// statement — human-readable, comparable, and the cache key here as it is in
// the generated package. Compiling it costs a parse; that is paid once per
// distinct text for the life of the pool, the way a prepared statement is.
type template struct {
	ops []op
	// nargs is one past the highest hole.
	nargs int
	// cmd is the command name: the first key.
	cmd string
	// cols are the fields a row of this command's result is read as, in
	// order: a find's projection, a findAndModify's fields, or an
	// aggregation's final $project or $count.
	cols []string
}

var errTemplate = errors.New("mgdrv: the command text is not a JSON object storm can compile")

// The compiled commands, by text. Bounded the way runtime's shape caches are:
// past the cap the map is dropped and refills from what is still in use.
var (
	tmplMu    sync.RWMutex
	tmplCache = map[string]*template{}
)

const tmplCap = 4096

func compiled(text string) (*template, error) {
	tmplMu.RLock()
	t := tmplCache[text]
	tmplMu.RUnlock()
	if t != nil {
		return t, nil
	}
	t, err := compile(text)
	if err != nil {
		return nil, err
	}
	tmplMu.Lock()
	if len(tmplCache) >= tmplCap {
		tmplCache = map[string]*template{}
	}
	tmplCache[text] = t
	tmplMu.Unlock()
	return t, nil
}

// encode writes the command's elements into an open document.
func (t *template) encode(b []byte, args []any) ([]byte, error) {
	if len(args) < t.nargs {
		return b, errors.New("mgdrv: " + t.cmd + " needs " + strconv.Itoa(t.nargs) +
			" arguments, got " + strconv.Itoa(len(args)))
	}
	// The open documents' starts. Sixteen levels cover every command a
	// generated package writes; a translated CHECK nests deeper, and grows
	// the stack rather than indexing past it.
	var buf [16]int
	stack := buf[:0]
	var err error
	for i := range t.ops {
		o := &t.ops[i]
		switch o.kind {
		case opRaw:
			b = append(b, o.raw...)
		case opOpen:
			b = append(b, o.raw...)
			var at int
			b, at = startDoc(b)
			stack = append(stack, at)
		case opClose:
			at := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			b = endDoc(b, at)
		case opArg:
			if b, err = appendValue(b, o.name, args[o.arg]); err != nil {
				return b, errors.New("mgdrv: argument " + strconv.Itoa(o.arg+1) + " of " + t.cmd +
					": unsupported Go type")
			}
		}
	}
	return b, nil
}
