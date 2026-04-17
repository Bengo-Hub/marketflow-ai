package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// GenerateLinks produces distribID-embedded product links, category links,
// recruit links, shop links, WhatsApp links, and profile URLs for the
// tenant. Lighter than get_fbo_profile — used when the agent already knows
// the tenant's distribID from an earlier tool call and just needs a
// specific link type.
type GenerateLinks struct {
	logger *zap.Logger
}

func NewGenerateLinks(logger *zap.Logger) *GenerateLinks {
	return &GenerateLinks{logger: logger.Named("tool.generate_links")}
}

func (*GenerateLinks) Name() string { return "generate_links" }
func (*GenerateLinks) Description() string {
	return "Generates FLP distribID-embedded links: product link (with itemCode), shop link, recruit link, category link, WhatsApp link, or profile URL. Provide the FBO's distribID, store code, and language (from get_fbo_profile) plus the link type and optional product code."
}
func (*GenerateLinks) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"distrib_id": map[string]any{"type": "string", "description": "FBO distribID (e.g. 254200047984)"},
			"store_code": map[string]any{"type": "string", "description": "FLP store code (e.g. KEN, USA, GBR)", "default": "KEN"},
			"language":   map[string]any{"type": "string", "description": "FLP language code (e.g. en, fr, sw)", "default": "en"},
			"link_type":  map[string]any{"type": "string", "description": "product | shop | recruit | category | whatsapp | profile"},
			"item_code":  map[string]any{"type": "string", "description": "Product item code (for link_type=product). E.g. '015', 'C9', '022'"},
			"category":   map[string]any{"type": "string", "description": "Category name (for link_type=category). E.g. 'Drinks', 'Skin Care'"},
			"slug":       map[string]any{"type": "string", "description": "Tenant slug (for link_type=profile)"},
			"phone":      map[string]any{"type": "string", "description": "WhatsApp phone digits (for link_type=whatsapp)"},
			"message":    map[string]any{"type": "string", "description": "Pre-filled WhatsApp message (optional)"},
		},
		"required": []string{"link_type"},
	}
}

func (t *GenerateLinks) Invoke(_ context.Context, _ uuid.UUID, argsJSON string) (string, error) {
	var args struct {
		DistribID string `json:"distrib_id"`
		StoreCode string `json:"store_code"`
		Language  string `json:"language"`
		LinkType  string `json:"link_type"`
		ItemCode  string `json:"item_code"`
		Category  string `json:"category"`
		Slug      string `json:"slug"`
		Phone     string `json:"phone"`
		Message   string `json:"message"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if args.StoreCode == "" {
		args.StoreCode = "KEN"
	}
	if args.Language == "" {
		args.Language = "en"
	}

	var link string
	switch args.LinkType {
	case "product":
		if args.ItemCode == "" {
			return `{"error":"item_code is required for product links"}`, nil
		}
		link = fmt.Sprintf("https://shop.foreverliving.com/retail/entry/Shop.do?store=%s&language=%s&itemCode=%s&distribID=%s",
			args.StoreCode, args.Language, args.ItemCode, args.DistribID)
	case "shop":
		link = fmt.Sprintf("https://shop.foreverliving.com/retail/entry/Shop.do?store=%s&language=%s&distribID=%s",
			args.StoreCode, args.Language, args.DistribID)
	case "recruit":
		link = fmt.Sprintf("https://join.foreverliving.com/marketing/joinnow/applicationForm.do?action=display&store=%s&distribID=%s&menuoff=n&langID=%s",
			args.StoreCode, args.DistribID, args.Language)
	case "category":
		if args.Category == "" {
			return `{"error":"category is required for category links"}`, nil
		}
		link = fmt.Sprintf("https://shop.foreverliving.com/retail/entry/Shop.do?store=%s&language=%s&categoryName=%s&distribID=%s",
			args.StoreCode, args.Language, args.Category, args.DistribID)
	case "whatsapp":
		phone := args.Phone
		if phone == "" {
			return `{"error":"phone is required for whatsapp links"}`, nil
		}
		msg := args.Message
		if msg == "" {
			msg = "Hi, I'm interested in your Forever Living products"
		}
		link = fmt.Sprintf("https://wa.me/%s?text=%s", phone, url.QueryEscape(msg))
	case "profile":
		slug := args.Slug
		if slug == "" {
			return `{"error":"slug is required for profile links"}`, nil
		}
		link = fmt.Sprintf("https://marketflow.codevertexitsolutions.com/p/%s", slug)
	default:
		return `{"error":"unknown link_type. Use: product, shop, recruit, category, whatsapp, or profile"}`, nil
	}

	result, _ := json.Marshal(map[string]string{
		"link_type": args.LinkType,
		"url":       link,
	})
	t.logger.Debug("generate_links", zap.String("type", args.LinkType), zap.String("url", link))
	return string(result), nil
}
