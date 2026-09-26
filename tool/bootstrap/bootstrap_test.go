package toolbootstrap

import (
	"strings"
	"testing"
)

// The hint names the package go could not find, not always storm's tool.
//
// The bootstrap for -dialect oracle blank-imports the database/sql driver, and
// an adopter without it in go.mod got go's own correct line — "no required
// module provides package github.com/sijms/go-ora/v2" — followed by storm's
// translation telling them to `go get github.com/gsoultan/storm/tool`, which
// they already had and which fixes nothing. Found by generating from the
// published v1.2.0 in a module that was not storm.
func TestTheHintNamesTheMissingPackage(t *testing.T) {
	const tool = "go get " + stormPath + "/tool"
	for _, c := range []struct {
		name, stderr, want, not string
	}{
		{
			name: "the driver a dialect needs",
			stderr: "model/.storm-bootstrap-1/main.go:10:2: no required module provides package " +
				"github.com/sijms/go-ora/v2; to add it:\n\tgo get github.com/sijms/go-ora/v2\n",
			want: "go get github.com/sijms/go-ora/v2",
			not:  tool,
		},
		{
			name: "a go.sum entry for the driver",
			stderr: "missing go.sum entry for module providing package github.com/sijms/go-ora/v2 " +
				"(imported by example.com/m/model/.storm-bootstrap-1); to add:\n\tgo get example.com/m/model/.storm-bootstrap-1\n",
			want: "go get github.com/sijms/go-ora/v2",
			not:  tool,
		},
		{
			name: "storm's own tool package",
			stderr: "no required module provides package " + stormPath + "/tool; to add it:\n\tgo get " +
				stormPath + "/tool\n",
			want: tool,
		},
		{
			name:   "go.mod out of date, which discovery causes",
			stderr: "go: updates to go.mod needed; to update it:\n\tgo mod tidy\n",
			want:   tool,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := missingToolDep(c.stderr)
			if !strings.Contains(got, c.want) {
				t.Errorf("hint does not say %q:\n%s", c.want, got)
			}
			if c.not != "" && strings.Contains(got, c.not) {
				t.Errorf("hint prescribes %q, which fixes nothing here:\n%s", c.not, got)
			}
		})
	}
	if got := missingToolDep("some other failure\n"); got != "" {
		t.Errorf("an unrelated failure was translated: %q", got)
	}
}
