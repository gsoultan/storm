package msdrv

import (
	"errors"
	"net"
	"strings"
	"testing"
)

// The pure parts, which the live tests reach only incidentally: the JSON key
// list a batch loader binds, the names pulled out of a server error, and the
// row arena.

// A bound key list is ONE parameter, so the statement's text does not depend on
// how many keys the caller passed (ADR-0010). A uuid goes as its canonical text
// here rather than MySQL's hex, because the column is UNIQUEIDENTIFIER and
// OPENJSON converts the text directly — which leaves no expression wrapped
// around the indexed column.
func TestJSONKeyLists(t *testing.T) {
	if got, ok := jsonList([]int64{1, 2, 3}); !ok || got != "[1,2,3]" {
		t.Errorf("int64 list = %q", got)
	}
	if got, ok := jsonList([]int32{4, 5}); !ok || got != "[4,5]" {
		t.Errorf("int32 list = %q", got)
	}
	u := [16]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef,
		0xfe, 0xdc, 0xba, 0x98, 0x76, 0x54, 0x32, 0x10}
	got, ok := jsonList([][16]byte{u})
	if !ok || got != `["01234567-89ab-cdef-fedc-ba9876543210"]` {
		t.Errorf("uuid list = %q", got)
	}
	// A control character left unescaped makes OPENJSON refuse the WHOLE
	// document, so one bad key loses every row the loader was fetching.
	got, _ = jsonList([]string{"a\"b\\c\nd\te"})
	for _, esc := range []string{`\"`, `\\`, `\n`, `\t`} {
		if !strings.Contains(got, esc) {
			t.Errorf("escape %s missing: %s", esc, got)
		}
	}
	// A control character below 0x20 has to be escaped as \uXXXX; there is no
	// shorter form and an unescaped one is not valid JSON.
	ctrl, _ := jsonList([]string{string([]byte{1})})
	if want := "[\"" + "\\u0001" + "\"]"; ctrl != want {
		t.Errorf("control character: %s, want %s", ctrl, want)
	}
	// A binary key travels as base64, which is what OPENJSON's varbinary
	// conversion reads.
	if got, _ := jsonList([][]byte{{0xde, 0xad, 0xbe, 0xef}}); got != `["3q2+7w=="]` {
		t.Errorf("binary list = %q", got)
	}
	// Not a list.
	if _, ok := jsonList("plain"); ok {
		t.Error("a string was taken for a list")
	}
	if _, ok := jsonList(int64(1)); ok {
		t.Error("a scalar was taken for a list")
	}
}

// A declaration and its TYPE_INFO must agree: the server binds by NAME and
// types by this string, so a value sent as a bigint against a declaration of
// nvarchar is a conversion error naming a parameter the caller never wrote.
func TestDeclarationTypes(t *testing.T) {
	got, err := declare([]any{int64(1), "x", []byte{1}, [16]byte{}, true})
	if err != nil {
		t.Fatal(err)
	}
	want := "@p1 bigint,@p2 nvarchar(4000),@p3 varbinary(max)," +
		"@p4 uniqueidentifier,@p5 bit"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	// A key list is one nvarchar(max) document.
	if got, _ := declare([]any{[]int64{1, 2}}); got != "@p1 nvarchar(max)" {
		t.Errorf("a key list declared as %s", got)
	}
	// Past nvarchar(4000) the MAX form is needed, or the value is truncated on
	// the way in.
	if got, _ := declare([]any{strings.Repeat("x", 5000)}); got != "@p1 nvarchar(max)" {
		t.Errorf("a long string declared as %s", got)
	}
	if _, err := declare([]any{struct{}{}}); err == nil {
		t.Error("a value with no binding was declared anyway")
	}
}

// The names, and ONLY the names. SQL Server's message ends "The duplicate key
// value is (alice@example.com)", which is a row's data in anything that logs
// the error.
func TestErrorNamesCarryNoValue(t *testing.T) {
	for _, c := range []struct {
		number            int32
		msg               string
		constraint, table string
	}{
		{2627, "Violation of UNIQUE KEY constraint 'uq_users_email'. Cannot insert " +
			"duplicate key in object 'dbo.users'. The duplicate key value is (ada@x.com).",
			"uq_users_email", "dbo.users"},
		{2601, "Cannot insert duplicate key row in object 'dbo.users' with unique " +
			"index 'uq_users_email'. The duplicate key value is (ada@x.com).",
			"uq_users_email", "dbo.users"},
		{547, `The INSERT statement conflicted with the FOREIGN KEY constraint ` +
			`"fk_members_org". The conflict occurred in database "storm", table "dbo.orgs".`,
			"fk_members_org", "storm"},
		{515, "Cannot insert the value NULL into column 'email', table 'db.dbo.users'.",
			"email", "db.dbo.users"},
	} {
		gotC, gotT := namesIn(c.number, c.msg)
		if gotC != c.constraint || gotT != c.table {
			t.Errorf("%d: got %q/%q, want %q/%q", c.number, gotC, gotT, c.constraint, c.table)
		}
	}
	// An error's own text never mentions the value.
	e := &Error{Number: 2627, Class: 14, State: 1,
		Constraint: "uq_users_email", Table: "dbo.users",
		msg: "The duplicate key value is (ada@x.com)."}
	if strings.Contains(e.Error(), "ada@x.com") {
		t.Errorf("the error carries a value: %s", e.Error())
	}
	if !strings.Contains(e.Error(), "uq_users_email") {
		t.Errorf("the error does not name the constraint: %s", e.Error())
	}
	// Reachable deliberately, for a debugger, and documented as unsafe to log.
	if !strings.Contains(e.ServerMessage(), "ada@x.com") {
		t.Error("ServerMessage dropped the server's text")
	}
	// A number with no shape has no names rather than a wrong guess.
	if c, tb := namesIn(9999, "something else entirely"); c != "" || tb != "" {
		t.Errorf("an unknown error yielded %q/%q", c, tb)
	}
}

// 547 is ONE number for two conditions, which both other targets split. The
// message says which, and the 409 you return for a missing reference is not the
// 422 you return for a value the table refuses.
func TestForeignKeyAndCheckAreToldApart(t *testing.T) {
	fk := &Error{Number: 547, Constraint: "fk_x", kindHint: "FOREIGN KEY"}
	ck := &Error{Number: 547, Constraint: "ck_x", kindHint: "CHECK"}
	if got := classify(fk).Error(); !strings.Contains(got, "foreign key") {
		t.Errorf("a foreign key violation classified as %s", got)
	}
	if got := classify(ck).Error(); !strings.Contains(got, "check") {
		t.Errorf("a check violation classified as %s", got)
	}
	// An error storm has no opinion about is returned UNCHANGED, because those
	// are the ones worth reading verbatim.
	other := &Error{Number: 4060}
	if classify(other) != error(other) {
		t.Error("an unrecognised error was wrapped")
	}
	if classify(nil) != nil {
		t.Error("classify(nil)")
	}
}

// The arena hands out stable slices and reuses its buffer between rows.
func TestArena(t *testing.T) {
	var a arena
	first := a.take(4)
	copy(first, []byte{1, 2, 3, 4})
	big := a.take(4096)
	if len(big) != 4096 {
		t.Fatalf("take(4096) gave %d", len(big))
	}
	a.reset()
	again := a.take(4)
	if len(again) != 4 || cap(again) != 4 {
		t.Errorf("reset did not reuse the buffer: len %d cap %d", len(again), cap(again))
	}
}

// Identifier quoting is the same rule compile/mssql uses, because a driver that
// quoted differently would be a second answer to a settled question.
func TestIdentBracket(t *testing.T) {
	if got := identBracket("order"); got != "[order]" {
		t.Errorf("got %s", got)
	}
	if got := identBracket("we]rd"); got != "[we]]rd]" {
		t.Errorf("got %s", got)
	}
}

// UCS-2 round trips, surrogate pairs included — every string in this protocol
// is UTF-16LE, and every LENGTH that describes one is a character count rather
// than a byte count. Reading one as the other half-reads every message and
// desynchronises the stream several tokens later.
func TestUCS2(t *testing.T) {
	for _, s := range []string{"", "hi", "hello there", "storm"} {
		x := &conn{wbuf: make([]byte, 4096), size: 4096, c: discard{}}
		x.begin(1)
		if err := x.writeUCS2(s); err != nil {
			t.Fatal(err)
		}
		b := x.wbuf[headerSize:x.wpos]
		if got := ucs2ToString(b); got != s {
			t.Errorf("%q round-tripped as %q", s, got)
		}
		if ucs2Len(s) != len(b) {
			t.Errorf("%q: ucs2Len says %d, wrote %d", s, ucs2Len(s), len(b))
		}
	}
	// Beyond the BMP, where one rune is two code units and the character count
	// is therefore not the rune count.
	wide := "a\U0001F329b"
	x := &conn{wbuf: make([]byte, 4096), size: 4096, c: discard{}}
	x.begin(1)
	if err := x.writeUCS2(wide); err != nil {
		t.Fatal(err)
	}
	b := x.wbuf[headerSize:x.wpos]
	if got := ucs2ToString(b); got != wide {
		t.Errorf("%q round-tripped as %q", wide, got)
	}
	if len(b) != 8 {
		t.Errorf("a surrogate pair took %d bytes, want 8", len(b))
	}
}

// The password obfuscation, which is a nibble swap and an XOR and is not a
// cipher. Asserted so that a change to it is deliberate: the server reverses
// exactly this, and a "better" version does not log in.
func TestPasswordObfuscation(t *testing.T) {
	x := &conn{wbuf: make([]byte, 4096), size: 4096, c: discard{}}
	x.begin(1)
	if err := x.writePassword("a"); err != nil {
		t.Fatal(err)
	}
	got := x.wbuf[headerSize:x.wpos]
	// 'a' is 0x61 0x00 as UTF-16LE; swap the nibbles and XOR with 0xA5.
	lo := byte(0x61)
	want := []byte{((lo << 4) | (lo >> 4)) ^ 0xA5, 0x00 ^ 0xA5}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got % x, want % x", got, want)
	}
}

// discard is a net.Conn that swallows writes, so the framing can be exercised
// without a socket.
type discard struct{ net.Conn }

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// ServerState duplicates an exported field so a package that must tell one
// RAISERROR from another can assert on an interface instead of importing this
// one. migrate does exactly that for the migration lock, which is why removing
// the "redundant" accessor would break a build nothing here compiles.
func TestServerStateIsReachableThroughAnInterface(t *testing.T) {
	var err error = &Error{Number: 50000, Class: 16, State: 77}
	var s interface{ ServerState() uint8 }
	if !errors.As(err, &s) {
		t.Fatal("*Error does not satisfy interface{ ServerState() uint8 }")
	}
	if got := s.ServerState(); got != 77 {
		t.Errorf("ServerState() = %d, want 77", got)
	}
}
