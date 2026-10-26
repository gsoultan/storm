package codegen_test

// dangleLiveSrc is the test written beside the generated context package.
// IMPORTPATH is that package's import path, substituted before it is written.
const dangleLiveSrc = `package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/pgddl"
	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/pgxdrv"
	"github.com/jackc/pgx/v5/pgxpool"

	store "IMPORTPATH"
	"IMPORTPATH/nfevent"
	"IMPORTPATH/nfuser"
)

type nfUser struct {
	storm.Model
	Name string
}

func (u *nfUser) Schema(t *storm.Table) { t.Col(&u.Name).Size(40) }

type nfEvent struct {
	storm.Model
	Kind    string
	Owner   nfUser
	Subject nfUser
	Witness *nfUser
}

func (e *nfEvent) Schema(t *storm.Table) {
	t.Col(&e.Kind).Size(40)
	t.Col(&e.Subject).AcknowledgeNoFK("events outlive what they describe")
	t.Col(&e.Witness).AcknowledgeNoFK("events outlive what they describe")
}

func (e *nfEvent) Plans(p *storm.Plans) { p.Named("Story").With(&e.Owner).With(&e.Subject) }

var (
	pool *pgxpool.Pool
	ex   runtime.Executor
	// alice is there; gone never was.
	alice = [16]byte{1, 15: 1}
	gone  = [16]byte{9, 15: 9}
	// seen names alice; dangling names a subject that is not there.
	seen     = [16]byte{2, 15: 2}
	dangling = [16]byte{3, 15: 3}
)

func TestMain(m *testing.M) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "STORM_DSN did not reach the generated package")
		os.Exit(1)
	}
	ctx := context.Background()
	s, err := storm.Build(&nfUser{}, &nfEvent{})
	must(err)
	p, err := pgxdrv.NewPool(ctx, dsn)
	must(err)
	const ns = "storm_dangle_live"
	_, err = p.Exec(ctx, "DROP SCHEMA IF EXISTS "+ns+" CASCADE; CREATE SCHEMA "+ns)
	must(err)
	_, err = p.Exec(ctx, "SET search_path TO "+ns+"; "+pgddl.Create(s))
	must(err)
	cfg := p.Config()
	cfg.ConnConfig.RuntimeParams["search_path"] = ns
	p.Close()
	pool, err = pgxpool.NewWithConfig(ctx, cfg)
	must(err)
	ex = pgxdrv.Pool{P: pool}

	must(nfuser.Insert(ctx, ex, &nfuser.Row{ID: alice, Name: "alice"}))
	must(nfevent.Insert(ctx, ex, &nfevent.Row{ID: seen, Kind: "seen", OwnerID: alice, SubjectID: alice,
		WitnessID: runtime.Null[[16]byte]{V: alice, Valid: true}}))
	// Accepted: the column has no foreign key.
	must(nfevent.Insert(ctx, ex, &nfevent.Row{ID: dangling, Kind: "dangling", OwnerID: alice, SubjectID: gone,
		WitnessID: runtime.Null[[16]byte]{V: gone, Valid: true}}))

	code := m.Run()
	_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+ns+" CASCADE")
	pool.Close()
	os.Exit(code)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func TestAPlanLoadsAMissingSubjectAsNil(t *testing.T) {
	ctx := context.Background()
	rows, err := store.NfEventWithSubject().All(ctx, ex)
	if err != nil {
		t.Fatalf("a missing subject failed the read: %v", err)
	}
	by := map[[16]byte]store.NfEventWithSubjectRow{}
	for _, r := range rows {
		by[r.ID] = r
	}
	if len(by) != 2 {
		t.Fatalf("%d events, want 2", len(by))
	}
	if s := by[seen].Subject; s == nil || s.Name != "alice" {
		t.Errorf("an event whose subject is there loaded %+v", s)
	}
	if s := by[dangling].Subject; s != nil {
		t.Errorf("an event whose subject is gone loaded %+v, want nil", s)
	}

	witnessed, err := store.NfEventWithWitness().Where(nfevent.ID.Eq(dangling)).All(ctx, ex)
	if err != nil {
		t.Fatalf("a missing witness failed the read: %v", err)
	}
	if len(witnessed) != 1 || witnessed[0].Witness != nil {
		t.Errorf("an event whose witness is gone loaded %+v, want nil", witnessed)
	}
}

func TestANamedPlanLoadsAMissingSubjectAsNil(t *testing.T) {
	rows, err := store.NfEventStory().Where(nfevent.ID.Eq(dangling)).All(context.Background(), ex)
	if err != nil {
		t.Fatalf("a missing subject failed the named plan: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d events, want 1", len(rows))
	}
	if rows[0].Owner.Name != "alice" {
		t.Errorf("the owner, which is there, loaded %+v", rows[0].Owner)
	}
	if rows[0].Subject != nil {
		t.Errorf("the subject, which is gone, loaded %+v, want nil", rows[0].Subject)
	}
}
`
