package planspike_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gsoultan/storm/internal/planspike/store/user"
	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/hooks"
	"github.com/gsoultan/storm/runtime/pgxdrv"
)

// Lifecycle hooks, end to end on a real database: each terminal runs its
// hooks in order, a Before hook can change what is written, and an error from
// one stops the write before anything is sent.
func TestLifecycleHooks(t *testing.T) {
	ctx := context.Background()
	ex, count := db(t)
	t.Cleanup(func() { user.Hooks = hooks.Lifecycle[user.Ins, user.Mut, user.Row]{} })

	var calls []string
	user.Hooks = hooks.Lifecycle[user.Ins, user.Mut, user.Row]{
		BeforeCreate: func(_ context.Context, hex runtime.Executor, n *user.Ins) error {
			if hex != ex {
				t.Error("BeforeCreate did not get the caller's executor")
			}
			calls = append(calls, "before-create")
			n.SetStatus("active")
			return nil
		},
		AfterCreate: func(_ context.Context, _ runtime.Executor, r *user.Row) error {
			calls = append(calls, "after-create")
			if r.ID == ([16]byte{}) {
				t.Error("AfterCreate ran before the database assigned the id")
			}
			return nil
		},
		BeforeUpdate: func(_ context.Context, _ runtime.Executor, m *user.Mut) error {
			calls = append(calls, "before-update")
			m.SetName("renamed-by-hook")
			return nil
		},
		AfterUpdate: func(context.Context, runtime.Executor, *user.Mut) error {
			calls = append(calls, "after-update")
			return nil
		},
		AfterFind: func(_ context.Context, _ runtime.Executor, r *user.Row) error {
			calls = append(calls, "after-find")
			r.Name = "decorated:" + r.Name
			return nil
		},
		BeforeDelete: func(_ context.Context, _ runtime.Executor, key *user.Row) error {
			calls = append(calls, "before-delete")
			return nil
		},
		AfterDelete: func(context.Context, runtime.Executor, *user.Row) error {
			calls = append(calls, "after-delete")
			return nil
		},
	}

	r := mustCreate(t, ctx, ex, "hooks@example.com", "Hooked")
	if r.Status != "active" {
		t.Errorf("Status = %q; BeforeCreate's assignment was not written", r.Status)
	}

	m := user.Mutate(r)
	if err := m.Update(ctx, ex); err != nil {
		t.Fatal(err)
	}

	got, err := user.New().IDEq(r.ID).List(ctx, ex)
	if err != nil || len(got) != 1 {
		t.Fatalf("List = %v, %v", got, err)
	}
	if got[0].Name != "decorated:renamed-by-hook" {
		t.Errorf("Name = %q; want BeforeUpdate's value, decorated by AfterFind", got[0].Name)
	}
	if err := user.New().IDEq(r.ID).Each(ctx, ex, func(r user.Row) error {
		if r.Name != "decorated:renamed-by-hook" {
			t.Errorf("Each: Name = %q, AfterFind did not run first", r.Name)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := user.Delete(ctx, ex, r.ID); err != nil {
		t.Fatal(err)
	}

	want := []string{"before-create", "after-create", "before-update", "after-update",
		"after-find", "after-find", "before-delete", "after-delete"}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("calls = %v, want %v", calls, want)
		}
	}

	// A Before hook that refuses sends nothing.
	refuse := errors.New("refused")
	user.Hooks = hooks.Lifecycle[user.Ins, user.Mut, user.Row]{
		BeforeCreate: func(context.Context, runtime.Executor, *user.Ins) error { return refuse },
		BeforeUpdate: func(context.Context, runtime.Executor, *user.Mut) error { return refuse },
		BeforeDelete: func(context.Context, runtime.Executor, *user.Row) error { return refuse },
	}
	count.Reset()
	n := user.Create()
	n.SetEmail("never@example.com")
	if _, err := n.Insert(ctx, ex); !errors.Is(err, refuse) {
		t.Errorf("Ins.Insert = %v, want the hook's error", err)
	}
	row := user.Row{Email: "never@example.com"}
	if err := user.Insert(ctx, ex, &row); !errors.Is(err, refuse) {
		t.Errorf("Insert = %v, want the hook's error", err)
	}
	m = user.Mutate(r)
	m.SetName("x")
	if err := m.Update(ctx, ex); !errors.Is(err, refuse) {
		t.Errorf("Update = %v, want the hook's error", err)
	}
	if err := user.Delete(ctx, ex, r.ID); !errors.Is(err, refuse) {
		t.Errorf("Delete = %v, want the hook's error", err)
	}
	if n := count.RoundTrips(); n != 0 {
		t.Errorf("%d statements sent after a Before hook refused", n)
	}
}

// An After hook's error reaches the caller, and inside storm.InTx it rolls the
// write back — which is what makes AfterCreate usable for an audit row.
func TestLifecycleHooks_AfterErrorRollsBackInTx(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(func() { user.Hooks = hooks.Lifecycle[user.Ins, user.Mut, user.Row]{} })
	boom := errors.New("audit failed")
	user.Hooks.AfterCreate = func(context.Context, runtime.Executor, *user.Row) error { return boom }
	org := anOrg(t)

	err := runtime.InTx(ctx, pgxdrv.Pool{P: pool}, func(ex runtime.Executor) error {
		n := user.Create()
		n.SetEmail("rolledback@example.com")
		n.SetName("x")
		n.SetStatus("pending")
		n.SetOrgID(org)
		_, err := n.Insert(ctx, ex)
		return err
	})
	if !errors.Is(err, boom) {
		t.Fatalf("InTx = %v, want AfterCreate's error", err)
	}
	user.Hooks = hooks.Lifecycle[user.Ins, user.Mut, user.Row]{}
	ex, _ := db(t)
	if ok, err := user.New().EmailEq("rolledback@example.com").Exists(ctx, ex); err != nil || ok {
		t.Fatalf("the insert survived its AfterCreate error: exists=%v err=%v", ok, err)
	}
}

// Statement hooks see what lifecycle hooks do not: every statement, with its
// outcome, through one wrapped executor.
func TestStatementHooks_SeeGeneratedStatements(t *testing.T) {
	ctx := context.Background()
	base, _ := db(t)
	var seen []hooks.Statement
	ex := hooks.Wrap(base, hooks.Hooks{After: func(_ context.Context, s *hooks.Statement) { seen = append(seen, *s) }})

	r := mustCreate(t, ctx, ex, "stmt-hooks@example.com", "S")
	if _, err := user.New().IDEq(r.ID).List(ctx, ex); err != nil {
		t.Fatal(err)
	}
	if err := user.Delete(ctx, ex, r.ID); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 {
		t.Fatalf("saw %d statements, want 3: %+v", len(seen), seen)
	}
	if seen[0].Kind != hooks.KindQuery || seen[0].Rows != 1 || seen[0].Err != nil {
		t.Errorf("insert: %+v", seen[0])
	}
	if seen[1].Kind != hooks.KindQuery || seen[1].Rows != 1 {
		t.Errorf("read: %+v", seen[1])
	}
	if seen[2].Kind != hooks.KindExec || seen[2].Rows != 1 {
		t.Errorf("delete: %+v", seen[2])
	}
}
