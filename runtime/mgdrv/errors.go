// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package mgdrv

import (
	"strconv"
	"strings"

	"github.com/gsoultan/storm/runtime"
)

// Error is a command the server refused, with its numeric code.
//
// The code is what anything that decides on an error reads; the message is
// for a person and changes between versions.
type Error struct {
	Code    int
	Name    string
	Message string
	// Labels are the server's error labels. TransientTransactionError means
	// the whole transaction may be run again.
	Labels []string
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("mongodb ")
	b.WriteString(strconv.Itoa(e.Code))
	if e.Name != "" {
		b.WriteString(" (")
		b.WriteString(e.Name)
		b.WriteByte(')')
	}
	b.WriteString(": ")
	b.WriteString(e.Message)
	return b.String()
}

// Server codes this driver acts on.
const (
	codeDuplicateKey      = 11000
	codeDuplicateKeyOld   = 11001
	codeValidationFailure = 121
	codeWriteConflict     = 112
	codeNoSuchTransaction = 251
)

// classify turns a server error into the sentinel storm's other targets
// produce, so a handler written against runtime.ErrUniqueViolation is written
// once for every back end.
func classify(e *Error) error {
	switch e.Code {
	case codeDuplicateKey, codeDuplicateKeyOld:
		return &runtime.ConstraintError{Kind: runtime.ErrUniqueViolation,
			Constraint: indexName(e.Message), Err: e}
	case codeValidationFailure:
		// The collection's validator: a NOT NULL or a type, which the server
		// reports as one failure without saying which field.
		return &runtime.ConstraintError{Kind: runtime.ErrCheckViolation, Err: e}
	case codeWriteConflict:
		return &runtime.ConstraintError{Kind: runtime.ErrSerializationFailure, Err: e}
	}
	for _, l := range e.Labels {
		if l == "TransientTransactionError" {
			return &runtime.ConstraintError{Kind: runtime.ErrSerializationFailure, Err: e}
		}
	}
	return e
}

// indexName reads the index out of "E11000 duplicate key error collection:
// db.c index: uq_users_email dup key: { … }".
func indexName(msg string) string {
	_, rest, ok := strings.Cut(msg, " index: ")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(rest, " ")
	return name
}
