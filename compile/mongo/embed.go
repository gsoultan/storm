package mongo

import "github.com/gsoultan/storm/schema"

// Embed is the document form that stores a referring row inside the row it
// references (storm.Embed).
const Embed = "embed"

// EmbedOf is the reference t is embedded through, or nil when t has a
// collection of its own. The parent's documents hold t's rows as an array
// named t.Name.
func EmbedOf(t *schema.Table) *schema.Relation {
	for _, r := range t.Relations {
		if r.Owner && r.Document == Embed {
			return r
		}
	}
	return nil
}

// Embedded lists the tables embedded in parent, in s's order.
func Embedded(s *schema.Schema, parent *schema.Table) []*schema.Table {
	var out []*schema.Table
	for _, t := range s.Tables {
		if r := EmbedOf(t); r != nil && r.Target == parent.Name {
			out = append(out, t)
		}
	}
	return out
}
