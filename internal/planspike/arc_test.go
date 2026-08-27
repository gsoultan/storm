package planspike_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gsoultan/storm/internal/planspike/store"
	"github.com/gsoultan/storm/internal/planspike/store/attachment"
	"github.com/gsoultan/storm/internal/planspike/store/post"
	"github.com/gsoultan/storm/internal/planspike/store/user"
	"github.com/gsoultan/storm/runtime"
)

// THE GATE: every variant of an arc loads in ONE batched round trip. One query
// per variant table is unavoidable — different tables, different columns — but
// they need not be different conversations, or an arc is an N+1 in the number
// of variants.
func TestArc_AllVariantsInOneBatchedRoundTrip(t *testing.T) {
	ctx := context.Background()
	ex, count := db(t)

	u := mustCreate(t, ctx, ex, "arc@example.com", "Arc")
	np := post.Create()
	np.SetTitle("arc post")
	np.SetBody("body")
	np.SetAuthorID(u.ID)
	pr, err := np.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}

	var ids [][16]byte
	for _, set := range []func(*attachment.Ins){
		func(a *attachment.Ins) { a.SetPostID(pr.ID) },
		func(a *attachment.Ins) { a.SetUserID(u.ID) },
	} {
		a := attachment.Create()
		a.SetFilename("f.txt")
		set(&a)
		r, err := a.Insert(ctx, ex)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
	}
	t.Cleanup(func() {
		for _, id := range ids {
			_ = attachment.Delete(ctx, ex, id)
		}
		_ = post.Delete(ctx, ex, pr.ID)
		_ = user.Delete(ctx, ex, u.ID)
	})

	count.Reset()
	rows, err := store.AttachmentWithSubject().
		Where(attachment.ID.In(ids...)).All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d attachments, want 2", len(rows))
	}
	// One round trip for the attachments, ONE for every variant together.
	if n := count.RoundTrips(); n != 2 {
		t.Errorf("%d round trips for a two-variant load, want 2", n)
	}

	var sawPost, sawUser bool
	for _, r := range rows {
		switch {
		case r.Post != nil:
			sawPost = true
			if r.Post.ID != pr.ID {
				t.Error("the post variant resolved to the wrong row")
			}
			if r.User != nil || r.Comment != nil {
				t.Error("more than one variant is set — the CHECK should forbid it")
			}
		case r.User != nil:
			sawUser = true
			if r.User.ID != u.ID {
				t.Error("the user variant resolved to the wrong row")
			}
		default:
			t.Error("an attachment resolved to no variant at all")
		}
	}
	if !sawPost || !sawUser {
		t.Error("not every variant came back")
	}
}

// The database enforces exactly-one, not the ORM. Two variants set at once must
// be rejected by the CHECK.
func TestArc_ExactlyOneIsEnforcedByTheDatabase(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	u := mustCreate(t, ctx, ex, "arc2@example.com", "Arc2")
	t.Cleanup(func() { _ = user.Delete(ctx, ex, u.ID) })

	np := post.Create()
	np.SetTitle("p")
	np.SetBody("b")
	np.SetAuthorID(u.ID)
	pr, err := np.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = post.Delete(ctx, ex, pr.ID) })

	two := attachment.Create()
	two.SetFilename("two.txt")
	two.SetPostID(pr.ID)
	two.SetUserID(u.ID)
	if _, err := two.Insert(ctx, ex); err == nil {
		t.Error("two variants at once must violate the exactly-one CHECK")
	}

	none := attachment.Create()
	none.SetFilename("none.txt")
	if _, err := none.Insert(ctx, ex); err == nil {
		t.Error("no variant at all must violate the exactly-one CHECK")
	}
}

// A type switch is not exhaustive; Match is. Adding a variant changes the
// arity, so every call site fails to compile rather than falling through to a
// default nobody revisited.
func TestArc_MatchDispatchesOnTheVariant(t *testing.T) {
	got := attachment.MatchSubject(
		attachment.Row{PostID: nullUUID([16]byte{7})},
		func([16]byte) string { return "post" },
		func([16]byte) string { return "comment" },
		func([16]byte) string { return "user" },
	)
	if got != "post" {
		t.Errorf("Match returned %q, want post", got)
	}
	if v := attachment.SubjectVariant(attachment.Row{UserID: nullUUID([16]byte{9})}); v != "users" {
		t.Errorf("SubjectVariant = %q, want users", v)
	}
}

// The predicates name the variant, not the column, so the call site does not
// change if the arc is ever re-lowered to a discriminator.
func TestArc_VariantPredicates(t *testing.T) {
	sql := attachment.New().Where(attachment.SubjectIsPost()).SQL()
	if !strings.Contains(sql, `"post_id" IS NOT NULL`) {
		t.Errorf("SubjectIsPost did not lower to a null check:\n%s", sql)
	}
}

func nullUUID(v [16]byte) runtime.Null[[16]byte] {
	return runtime.Null[[16]byte]{V: v, Valid: true}
}
