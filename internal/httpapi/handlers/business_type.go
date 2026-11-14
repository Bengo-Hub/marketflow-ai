package handlers

import (
	"context"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-ai/internal/compliance"
)

// chatBusinessType is the business type a Marketing Manager chat answer is written and checked
// under: the tenant's own type, or "generic" when it is empty or unknown, never the keyword
// auto-detect (a business owner's question about coffee must not turn a pharmacy into a cafe).
func (h *OSHandler) chatBusinessType(ctx context.Context, tenantID uuid.UUID) string {
	return ownBusinessType(h.businessType(ctx, tenantID))
}

// ownBusinessType keeps a known type and turns empty or unknown values into "generic".
func ownBusinessType(bt string) string {
	if compliance.Known(bt) {
		return bt
	}
	return compliance.TypeGeneric
}
