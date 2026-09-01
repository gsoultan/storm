package storm_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm"
)

type jCustomer struct {
	storm.Model
	Email string
	Name  string
}

type jOrder struct {
	storm.Model
	Customer jCustomer
	Total    storm.Decimal
	Status   string
}

func (o *jOrder) Aggregates(a *storm.Aggregates) {
	a.Named("ByCustomer").By(&o.Customer).Count("Orders").Sum(&o.Total, "Spend")
}

func (o *jOrder) Joins(j *storm.Joins) {
	var c jCustomer
	j.Named("WithCustomer").
		Inner(&c, &o.Customer).
		Take(&o.ID, "OrderID").
		Take(&c.Email, "Email").
		OrderDesc(&o.CreatedAt)

	j.Named("Lifetime").
		With("spend", &jOrder{}, "ByCustomer").
		Inner(&c, &o.Customer).
		LeftWith("spend", storm.OnCols("spend", "customer_id", &c.ID)).
		Take(&o.ID, "OrderID").
		TakeFrom("spend", "Spend", "Lifetime")
}

func TestJoinBuilds(t *testing.T) {
	s, err := storm.Build(&jOrder{}, &jCustomer{})
	if err != nil {
		t.Fatal(err)
	}
	tbl := s.Table("j_orders")
	if tbl == nil {
		t.Fatal("no table")
	}
	if len(tbl.Joins) != 2 {
		t.Fatalf("got %d joins, want 2", len(tbl.Joins))
	}
	wc := tbl.Joins[0]
	if len(wc.Tables) != 1 || wc.Tables[0].Table != "j_customers" {
		t.Errorf("WithCustomer joins %+v", wc.Tables)
	}
	// The FK relation supplied the ON clause; nothing was spelled.
	if wc.Tables[0].On.Left.Col != "customer_id" || wc.Tables[0].On.Right.Col != "id" {
		t.Errorf("ON is %+v", wc.Tables[0].On)
	}
	lt := tbl.Joins[1]
	if len(lt.CTEs) != 1 || lt.CTEs[0].Aggregate != "ByCustomer" {
		t.Errorf("CTEs = %+v", lt.CTEs)
	}
	// Through a LEFT join, even a count is nullable.
	for _, c := range lt.Select {
		if c.As == "Lifetime" && !c.Nullable {
			t.Error("a column taken through a LEFT join is not nullable")
		}
	}
}

// ---- refusals ---------------------------------------------------------------

type jStray struct {
	storm.Model
	Name string
}

type jBadPtr struct {
	storm.Model
	Customer jCustomer
}

func (o *jBadPtr) Joins(j *storm.Joins) {
	var stray jStray // never joined
	var c jCustomer
	j.Named("X").Inner(&c, &o.Customer).Take(&stray.Name, "Name")
}

type jNotFK struct {
	storm.Model
	Label string
}

func (o *jNotFK) Joins(j *storm.Joins) {
	var c jCustomer
	j.Named("X").Inner(&c, &o.Label) // Label is not a foreign key
}

type jMissingAgg struct {
	storm.Model
	Customer jCustomer
}

func (o *jMissingAgg) Joins(j *storm.Joins) {
	var c jCustomer
	j.Named("X").
		With("spend", &jMissingAgg{}, "NoSuchAggregate").
		Inner(&c, &o.Customer).
		Take(&o.ID, "ID")
}

type jDupOut struct {
	storm.Model
	Customer jCustomer
}

func (o *jDupOut) Joins(j *storm.Joins) {
	var c jCustomer
	j.Named("X").Inner(&c, &o.Customer).Take(&o.ID, "N").Take(&c.Email, "N")
}

// Every one of these is a mistake PostgreSQL would raise at execution, or
// silently answer wrongly. Build time is where they belong.
func TestJoinDeclarationsAreCheckedAtBuildTime(t *testing.T) {
	for _, c := range []struct {
		name  string
		model any
		want  []string
	}{
		{"field pointer into an unjoined model", &jBadPtr{}, []string{"does not point into"}},
		{"joined on something that is not a FK", &jNotFK{}, []string{"foreign key"}},
		{"CTE names a missing aggregate", &jMissingAgg{}, []string{"NoSuchAggregate"}},
		{"two outputs share a name", &jDupOut{}, []string{`"N"`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := storm.Build(c.model, &jCustomer{}, &jStray{})
			if err == nil {
				t.Fatal("accepted")
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error does not mention %q: %v", w, err)
				}
			}
		})
	}
}
