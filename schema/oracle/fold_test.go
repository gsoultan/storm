package oracle

// The case rule, which no other introspector storm has needs.
//
// Oracle stores an identifier as it was CREATED and folds an unquoted one UP
// on the way in. PostgreSQL folds DOWN, so its catalogue already reads the way
// a model does; SQL Server preserves what it was given. Oracle is the only one
// where the ordinary case is SHOUTING — and a model generated from it verbatim
// would declare Go fields from names nobody wrote.

import "testing"

func TestAnUnquotedNameIsLoweredAndAQuotedOneIsNot(t *testing.T) {
	for in, want := range map[string]string{
		// Shouting: this can only have come from an unquoted declaration.
		"USERS":      "users",
		"CREATED_AT": "created_at",
		"PK_USERS":   "pk_users",
		// Already lowercase: this came from a quoted one, which is what storm
		// itself writes, and it is already what somebody typed.
		"users":      "users",
		"created_at": "created_at",
		// MIXED case can only be quoted, and lowering it would rename a
		// column the model has to match exactly.
		"UserName": "UserName",
		"aB":       "aB",
		"":         "",
	} {
		if got := fold(in); got != want {
			t.Errorf("fold(%q) = %q, want %q", in, got, want)
		}
	}
}

// And the reverse, for a name going INTO a catalogue query: what the caller
// typed lowercase was almost certainly created unquoted, so it is stored upper.
func TestASchemaNameIsRaisedForTheLookup(t *testing.T) {
	for in, want := range map[string]string{
		"storm": "STORM",
		"STORM": "STORM",
		"Mixed": "Mixed", // quoted, so verbatim
	} {
		if got := fold2(in); got != want {
			t.Errorf("fold2(%q) = %q, want %q", in, got, want)
		}
	}
}

// An empty namespace is not the same query with a term removed: USER is the
// connected user, which is what an application's unqualified names resolve
// against.
func TestTheEmptyNamespaceMeansTheConnectedUser(t *testing.T) {
	pred, arg := owner("")
	if pred != "owner = USER" || arg != "" {
		t.Errorf("owner(\"\") = %q, %q", pred, arg)
	}
	pred, arg = owner("storm")
	if pred != "owner = :1" || arg != "STORM" {
		t.Errorf("owner(\"storm\") = %q, %q", pred, arg)
	}
}

// A default comes back parenthesised, and a model's does not.
func TestAStoredDefaultIsUnwrapped(t *testing.T) {
	for in, want := range map[string]string{
		"(1)":          "1",
		"((1))":        "1",
		"1":            "1",
		"(a) + (b)":    "(a) + (b)", // NOT the whole expression's parentheses
		"SYSTIMESTAMP": "SYSTIMESTAMP",
	} {
		if got := unwrap(in); got != want {
			t.Errorf("unwrap(%q) = %q, want %q", in, got, want)
		}
	}
}
