// Package planspike is the M3 named-plan ergonomics spike.
//
// It hand-writes what `raorm generate` would emit for ONE named plan —
// Org with its Users — on top of two genuinely generated table packages. The
// question it exists to answer is the only one M3 still carries: are named
// plan types usable? Four weeks is too long to spend finding out.
//
// Nothing here is the shipping design. It is the M0 method applied to M3:
// separate "is the design usable?" from "can a generator emit it?", because
// those are two failures that look identical from the outside.
//
// # Where the plan layer has to live, and why
//
// Not in package org. A plan joins two tables, so its code must name both, and
// a table package that imports a sibling reintroduces exactly the import cycle
// that one-package-per-table was chosen to avoid: Org has Users, User has an
// Org, and Go will not compile a cycle no matter how the fields are spelled.
//
// So the plan layer sits in the parent package — the bounded context — which
// imports every table package and is imported by none of them. docs/API.md
// already put plans.go there; this is the structural reason it has to be.
package planspike

import (
	"context"

	"github.com/gsoultan/raorm/internal/planspike/gen/org"
	"github.com/gsoultan/raorm/internal/planspike/gen/user"
	"github.com/gsoultan/raorm/runtime"
)

// OrgWithUsers is the row type the plan yields.
//
// This is the property the whole design exists for: Users is a field HERE and
// nowhere else. org.Row has no Users field at all, so an unloaded relation is
// not an empty slice, not a lazy fetch and not a lint warning — it does not
// compile. See testdata/compilefail/unloaded_relation.go.
type OrgWithUsers struct {
	org.Row
	Users []user.Row
}

// OrgWithUsersQuery is the plan's builder.
//
// FINDING: every builder method of the underlying Query has to be redeclared
// here. Go has no delegation, and embedding org.Query would return org.Query
// from Where(), dropping straight back out of the plan. That is pure
// boilerplate — but it is boilerplate a *generator* writes, so it costs the
// reader nothing and the author nothing. It is only a reason to stop if the
// method set is unstable, and it is not: Where, WhereIf, Any, Not, NotAny,
// Limit.
type OrgWithUsersQuery struct {
	q org.Query

	// childLimit caps children *per parent set*, not per parent. Naming it
	// here rather than defaulting it is deliberate; see the note on All.
	childLimit int64
}

// OrgsWithUsers starts the plan.
//
// FINDING: this is a package-level function, not `org.Query().Load(plan)`.
// docs/API.md §7 writes the latter, and it cannot be built: Go methods may not
// have type parameters, so a method taking a plan value has no way to vary its
// return type by plan. The plan has to be the entry point (or a generated
// method per plan) for the result to stay typed. The doc needs correcting.
func OrgsWithUsers() OrgWithUsersQuery {
	// OPEN QUESTION for M3, not answered here. Any default is arbitrary: too
	// low truncates a legitimate load, too high is the unbounded read the dba
	// profile vetoes. The spike's first default of 10,000 silently failed a
	// 50-parent x 500-child fixture on the first run, which is the evidence
	// that the number cannot be picked by feel. Candidates: derive it from
	// the parent limit times a declared per-parent bound, or require the plan
	// to state it. What is settled is the *behaviour on reaching it* — an
	// error, never a partial result.
	return OrgWithUsersQuery{q: org.New(), childLimit: 1 << 20}
}

func (p OrgWithUsersQuery) Where(ps ...org.Pred) OrgWithUsersQuery {
	p.q = p.q.Where(ps...)
	return p
}

func (p OrgWithUsersQuery) WhereIf(cond bool, pr org.Pred) OrgWithUsersQuery {
	p.q = p.q.WhereIf(cond, pr)
	return p
}

func (p OrgWithUsersQuery) Any(ps ...org.Pred) OrgWithUsersQuery {
	p.q = p.q.Any(ps...)
	return p
}

func (p OrgWithUsersQuery) Not(pr org.Pred) OrgWithUsersQuery {
	p.q = p.q.Not(pr)
	return p
}

// Limit caps parents.
func (p OrgWithUsersQuery) Limit(n int64) OrgWithUsersQuery {
	p.q = p.q.Limit(n)
	return p
}

// ChildLimit caps the total children fetched across all parents.
func (p OrgWithUsersQuery) ChildLimit(n int64) OrgWithUsersQuery {
	p.childLimit = n
	return p
}

// All runs the plan in exactly two round trips, whatever the parent count.
//
// The mechanism is `= ANY($1)`: one placeholder binds the whole id list, so
// fifty parents and five thousand parents produce the same SQL and the same
// compiled statement. No join is involved, which is why M3 was never actually
// blocked on join support.
func (p OrgWithUsersQuery) All(ctx context.Context, ex runtime.Executor) ([]OrgWithUsers, error) {
	parents, err := p.q.All(ctx, ex, nil)
	if err != nil {
		return nil, err
	}
	if len(parents) == 0 {
		// Round trip two would be `WHERE org_id = ANY('{}')`, which is a
		// guaranteed-empty query. Not issuing it is not an optimisation; it is
		// the difference between the plan costing 2 round trips and costing 2
		// when it has work to do and 2 when it does not.
		return nil, nil
	}

	out := make([]OrgWithUsers, len(parents))
	ids := make([][16]byte, len(parents))
	at := make(map[[16]byte]int, len(parents))
	for i, o := range parents {
		out[i] = OrgWithUsers{Row: o}
		ids[i] = o.ID
		at[o.ID] = i
	}

	kids, err := user.New().
		Where(user.OrgID.In(ids...)).
		Limit(p.childLimit).
		All(ctx, ex, nil)
	if err != nil {
		return nil, err
	}

	// FINDING: user.New() defaults to limit 1000. For a single-table read that
	// is a sane guard against an unbounded scan. For a relation load it is a
	// silent truncation: 200 parents with 10 children each quietly loses half
	// the children and every count in the result is wrong. Generated plan
	// loaders must set the child limit explicitly — and M3 should make an
	// exceeded child limit an *error*, not a shrug, because a plan that
	// silently returns partial relations is worse than one that fails.
	if int64(len(kids)) >= p.childLimit {
		return nil, errChildLimit
	}

	for _, k := range kids {
		if i, ok := at[k.OrgID]; ok {
			out[i].Users = append(out[i].Users, k)
		}
	}
	return out, nil
}

type childLimitError struct{}

func (childLimitError) Error() string {
	return "raorm: relation load hit its child limit — the result would be silently partial; raise ChildLimit or narrow the parent query"
}

var errChildLimit = childLimitError{}
