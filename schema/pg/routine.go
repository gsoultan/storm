package schemapg

import (
	"context"
	"strings"

	"github.com/gsoultan/storm/schema"
)

// Reading the SQL-bodied objects back out of the catalog.
//
// Every query here excludes objects owned by an EXTENSION. Installing
// btree_gist or pg_trgm — which storm's own DDL does, for exclusion
// constraints and trigram indexes — puts dozens of functions into the
// namespace. Without this filter storm introspects them, finds no matching
// declaration in the model, and emits a migration that DROPs the extension's
// functions out from under it.

func loadFunctions(ctx context.Context, c Conn, ns string, s *schema.Schema) error {
	rows, err := c.Query(ctx, `
		SELECT p.proname,
		       pg_get_function_arguments(p.oid),
		       pg_get_function_result(p.oid),
		       l.lanname,
		       p.provolatile::text,
		       p.proisstrict,
		       p.prosecdef,
		       COALESCE(p.prosrc, ''),
		       COALESCE(pg_get_functiondef(p.oid), '')
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		JOIN pg_language l ON l.oid = p.prolang
		WHERE n.nspname = $1
		  -- 'f' is a plain function. Aggregates ('a'), window functions ('w')
		  -- and procedures ('p') are deliberately out: storm has no way to
		  -- declare one, so introspecting them would mean diffing them against
		  -- a model that can never contain them — drift that never clears.
		  AND p.prokind = 'f'
		  AND NOT EXISTS (
		      SELECT 1 FROM pg_depend d
		      WHERE d.objid = p.oid AND d.deptype = 'e')
		ORDER BY p.proname, pg_get_function_arguments(p.oid)`, ns)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		f := &schema.Function{}
		var vol string
		var secDef bool
		if err := rows.Scan(&f.Name, &f.Args, &f.Returns, &f.Language,
			&vol, &f.Strict, &secDef, &f.Body, &f.Def); err != nil {
			return err
		}
		f.Volatility = volatility(vol)
		f.Security = "INVOKER"
		if secDef {
			f.Security = "DEFINER"
		}
		s.Functions = append(s.Functions, f)
	}
	return rows.Err()
}

// volatility maps pg_proc.provolatile onto the keyword that produced it.
func volatility(c string) string {
	switch c {
	case "i":
		return "IMMUTABLE"
	case "s":
		return "STABLE"
	default:
		return "VOLATILE"
	}
}

func loadViews(ctx context.Context, c Conn, ns string, s *schema.Schema) error {
	rows, err := c.Query(ctx, `
		SELECT cl.relname, pg_get_viewdef(cl.oid, true)
		FROM pg_class cl
		JOIN pg_namespace n ON n.oid = cl.relnamespace
		WHERE n.nspname = $1 AND cl.relkind = 'v'
		  AND NOT EXISTS (
		      SELECT 1 FROM pg_depend d
		      WHERE d.objid = cl.oid AND d.deptype = 'e')
		ORDER BY cl.relname`, ns)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		v := &schema.View{}
		if err := rows.Scan(&v.Name, &v.Def); err != nil {
			return err
		}
		// pg_get_viewdef indents and trails a semicolon; the declared form has
		// neither. Both sides of a diff come through here, so this only has to
		// be consistent, not minimal.
		v.Def = strings.TrimRight(strings.TrimSpace(v.Def), ";")
		s.Views = append(s.Views, v)
	}
	return rows.Err()
}

func loadTriggers(ctx context.Context, c Conn, ns string, s *schema.Schema) error {
	rows, err := c.Query(ctx, `
		SELECT t.tgname, cl.relname, pg_get_triggerdef(t.oid)
		FROM pg_trigger t
		JOIN pg_class cl ON cl.oid = t.tgrelid
		JOIN pg_namespace n ON n.oid = cl.relnamespace
		WHERE n.nspname = $1
		  -- tgisinternal is every trigger PostgreSQL made for itself: the ones
		  -- enforcing foreign keys, and the per-partition clones it creates
		  -- when a trigger is declared on a partitioned parent. Both would come
		  -- back as triggers no model declared.
		  AND NOT t.tgisinternal
		  AND NOT EXISTS (
		      SELECT 1 FROM pg_depend d
		      WHERE d.objid = t.oid AND d.deptype = 'e')
		ORDER BY cl.relname, t.tgname`, ns)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		tr := &schema.Trigger{}
		if err := rows.Scan(&tr.Name, &tr.Table, &tr.Def); err != nil {
			return err
		}
		tr.Def = stripNamespace(tr.Def, ns, tr.Table)
		s.Triggers = append(s.Triggers, tr)
	}
	return rows.Err()
}

// stripNamespace removes the schema qualification pg_get_triggerdef puts on the
// table — and ONLY on the table: the trigger's own name is bare and the
// function it executes is rendered against search_path, which Introspect has
// already set to this namespace.
//
// It has to go because the two sides of a diff are read from different
// namespaces by construction: the model from a scratch schema, the database
// from its real one. Left in, every trigger in the model reads
// `ON storm_normalize_4213.grants` and every trigger in the database reads
// `ON public.grants`, so all of them look changed and the plan drops and
// recreates the lot on every run.
func stripNamespace(def, ns, table string) string {
	for _, qualified := range []string{
		" ON " + ns + "." + table + " ",
		" ON " + quoteIdent(ns) + "." + quoteIdent(table) + " ",
		" ON " + ns + "." + quoteIdent(table) + " ",
		" ON " + quoteIdent(ns) + "." + table + " ",
	} {
		if i := strings.Index(def, qualified); i >= 0 {
			return def[:i] + " ON " + table + " " + def[i+len(qualified):]
		}
	}
	return def
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
