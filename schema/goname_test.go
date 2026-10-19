package schema

import "testing"

// ColumnName is what a model field maps to, and what a raw row type's fields
// are matched against. The acronym cases are the ones the two sides once
// disagreed on.
func TestColumnName(t *testing.T) {
	for field, want := range map[string]string{
		"ID":         "id",
		"OrgID":      "org_id",
		"CPUPercent": "cpu_percent",
		"HTTPServer": "http_server",
		"CpuPercent": "cpu_percent",
		"Total2":     "total2",
		"P95Latency": "p95_latency",
	} {
		if got := ColumnName(field); got != want {
			t.Errorf("ColumnName(%q) = %q, want %q", field, got, want)
		}
	}
}
