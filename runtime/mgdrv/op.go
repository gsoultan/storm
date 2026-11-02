package mgdrv

// op is one step of a compiled command: write some fixed bytes, open or close
// a nested document, or bind an argument.
type op struct {
	kind uint8
	// raw is the fixed element bytes for opRaw, or the element header (type
	// byte and name) for opOpen.
	raw []byte
	// name is the element name an argument is bound under.
	name string
	// arg is the argument's index, from 0.
	arg int
}

const (
	opRaw uint8 = iota
	opOpen
	opClose
	opArg
)
