package tools

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// GetProduct looks up a product KB entry by product code (e.g. "015" for Aloe Vera Gel).
type GetProduct struct {
	db     *sql.DB
	logger *zap.Logger
}

func NewGetProduct(db *sql.DB, logger *zap.Logger) *GetProduct {
	return &GetProduct{db: db, logger: logger.Named("tool.get_product")}
}

func (*GetProduct) Name() string { return "get_product" }
func (*GetProduct) Description() string {
	return "Look up a specific Forever Living product by its product code (e.g. '015', '022', 'C9'). Returns title, category, and full chunk text. Use when the user references a product code or exact product name."
}
func (*GetProduct) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"code": map[string]any{"type": "string", "description": "Product code or exact product name"},
		},
		"required": []string{"code"},
	}
}

func (t *GetProduct) Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error) {
	var args struct{ Code string `json:"code"` }
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	row := t.db.QueryRowContext(ctx, `
		SELECT title, category, content
		FROM knowledge_base_entries
		WHERE tenant_id = $1
		  AND category = 'product'
		  AND (metadata->>'product_code' ILIKE $2 OR title ILIKE $2)
		LIMIT 1`, tenantID, "%"+args.Code+"%")
	var title, category, content string
	if err := row.Scan(&title, &category, &content); err != nil {
		if err == sql.ErrNoRows {
			return `{"found":false}`, nil
		}
		return "", err
	}
	out, _ := json.Marshal(map[string]any{
		"found": true, "title": title, "category": category, "content": content,
	})
	return string(out), nil
}

// GetFAQ returns FAQ entries filtered by keyword.
type GetFAQ struct {
	db     *sql.DB
	logger *zap.Logger
}

func NewGetFAQ(db *sql.DB, logger *zap.Logger) *GetFAQ {
	return &GetFAQ{db: db, logger: logger.Named("tool.get_faq")}
}

func (*GetFAQ) Name() string { return "get_faq" }
func (*GetFAQ) Description() string {
	return "Fetch FAQ entries from the knowledge base filtered by a keyword (title ILIKE). Use for common quick-answer questions."
}
func (*GetFAQ) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"keyword": map[string]any{"type": "string", "description": "Keyword appearing in the FAQ title"},
		},
		"required": []string{"keyword"},
	}
}

func (t *GetFAQ) Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error) {
	var args struct{ Keyword string `json:"keyword"` }
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	rows, err := t.db.QueryContext(ctx, `
		SELECT title, content
		FROM knowledge_base_entries
		WHERE tenant_id = $1 AND category = 'faq' AND title ILIKE $2
		LIMIT 5`, tenantID, "%"+args.Keyword+"%")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	type faq struct{ Title, Content string }
	out := make([]faq, 0, 5)
	for rows.Next() {
		var f faq
		if err := rows.Scan(&f.Title, &f.Content); err == nil {
			out = append(out, f)
		}
	}
	b, _ := json.Marshal(map[string]any{"results": out, "count": len(out)})
	return string(b), nil
}

// GetComplianceRules returns the current FLP compliance ruleset so the model can self-check.
type GetComplianceRules struct{}

func NewGetComplianceRules() *GetComplianceRules { return &GetComplianceRules{} }

func (*GetComplianceRules) Name() string { return "get_compliance_rules" }
func (*GetComplianceRules) Description() string {
	return "Returns the FLP / FDA compliance rules and banned phrases that the final answer MUST NOT violate. Call this once when drafting sensitive (medical, income-claim, regulatory) content."
}
func (*GetComplianceRules) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (*GetComplianceRules) Invoke(ctx context.Context, _ uuid.UUID, _ string) (string, error) {
	rules := map[string]any{
		"medical": map[string]any{
			"banned_verbs":     []string{"cures", "treats", "heals", "prevents disease", "diagnoses"},
			"approved_verbs":   []string{"supports", "promotes", "helps maintain", "contributes to"},
			"always_advise":    "Always recommend consulting a healthcare professional for medical questions.",
		},
		"income": map[string]any{
			"never_guarantee": "Never guarantee a specific income amount. Always say 'results vary'.",
			"require_disclaimer": "Any mention of earnings must include an income disclaimer.",
		},
		"product_claims": map[string]any{
			"require_citation": "Cite product code (e.g. #015) and CC value when making a recommendation.",
			"aloe_facts":       "Up to 99.7% pure stabilized aloe vera gel, IASC certified, USDA/EU organic farms.",
		},
	}
	b, _ := json.MarshalIndent(rules, "", "  ")
	return string(b), nil
}

