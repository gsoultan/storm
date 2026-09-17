package storm_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/migrate"
)

// A composite foreign key is what stops a child in one tenant pointing at a
// parent in another. A single-column key to identities(id) cannot: the tenant
// has to travel IN the key. These tests hold that property against a real
// server, because "the DDL mentions the constraint" and "the database refuses
// the row" are different claims and only the second one matters.

type ctenant struct {
	storm.Model
	Name string
}

type cidentity struct {
	storm.Model
	Tenant ctenant
	Email  string
}

func (i *cidentity) Schema(t *storm.Table) {
	// The composite unique is what a composite key can point AT.
	t.Unique(&i.ID, &i.Tenant)
}

type cgrant struct {
	storm.Model
	TenantID   storm.UUID
	IdentityID storm.UUID
	Role       string
}

func (g *cgrant) Schema(t *storm.Table) {
	var i cidentity
	t.ForeignKey(&g.IdentityID, &g.TenantID).
		References(&i, &i.ID, &i.Tenant).
		OnDelete(storm.Cascade).
		Named("cgrants_identity_tenant_fkey")
}

func compositeModel() []any { return []any{&ctenant{}, &cidentity{}, &cgrant{}} }

func TestCompositeForeignKeyRoundTrips(t *testing.T) {
	c := connect(t)
	ctx := context.Background()
	const ns = "storm_composite_fk"
	t.Cleanup(func() { _, _ = c.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+ns+" CASCADE") })
	if _, err := c.Exec(ctx, "DROP SCHEMA IF EXISTS "+ns+" CASCADE; CREATE SCHEMA "+ns); err != nil {
		t.Fatal(err)
	}

	s, err := storm.Build(compositeModel()...)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := migrate.ForWith(ctx, c, ns, s, migrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.SQL(), "cgrants_identity_tenant_fkey") {
		t.Fatalf("the composite key is not in the plan:\n%s", plan.SQL())
	}
	if _, err := c.Exec(ctx, "SET search_path TO "+ns); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, plan.SQL()); err != nil {
		t.Fatalf("plan does not apply: %v\n---\n%s", err, plan.SQL())
	}
	again, err := migrate.ForWith(ctx, c, ns, s, migrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Empty() {
		t.Fatalf("a composite foreign key does not round-trip:\n%s", again.SQL())
	}
}

// The property the constraint exists for. Without the tenant in the key this
// insert succeeds and one tenant's grant points at another tenant's identity.
func TestCompositeForeignKeyRefusesACrossTenantRow(t *testing.T) {
	c := connect(t)
	ctx := context.Background()
	const ns = "storm_composite_xt"
	t.Cleanup(func() { _, _ = c.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+ns+" CASCADE") })
	if _, err := c.Exec(ctx, "DROP SCHEMA IF EXISTS "+ns+" CASCADE; CREATE SCHEMA "+ns); err != nil {
		t.Fatal(err)
	}
	s, err := storm.Build(compositeModel()...)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := migrate.ForWith(ctx, c, ns, s, migrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "SET search_path TO "+ns); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, plan.SQL()); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Exec(ctx, `
		INSERT INTO ctenants(id, name) VALUES
			('11111111-1111-1111-1111-111111111111','a'),
			('22222222-2222-2222-2222-222222222222','b');
		INSERT INTO cidentities(id, tenant_id, email) VALUES
			('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa','11111111-1111-1111-1111-111111111111','a@x');`); err != nil {
		t.Fatal(err)
	}

	// Same identity, WRONG tenant. The identity exists; the pair does not.
	_, err = c.Exec(ctx, `
		INSERT INTO cgrants(id, tenant_id, identity_id, role)
		VALUES ('bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb',
		        '22222222-2222-2222-2222-222222222222',
		        'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa','admin')`)
	if err == nil {
		t.Fatal("a grant in tenant b was allowed to reference an identity in tenant a")
	}
	if !strings.Contains(err.Error(), "cgrants_identity_tenant_fkey") {
		t.Fatalf("refused, but not by the composite key: %v", err)
	}

	// The same row in the RIGHT tenant must still be accepted, or the
	// constraint is just rejecting everything.
	if _, err := c.Exec(ctx, `
		INSERT INTO cgrants(id, tenant_id, identity_id, role)
		VALUES ('cccccccc-cccc-cccc-cccc-cccccccccccc',
		        '11111111-1111-1111-1111-111111111111',
		        'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa','admin')`); err != nil {
		t.Fatalf("the in-tenant row was refused too: %v", err)
	}
}

// Pointing at columns that are not a key is the mistake this catches at BUILD
// time, naming the model that has to change. PostgreSQL would only say so when
// the DDL was applied, against a generated statement instead of a declaration.
func TestCompositeForeignKeyNeedsAKeyOnTheFarSide(t *testing.T) {
	_, err := storm.Build(&ctenant{}, &cidentity{}, &badgrant{})
	if err == nil {
		t.Fatal("a foreign key to columns with no unique constraint was accepted")
	}
	if !strings.Contains(err.Error(), "unique") || !strings.Contains(err.Error(), "cidentity") {
		t.Fatalf("the error does not say what is missing or where: %v", err)
	}
}

// badgrant points at (id, email), which cidentity has no key on.
type badgrant struct {
	storm.Model
	IdentityID storm.UUID
	Email      string
}

func (g *badgrant) Schema(t *storm.Table) {
	var i cidentity
	t.ForeignKey(&g.IdentityID, &g.Email).References(&i, &i.ID, &i.Email)
}

// Arity is checked where it is declared, not left to produce a confusing error
// from the far side later.
func TestCompositeForeignKeyChecksArity(t *testing.T) {
	_, err := storm.Build(&ctenant{}, &cidentity{}, &shortgrant{})
	if err == nil {
		t.Fatal("a 2-column key referencing 1 column was accepted")
	}
	if !strings.Contains(err.Error(), "must match") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

type shortgrant struct {
	storm.Model
	IdentityID storm.UUID
	TenantID   storm.UUID
}

func (g *shortgrant) Schema(t *storm.Table) {
	var i cidentity
	t.ForeignKey(&g.IdentityID, &g.TenantID).References(&i, &i.ID)
}
