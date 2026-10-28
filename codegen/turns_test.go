package codegen_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/codegen"
)

// A queue that takes turns by tenant: each tenant's first rows numbered, and
// every tenant's first ahead of anyone's second, in one statement whose groups
// are found a seek at a time from a starting tenant, wrapping round.

type tnOrg struct {
	storm.Model
	Name string
}

func (o *tnOrg) Schema(t *storm.Table) { t.Col(&o.Name).Size(16) }

type tnJob struct {
	storm.Model
	Org       *tnOrg
	Status    string
	DueAt     time.Time
	DeletedAt *time.Time
}

func (j *tnJob) Schema(t *storm.Table) {
	t.Col(&j.Status).Size(16)
	t.Index(&j.Status, &j.Org)
	t.SoftDelete(&j.DeletedAt)
	t.TakesTurns(&j.Org)
}

func TestTakesTurnsAgainstPostgres(t *testing.T) {
	if os.Getenv("STORM_DSN") == "" {
		t.Skip("STORM_DSN unset")
	}
	s, err := storm.Build(&tnOrg{}, &tnJob{})
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	base := "tnlive" + strconv.Itoa(os.Getpid())
	dir := filepath.Join(root, "internal", base)
	t.Cleanup(func() { os.RemoveAll(dir) })

	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir: dir, Import: "github.com/gsoultan/storm",
		Package: "store", PackageImport: "github.com/gsoultan/storm/internal/" + base,
	})
	if err != nil {
		t.Fatal(err)
	}
	for rel, src := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, src, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	src := strings.ReplaceAll(turnsLiveSrc, "IMPORTPATH", "github.com/gsoultan/storm/internal/"+base)
	src = strings.ReplaceAll(src, "MODELS", turnsModels)
	if err := os.WriteFile(filepath.Join(dir, "live_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "test", "-count=1", "-v", "./internal/"+base+"/")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("taking turns does not deal what it says:\n%s", out)
	}
	for _, name := range []string{
		"TestEveryGroupsFirstTurnComesFirst",
		"TestAGroupGetsMoreTurnsWhenTheOthersRunOut",
		"TestTheGroupsStartFromAndWrap",
		"TestTheGroupsAndTheRowsHaveTheirOwnPredicates",
		"TestAllTurnsRefusesWhatItCannotDeal",
	} {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("%s did not run:\n%s", name, out)
		}
	}
}

func TestTakesTurnsWantsAColumn(t *testing.T) {
	if _, err := storm.Build(&tnOrg{}, &tnOdd{}); err == nil || !strings.Contains(err.Error(), "TakesTurns") {
		t.Errorf("TakesTurns on a field that is no column built: %v", err)
	}
}

type tnOdd struct {
	storm.Model
	Notes []tnJob
}

func (o *tnOdd) Schema(t *storm.Table) {
	var j tnJob
	t.TakesTurns(&j.Status)
}

const turnsModels = `type tnOrg struct {
	storm.Model
	Name string
}

func (o *tnOrg) Schema(t *storm.Table) { t.Col(&o.Name).Size(16) }

type tnJob struct {
	storm.Model
	Org       *tnOrg
	Status    string
	DueAt     time.Time
	DeletedAt *time.Time
}

func (j *tnJob) Schema(t *storm.Table) {
	t.Col(&j.Status).Size(16)
	t.Index(&j.Status, &j.Org)
	t.SoftDelete(&j.DeletedAt)
	t.TakesTurns(&j.Org)
}
`

const turnsLiveSrc = `package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/pgddl"
	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/pgxdrv"
	"github.com/jackc/pgx/v5/pgxpool"

	"IMPORTPATH/tnjob"
	"IMPORTPATH/tnorg"
)

MODELS

var (
	pool *pgxpool.Pool
	ex   runtime.Executor
)

func TestMain(m *testing.M) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "STORM_DSN did not reach the generated package")
		os.Exit(1)
	}
	ctx := context.Background()
	s, err := storm.Build(&tnOrg{}, &tnJob{})
	must(err)
	p, err := pgxdrv.NewPool(ctx, dsn)
	must(err)
	const ns = "storm_takes_turns_live"
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

var (
	orgA = [16]byte{1}
	orgB = [16]byte{2}
	orgC = [16]byte{3}
	now  = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
)

// seed writes five due jobs for A, two for B, one for C and two with no
// organization, each a minute apart in its group, every group starting at the
// same minute; and, out of the deal, one of B's that is not due, one of C's
// that is deleted and one of a fourth organization's that is done.
func seed(t *testing.T) (names map[[16]byte]string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "DELETE FROM tn_jobs; DELETE FROM tn_orgs"); err != nil {
		t.Fatal(err)
	}
	orgD := [16]byte{4}
	for i, id := range [][16]byte{orgA, orgB, orgC, orgD} {
		o := tnorg.Create()
		o.SetID(id)
		o.SetName(string(rune('a' + i)))
		if _, err := o.Insert(ctx, ex); err != nil {
			t.Fatal(err)
		}
	}
	names = map[[16]byte]string{}
	add := func(name string, org *[16]byte, status string, due time.Time, gone bool) {
		t.Helper()
		j := tnjob.Create()
		if org != nil {
			j.SetOrgID(*org)
		}
		j.SetStatus(status)
		j.SetDueAt(due)
		row, err := j.Insert(ctx, ex)
		if err != nil {
			t.Fatal(err)
		}
		names[row.ID] = name
		if gone {
			if err := tnjob.Delete(ctx, ex, row.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := 0; i < 5; i++ {
		add(fmt.Sprintf("a%d", i+1), &orgA, "pending", now.Add(time.Duration(i-10)*time.Minute), false)
	}
	for i := 0; i < 2; i++ {
		add(fmt.Sprintf("b%d", i+1), &orgB, "pending", now.Add(time.Duration(i-10)*time.Minute+time.Second), false)
	}
	add("c1", &orgC, "pending", now.Add(-10*time.Minute+2*time.Second), false)
	for i := 0; i < 2; i++ {
		add(fmt.Sprintf("n%d", i+1), nil, "pending", now.Add(time.Duration(i-10)*time.Minute+3*time.Second), false)
	}
	add("b-later", &orgB, "pending", now.Add(time.Hour), false)
	add("c-gone", &orgC, "pending", now.Add(-20*time.Minute), true)
	add("d-done", &orgD, "done", now.Add(-20*time.Minute), false)
	return names
}

func due() tnjob.Query {
	return tnjob.New().Where(tnjob.Status.Eq("pending"), tnjob.DueAt.Lte(now)).Order(tnjob.DueAt.Asc(), tnjob.ID.Asc())
}

func pending() tnjob.Query { return tnjob.New().Where(tnjob.Status.Eq("pending")) }

func dealt(t *testing.T, names map[[16]byte]string, rows []tnjob.TurnRow) string {
	t.Helper()
	var out []string
	for _, r := range rows {
		out = append(out, fmt.Sprintf("%s:%d", names[r.ID], r.Turn))
	}
	return fmt.Sprint(out)
}

func TestEveryGroupsFirstTurnComesFirst(t *testing.T) {
	names := seed(t)
	rows, err := due().Limit(4).AllTurns(context.Background(), ex, pending(), [16]byte{}, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := dealt(t, names, rows), "[a1:1 b1:1 c1:1 n1:1]"; got != want {
		t.Errorf("dealt %s, want %s", got, want)
	}
}

func TestAGroupGetsMoreTurnsWhenTheOthersRunOut(t *testing.T) {
	names := seed(t)
	rows, err := due().Limit(10).AllTurns(context.Background(), ex, pending(), [16]byte{}, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := dealt(t, names, rows), "[a1:1 b1:1 c1:1 n1:1 a2:2 b2:2 n2:2 a3:3 a4:4 a5:5]"; got != want {
		t.Errorf("dealt %s, want %s", got, want)
	}
}

func TestTheGroupsStartFromAndWrap(t *testing.T) {
	names := seed(t)
	ctx := context.Background()
	rows, err := due().Limit(10).AllTurns(ctx, ex, pending(), orgB, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := dealt(t, names, rows), "[b1:1 n1:1 b2:2 n2:2]"; got != want {
		t.Errorf("from B, one group: dealt %s, want %s", got, want)
	}
	rows, err = due().Limit(10).AllTurns(ctx, ex, pending(), [16]byte{9}, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := dealt(t, names, rows), "[a1:1 b1:1 n1:1 a2:2 b2:2 n2:2 a3:3 a4:4 a5:5]"; got != want {
		t.Errorf("from past the last, two groups: dealt %s, want %s", got, want)
	}
	rows, err = due().Limit(10).AllTurns(ctx, ex, pending(), orgC, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := dealt(t, names, rows), "[a1:1 c1:1 n1:1 a2:2 n2:2 a3:3 a4:4 a5:5]"; got != want {
		t.Errorf("from C, two groups, wrapping: dealt %s, want %s", got, want)
	}
}

// The groups are chosen by their own predicates: B is a group for having a
// pending job, though none of its due ones is left once b1 and b2 are done;
// D is not, though its row is due, because it is not pending.
func TestTheGroupsAndTheRowsHaveTheirOwnPredicates(t *testing.T) {
	names := seed(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "UPDATE tn_jobs SET status = 'done' WHERE org_id = $1 AND due_at < $2", orgB, now); err != nil {
		t.Fatal(err)
	}
	rows, err := due().Limit(10).AllTurns(ctx, ex, pending(), orgB, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := dealt(t, names, rows), "[n1:1 n2:2]"; got != want {
		t.Errorf("dealt %s, want only the unowned rows: B is a group with nothing due", got)
	}
	anyStatus := tnjob.New().Where(tnjob.DueAt.Lte(now))
	rows, err = due().Limit(10).AllTurns(ctx, ex, anyStatus, [16]byte{4}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := dealt(t, names, rows), "[n1:1 n2:2]"; got != want {
		t.Errorf("dealt %s, want %s: D's row is not pending", got, want)
	}
}

func TestAllTurnsRefusesWhatItCannotDeal(t *testing.T) {
	seed(t)
	ctx := context.Background()
	for name, call := range map[string]func() ([]tnjob.TurnRow, error){
		"no groups":  func() ([]tnjob.TurnRow, error) { return due().AllTurns(ctx, ex, pending(), [16]byte{}, 0, nil) },
		"an offset":  func() ([]tnjob.TurnRow, error) { return due().Offset(1).AllTurns(ctx, ex, pending(), [16]byte{}, 1, nil) },
		"a row lock": func() ([]tnjob.TurnRow, error) { return due().ForUpdate().AllTurns(ctx, ex, pending(), [16]byte{}, 1, nil) },
	} {
		if _, err := call(); err == nil || errors.Is(err, context.Canceled) {
			t.Errorf("%s was dealt: %v", name, err)
		}
	}
}
`
