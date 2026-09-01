package model

import (
	st "github.com/gsoultan/storm"

	"example.com/basic/shared"
)

// Team is found by the embed rule, under an ALIASED storm import.
type Team struct {
	st.Model
	shared.Base

	Name    string
	Members []Member
}

func (t *Team) Schema(s *st.Table) { s.Unique(&t.Name) }

// Member is found by the embed rule.
type Member struct {
	st.Model
	Team  Team
	Email string
}

// Region has a NATURAL key and embeds nothing — found only by the Schema rule.
type Region struct {
	Code string
	Name string
}

func (r *Region) Schema(t *st.Table) { t.PrimaryKey(&r.Code) }

// Local is a same-package mixin.
type Local struct{ Note string }

func (l *Local) Schema(t *st.Table) { t.Col(&l.Note).Size(80) }

// Legacy is a model by shape but explicitly excluded.
//
//storm:ignore
type Legacy struct {
	st.Model
	Old string
}

// Tagged has no storm marker at all and is opted in by hand.
//
//storm:model
type Tagged struct {
	ID   int64
	Name string
}

// notAModel is unexported and unreachable from a bootstrap.
type notAModel struct {
	st.Model
	X string
}

func (n *notAModel) Schema(t *st.Table) { t.Col(&n.X).Size(1) }
