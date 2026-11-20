// Package kbaudience keeps Forever Living (FLP) knowledge out of other tenants' answers.
//
// Platform knowledge base rows are shared, but the FLP sets (products, FAQ, business
// opportunity, compliance) belong to Forever distributors only. marketflow-api marks them with a
// source_kind starting "flp_" (or business_type "flp" in metadata). A tenant whose business type
// is not "flp", the Codevertex platform tenant included, never retrieves them.
package kbaudience

import "fmt"

// Clause returns a SQL condition, to AND into a knowledge_base_entries query, that drops FLP
// platform rows unless the tenant is an FLP tenant. alias is the entries table alias ("" for
// none) and tenantParam the placeholder holding the tenant UUID, such as "$2".
func Clause(alias, tenantParam string) string {
	col := func(c string) string {
		if alias == "" {
			return c
		}
		return alias + "." + c
	}
	return fmt.Sprintf(`(%[1]s <> 'platform'
		OR NOT (left(COALESCE(%[2]s->>'source_kind', ''), 4) = 'flp_' OR COALESCE(%[2]s->>'business_type', '') = 'flp')
		OR EXISTS (SELECT 1 FROM tenants flp_t WHERE flp_t.id = %[3]s AND lower(flp_t.business_type) = 'flp'))`,
		col("scope"), col("metadata"), tenantParam)
}
