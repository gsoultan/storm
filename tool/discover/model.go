package tooldiscover

// Model is one model type found in the adopter's source.
//
// It carries where it was found as well as what it is, because every error
// this package raises is about a specific declaration in a specific file and
// "some model somewhere is unexported" is not an actionable sentence.
type Model struct {
	// ImportPath is the package the synthesized bootstrap must import.
	ImportPath string
	// PkgName is the package clause, which is the import's local name.
	PkgName string
	// TypeName is the struct's name.
	TypeName string
	// Pos is file:line, for errors.
	Pos string
	// Why records which rule matched, so `storm models` can explain itself
	// and a surprise inclusion is traceable to a reason.
	Why Reason
}

// Reason is the rule that identified a type as a model.
type Reason string

const (
	// EmbedsModel — the struct embeds storm.Model.
	EmbedsModel Reason = "embeds storm.Model"
	// HasSchema — the type has a Schema(*storm.Table) method. This is the rule
	// that catches a model with a natural key, which embeds nothing.
	HasSchema Reason = "has a Schema method"
	// HasPlans — the type has a Plans(*storm.Plans) method.
	HasPlans Reason = "has a Plans method"
	// HasProjections — the type has a Projections(*storm.Projections) method.
	HasProjections Reason = "has a Projections method"
	// Directive — the type carries //storm:model.
	Directive Reason = "//storm:model"
)
