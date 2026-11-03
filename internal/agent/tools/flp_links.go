package tools

import (
	"fmt"
	"net/url"
	"strings"
)

// flpLinks forms every Forever shop, product, category and join link the agent tools give out.
// The rule is owned by marketflow-api (internal/flp ForTenant and StoreCode) and is mirrored
// here unchanged, so a link from the chat matches the one on the tenant's /p page:
//   - the distributor ID is the stored one, else the slug when the slug is a distributor ID
//     (8 or more digits);
//   - the store is the stored code, else the Forever store for the tenant's country, else KEN;
//   - the language is the stored one, else en.
//
// Change the rule in marketflow-api first, then here.
type flpLinks struct {
	DistribID string
	StoreCode string
	Language  string
	Slug      string
}

const (
	flpShopBase    = "https://shop.foreverliving.com/retail/entry/Shop.do"
	flpRecruitBase = "https://join.foreverliving.com/marketing/joinnow/applicationForm.do"
	flpProfileBase = "https://marketflow.codevertexafrica.com/p/"
)

// flpStoreByCountry is marketflow-api's flp.StoreCodesByCountry.
var flpStoreByCountry = map[string]string{
	"KE": "KEN", "UG": "UGA", "TZ": "TZA", "NG": "NGA", "ZA": "ZAF", "GH": "GHA",
	"US": "USA", "GB": "GBR", "DE": "DEU", "FR": "FRA", "IN": "IND", "AE": "ARE",
}

// flpForTenant mirrors marketflow-api flp.ForTenant; ok is false without a distributor ID.
func flpForTenant(slug, distribID, storeCode, language, country string) (flpLinks, bool) {
	d := strings.TrimSpace(distribID)
	if d == "" && isDistribID(slug) {
		d = slug
	}
	if d == "" {
		return flpLinks{}, false
	}
	lang := strings.ToLower(strings.TrimSpace(language))
	if lang == "" {
		lang = "en"
	}
	return flpLinks{DistribID: d, StoreCode: flpStoreCode(storeCode, country), Language: lang, Slug: slug}, true
}

// flpStoreCode mirrors marketflow-api flp.StoreCode.
func flpStoreCode(explicit, country string) string {
	if s := strings.ToUpper(strings.TrimSpace(explicit)); s != "" {
		return s
	}
	if s, ok := flpStoreByCountry[strings.ToUpper(strings.TrimSpace(country))]; ok {
		return s
	}
	return "KEN"
}

// isDistribID mirrors marketflow-api flp.IsDistribID: 8 or more digits.
func isDistribID(s string) bool {
	if len(s) < 8 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (b flpLinks) shop(extra url.Values) string {
	p := url.Values{}
	for k, v := range extra {
		p[k] = v
	}
	p.Set("store", b.StoreCode)
	p.Set("language", b.Language)
	p.Set("distribID", b.DistribID)
	return flpShopBase + "?" + p.Encode()
}

// ShopURL is the distributor's store landing page.
func (b flpLinks) ShopURL() string { return b.shop(nil) }

// ProductURL links one product under the distributor's ID.
func (b flpLinks) ProductURL(itemCode string) string {
	return b.shop(url.Values{"itemCode": {itemCode}})
}

// CategoryURL links a product category under the distributor's ID.
func (b flpLinks) CategoryURL(category string) string {
	return b.shop(url.Values{"categoryName": {category}})
}

// RecruitURL is the distributor's join form.
func (b flpLinks) RecruitURL() string {
	p := url.Values{}
	p.Set("action", "display")
	p.Set("store", b.StoreCode)
	p.Set("distribID", b.DistribID)
	p.Set("menuoff", "n")
	p.Set("langID", b.Language)
	return flpRecruitBase + "?" + p.Encode()
}

// ProfileURL is the distributor's MarketFlow page.
func (b flpLinks) ProfileURL() string {
	slug := b.Slug
	if slug == "" {
		slug = b.DistribID
	}
	return flpProfileBase + url.PathEscape(slug)
}

// whatsAppURL is a wa.me link with only the digits of phone.
func whatsAppURL(phone, message string) string {
	var digits strings.Builder
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	u := fmt.Sprintf("https://wa.me/%s", digits.String())
	if message != "" {
		u += "?text=" + url.QueryEscape(message)
	}
	return u
}
