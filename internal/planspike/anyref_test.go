package planspike_test

import (
	"context"
	"testing"

	"github.com/gsoultan/storm/internal/planspike/store/auditlog"
)

// The discriminator pair against a real server.
//
// An arc keeps referential integrity and this gives it up, so the only thing
// the database enforces here is that both columns are present. Everything
// else — that the type names a real table, that the id names a real row — is
// the application's problem, which is exactly what AcknowledgeNoFK makes
// somebody write down.
//
// The subject ids are synthetic on purpose. An earlier version of this test
// inserted a real post so the reference would point at something, which meant
// creating a user, which put a 501st row in the org three other tests assert
// has exactly 500 — caught by -shuffle=on. Needing a real subject was also the
// wrong instinct: not needing one IS the shape.
func TestAnyRefRoundTrips(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	subject := [16]byte{0xa1, 0x1e, 0xf0, 1}

	n := auditlog.Create()
	n.SetAction("post.published")
	n.SetSubjectType("posts")
	n.SetSubjectID(subject)
	rec, err := n.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if rec.SubjectType != "posts" || rec.SubjectID != subject {
		t.Fatalf("round-tripped as (%q, %x)", rec.SubjectType, rec.SubjectID)
	}

	// Both halves filter, and the pair is what a lookup actually asks for.
	rows, err := auditlog.New().
		Where(auditlog.SubjectType.Eq("posts"), auditlog.SubjectID.Eq(subject)).
		All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != rec.ID {
		t.Fatalf("lookup by (type, id) returned %d rows", len(rows))
	}

	// The id alone must not be enough. Two tables can hold the same uuid, and
	// matching on it would be the failure mode a discriminator invites.
	other, err := auditlog.New().
		Where(auditlog.SubjectType.Eq("comments"), auditlog.SubjectID.Eq(subject)).
		All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Errorf("a different subject type matched %d rows", len(other))
	}
}

// Nothing stops an AnyRef naming a row that does not exist. That is not a bug
// to fix — it is the property the acknowledgement is about — and asserting it
// keeps anyone from "fixing" it into a foreign key the shape cannot have.
func TestAnyRefAcceptsAnOrphan(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	n := auditlog.Create()
	n.SetAction("thing.deleted")
	n.SetSubjectType("posts")
	n.SetSubjectID([16]byte{0xa1, 0x1e, 0xf0, 2})
	if _, err := n.Insert(ctx, ex); err != nil {
		t.Fatalf("an orphan reference was refused: %v — AnyRef cannot have a foreign key", err)
	}
}
