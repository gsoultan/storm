// Package aliasrowx deliberately declares a name that differs from its
// directory (aliasrow). Real adopters do this — anubis keeps package
// authzrquery in a directory named rquery — and the raw-scanner emitter must
// qualify row types by the DECLARED name while importing the directory path.
package aliasrowx

// Row is a raw-query row type for codegen's alias regression test.
type Row struct {
	Name string
}
