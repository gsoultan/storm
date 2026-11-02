package tool

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/examples/blog/model"
)

// The quickstart's model does not port, and the refusal says why in terms an
// adopter can act on — beginning with the one ADR-0004 makes mandatory.
func TestPortableMongoRefusesTheBlogModelByName(t *testing.T) {
	s, err := storm.Build(model.All()...)
	if err != nil {
		t.Fatal(err)
	}
	err = portable("mongodb", s)
	if err == nil {
		t.Fatal("the blog model ports to MongoDB, apparently")
	}
	for _, want := range []string{"OnDocument(storm.Reference())",
		"union Feed", "named projections"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must contain %q:\n%v", want, err)
		}
	}
}
