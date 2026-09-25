package schemapg

import (
	"context"
	"strings"

	"github.com/gsoultan/storm/schema"
)

// loadPartitions fills in both halves of partitioning: what a table is
// partitioned BY, and which parent a table is a partition OF.
//
// The second is what keeps storm from proposing destruction. A partition is an
// ordinary relation in pg_class, so a schema whose partitions are created by a
// scheduled job — one per month, the usual shape — presents storm with tables
// that exist in the database and in no model. Told apart, they are left alone;
// not told apart, the next diff drops the audit history.
func loadPartitions(ctx context.Context, c Conn, ns string, s *schema.Schema) error {
	if err := loadPartitionStrategies(ctx, c, ns, s); err != nil {
		return err
	}
	return loadPartitionBounds(ctx, c, ns, s)
}

func loadPartitionStrategies(ctx context.Context, c Conn, ns string, s *schema.Schema) error {
	rows, err := c.Query(ctx, `
		SELECT cl.relname,
		       CASE p.partstrat WHEN 'r' THEN 'RANGE'
		                        WHEN 'l' THEN 'LIST'
		                        WHEN 'h' THEN 'HASH' END,
		       pg_get_partkeydef(cl.oid)
		FROM pg_partitioned_table p
		JOIN pg_class cl ON cl.oid = p.partrelid
		JOIN pg_namespace n ON n.oid = cl.relnamespace
		WHERE n.nspname = $1
		ORDER BY cl.relname`, ns)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name, strat, keydef string
		if err := rows.Scan(&name, &strat, &keydef); err != nil {
			return err
		}
		t := s.Table(name)
		if t == nil {
			continue
		}
		// pg_get_partkeydef renders "RANGE (occurred_at)". The strategy is
		// already known, so only the key inside the parentheses is needed —
		// and it is taken as text rather than parsed because a partition key
		// may be an expression, which is not a column list at all.
		t.Partition = &schema.Partition{Strategy: strat, Columns: partKeyCols(keydef)}
	}
	return rows.Err()
}

func loadPartitionBounds(ctx context.Context, c Conn, ns string, s *schema.Schema) error {
	rows, err := c.Query(ctx, `
		SELECT child.relname, parent.relname,
		       COALESCE(pg_get_expr(child.relpartbound, child.oid), '')
		FROM pg_inherits i
		JOIN pg_class child ON child.oid = i.inhrelid
		JOIN pg_class parent ON parent.oid = i.inhparent
		JOIN pg_namespace n ON n.oid = child.relnamespace
		WHERE n.nspname = $1 AND child.relispartition
		ORDER BY child.relname`, ns)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var child, parent, bound string
		if err := rows.Scan(&child, &parent, &bound); err != nil {
			return err
		}
		if t := s.Table(child); t != nil {
			t.PartitionOf = parent
			t.Bound = bound
		}
	}
	return rows.Err()
}

// partKeyCols pulls the key out of "RANGE (occurred_at)" or
// "LIST (tenant_id, kind)". Depth-aware because an expression key brings its
// own parentheses and commas: "RANGE (((a + b)))".
func partKeyCols(def string) []string {
	i := strings.IndexByte(def, '(')
	if i < 0 || def[len(def)-1] != ')' {
		return nil
	}
	inner := def[i+1 : len(def)-1]
	var (
		out   []string
		cur   []byte
		depth int
	)
	flush := func() {
		if t := strings.TrimSpace(string(cur)); t != "" {
			out = append(out, t)
		}
		cur = cur[:0]
	}
	for j := 0; j < len(inner); j++ {
		switch ch := inner[j]; ch {
		case '(':
			depth++
			cur = append(cur, ch)
		case ')':
			depth--
			cur = append(cur, ch)
		case ',':
			if depth == 0 {
				flush()
				continue
			}
			cur = append(cur, ch)
		default:
			cur = append(cur, ch)
		}
	}
	flush()
	return out
}
