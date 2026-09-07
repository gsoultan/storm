package storm_test

import (
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/pgddl"
)

type serialModel struct {
	Seq   int64
	Small int16
	Name  string
}

func (m *serialModel) Schema(t *storm.Table) {
	t.Name("serial_models")
	t.Col(&m.Seq).Serial()
	t.Col(&m.Small).Identity()
	t.PrimaryKey(&m.Seq)
}

// The whole loop, against a live server: a model that says Serial emits
// bigserial, applies, and comes BACK as Serial — not as a bigint carrying a
// nextval default that names the sequence of the one database it was read
// from. That default is what `storm import` used to put in a model, and it is
// why an imported model could never pass `storm verify`: the scratch apply
// referenced a sequence that existed nowhere else.
//
// Identity travels beside it to prove the two stay distinct. Postgres backs
// both with an owned sequence, so a reader that only asked "does this column
// own a sequence?" would call identity a serial and quietly propose an ALTER
// on an adopter's first diff.
func TestSerialRoundTrips(t *testing.T) {
	c := connect(t)

	s, err := storm.Build(&serialModel{})
	if err != nil {
		t.Fatal(err)
	}
	got := applyInto(t, c, "storm_serial_rt", pgddl.Create(s))

	tbl := got.Table("serial_models")
	if tbl == nil {
		t.Fatal("serial_models did not come back")
	}
	seq, small := tbl.Column("seq"), tbl.Column("small")
	if seq == nil || small == nil {
		t.Fatal("columns did not come back")
	}
	if !seq.Serial {
		t.Errorf("seq came back Serial=false (default %q, identity %v)", seq.Default, seq.Identity)
	}
	if seq.Default != "" {
		t.Errorf("seq carries a default as well as being serial: %q — that default names "+
			"this database's sequence and applies nowhere else", seq.Default)
	}
	if small.Serial {
		t.Error("an identity column came back as Serial; the two facts collapsed")
	}
	if !small.Identity {
		t.Errorf("small came back Identity=false (default %q)", small.Default)
	}

	// And it is a fixpoint: what we emit from the introspected IR is what we
	// emitted in the first place.
	if a, b := pgddl.Create(s), pgddl.Create(got); a != b {
		t.Errorf("serial is not a fixpoint:\n--- from model ---\n%s\n--- from database ---\n%s", a, b)
	}
}
