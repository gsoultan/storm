// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package pgdrv

// The paths a live test does not reach: every call classifies the driver's
// error, and an unclassified one is an error a handler cannot tell a 409 from
// a 500 by. Pool, Tx and Conn all run through the same four functions over a
// session, so a fake session reaches those directly and needs no server.

import (
	"context"
	"errors"
	"testing"

	"github.com/gsoultan/caduceus"
	"github.com/gsoultan/caduceus/wire"
	"github.com/gsoultan/storm/v2/runtime"
)

// failing is a session that answers everything with one error.
type failing struct {
	err      error
	tag      caduceus.CommandTag
	sendCall int
}

func (f *failing) Query(context.Context, string, ...any) (*caduceus.Rows, error) {
	return nil, f.err
}
func (f *failing) Exec(context.Context, string, ...any) (caduceus.CommandTag, error) {
	return f.tag, f.err
}
func (f *failing) CopyFrom(context.Context, []string, []string, caduceus.CopySource) (int64, error) {
	return 0, f.err
}
func (f *failing) SendBatch(context.Context, []caduceus.Query) (*caduceus.BatchResults, error) {
	f.sendCall++
	return nil, f.err
}

func unique() error {
	return &wire.Error{Code: "23505", ConstraintName: "users_email_key", TableName: "users"}
}

func TestEveryCallClassifiesTheDriversError(t *testing.T) {
	ctx := context.Background()
	s := &failing{err: unique()}

	if _, err := query(ctx, s, "SELECT 1", nil, nil); !errors.Is(err, runtime.ErrUniqueViolation) {
		t.Errorf("Query: err = %v, want a unique violation", err)
	}
	if _, err := exec(ctx, s, "INSERT", nil); !errors.Is(err, runtime.ErrUniqueViolation) {
		t.Errorf("Exec: err = %v, want a unique violation", err)
	}
	if _, err := copyFrom(ctx, s, "users", []string{"id"}, emptySource{}); !errors.Is(err, runtime.ErrUniqueViolation) {
		t.Errorf("CopyFrom: err = %v, want a unique violation", err)
	}
	if err := batch(ctx, s, []runtime.BatchOp{{SQL: "INSERT"}}, nil); !errors.Is(err, runtime.ErrUniqueViolation) {
		t.Errorf("Batch: err = %v, want a unique violation", err)
	}

	var ce *runtime.ConstraintError
	_, err := exec(ctx, s, "INSERT", nil)
	if !errors.As(err, &ce) {
		t.Fatalf("Exec: err = %v, want a *runtime.ConstraintError", err)
	}
	if ce.Constraint != "users_email_key" || ce.Table != "users" {
		t.Errorf("the classification lost its metadata: %+v", ce)
	}
}

// An error storm has no opinion about passes through UNCHANGED.
func TestAnUnrecognisedErrorPassesThrough(t *testing.T) {
	mine := errors.New("the network went away")
	if _, err := query(context.Background(), &failing{err: mine}, "SELECT 1", nil, nil); !errors.Is(err, mine) {
		t.Errorf("err = %v, want the driver's own error", err)
	}
}

// A failed Query still gives a pool's connection back, exactly once.
func TestAFailedQueryRunsDone(t *testing.T) {
	var released counter
	_, _ = query(context.Background(), &failing{err: unique()}, "SELECT 1", nil, &released)
	if released != 1 {
		t.Errorf("done ran %d times, want once", released)
	}
}

func TestExecReportsTheAffectedCount(t *testing.T) {
	n, err := exec(context.Background(), &failing{tag: "UPDATE 3"}, "UPDATE users SET x = 1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("affected = %d, want 3", n)
	}
}

// An empty batch sends NOTHING: queueing zero statements would be a round trip
// for no work, and caduceus refuses one with ErrEmptyBatch.
func TestBatchWithNoOpsSendsNothing(t *testing.T) {
	f := &failing{}
	called := false
	err := batch(context.Background(), f, nil, func(int, runtime.Rows, int64, error) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("an empty batch returned %v", err)
	}
	if f.sendCall != 0 {
		t.Error("an empty batch reached the connection")
	}
	if called {
		t.Error("the callback ran for a batch with no ops")
	}
}

// params copies the argument slice only when it has something to wrap, and
// never writes into the caller's.
func TestParamsLeavesPlainArgumentsAlone(t *testing.T) {
	plain := []any{int64(1), "x", [16]byte{1}}
	if got := params(plain); &got[0] != &plain[0] {
		t.Error("a slice with nothing to wrap was copied")
	}
	d := runtime.Decimal{}
	mixed := []any{int64(1), d}
	got := params(mixed)
	if _, ok := got[1].(decimalParam); !ok {
		t.Errorf("a Decimal was not wrapped: %T", got[1])
	}
	if _, ok := mixed[1].(runtime.Decimal); !ok {
		t.Error("the caller's slice was written to")
	}
	if got := params([]any{(*runtime.Decimal)(nil)}); got[0] != nil {
		t.Errorf("a nil *Decimal is %v, want nil (NULL)", got[0])
	}
}

// A value sent for a parameter of another type is refused here: binary read
// with the wrong receive function is refused by the server at best.
func TestABinaryParamRefusesTheWrongType(t *testing.T) {
	if _, _, err := (decimalParam{}).AppendBinaryParam(nil, 23); err == nil {
		t.Error("a Decimal was written for an int4")
	}
}

type emptySource struct{}

func (emptySource) Next() bool    { return false }
func (emptySource) Values() []any { return nil }
func (emptySource) Err() error    { return nil }

// counter counts the times it is ended.
type counter int

func (c *counter) end() { *c++ }
