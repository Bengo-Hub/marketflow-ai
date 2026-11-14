package osrun

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

// Buying credits from WhatsApp (Sprint 17): when an action asked for on WhatsApp is short of AI
// credits, the reply offers one "Buy N credits (KES X)" button for the smallest covering bundle.
// Only a tap on it starts a purchase: subscriptions-api creates the platform-billed credit
// purchase and Treasury sends the M-Pesa prompt to the chat number. Nothing is ever charged
// without that tap and the PIN on the phone. When Treasury reports the payment, the action that
// was waiting for the credits resumes (Resume).

// BuyOffer is one Buy credits button waiting for a tap, and later the purchase it started.
type BuyOffer struct {
	Ref      string `json:"ref"`
	TenantID string `json:"tenant_id"`
	UserID   string `json:"user_id"`
	Sender   string `json:"sender"`
	// TenantSlug and PortalURL build the links in the messages after the payment.
	TenantSlug string  `json:"tenant_slug,omitempty"`
	PortalURL  string  `json:"portal_url,omitempty"`
	// RunID is the turn the shortfall came from, so a failure message about that turn can carry
	// the button ("" when there is none).
	RunID      string  `json:"run_id,omitempty"`
	BundleCode string  `json:"bundle_code"`
	Units      int     `json:"units"`
	PriceKES   float64 `json:"price_kes"`
	// Need is the credits the waiting action needs in all (0: any balance will do).
	Need int `json:"need,omitempty"`
	// Waiting is the approval the credits are for; nil when the person only has to send the
	// request again.
	Waiting *Approval `json:"waiting,omitempty"`
	// IntentID is Treasury's payment intent, once the tap started the purchase.
	IntentID string    `json:"intent_id,omitempty"`
	Created  time.Time `json:"created"`
}

func (c *Codes) buyKey(tenantID, userID string) string {
	return c.ns + ":os:wa-buy:" + tenantID + ":" + userID
}

func (c *Codes) topupKey(intentID string) string { return c.ns + ":os:wa-topup:" + intentID }

func newRef() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// TopupTTL is how long an offer and a started purchase are kept.
func (c *Codes) TopupTTL() time.Duration { return c.topupTTL }

// OfferBuy keeps a Buy credits offer for the user, replacing any other, and returns its ref (the
// button id carries it).
func (c *Codes) OfferBuy(ctx context.Context, o BuyOffer) (string, error) {
	if c == nil || c.rdb == nil {
		return "", errors.New("osrun: no redis")
	}
	if o.TenantID == "" || o.UserID == "" || o.Sender == "" || o.BundleCode == "" || o.PriceKES <= 0 {
		return "", errors.New("osrun: tenant, user, number, bundle and price are required")
	}
	ref, err := newRef()
	if err != nil {
		return "", err
	}
	o.Ref, o.IntentID, o.Created = ref, "", c.now().UTC()
	b, err := json.Marshal(o)
	if err != nil {
		return "", err
	}
	if err := c.rdb.Set(ctx, c.buyKey(o.TenantID, o.UserID), b, c.topupTTL).Err(); err != nil {
		return "", err
	}
	return ref, nil
}

// PendingBuy returns the user's Buy credits offer without taking it.
func (c *Codes) PendingBuy(ctx context.Context, tenantID, userID string) (*BuyOffer, bool) {
	if c == nil || c.rdb == nil {
		return nil, false
	}
	b, err := c.rdb.Get(ctx, c.buyKey(tenantID, userID)).Bytes()
	if err != nil {
		return nil, false
	}
	var o BuyOffer
	if json.Unmarshal(b, &o) != nil {
		return nil, false
	}
	return &o, true
}

// TakeBuy uses the offer a Buy credits tap names: single use, from the number it was sent to.
// A tap on an older offer is ErrStaleTap; no offer is ErrNoCode.
func (c *Codes) TakeBuy(ctx context.Context, tenantID, userID, sender, ref string) (*BuyOffer, error) {
	o, ok := c.PendingBuy(ctx, tenantID, userID)
	if !ok {
		return nil, ErrNoCode
	}
	if o.Ref != ref || (o.Sender != "" && sender != "" && o.Sender != sender) {
		return nil, ErrStaleTap
	}
	n, err := c.rdb.Del(ctx, c.buyKey(tenantID, userID)).Result()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, ErrNoCode // a second tap raced this one
	}
	return o, nil
}

// SaveTopup keeps a started purchase under its Treasury intent until the payment is reported.
func (c *Codes) SaveTopup(ctx context.Context, o BuyOffer) error {
	if c == nil || c.rdb == nil {
		return errors.New("osrun: no redis")
	}
	if o.IntentID == "" {
		return errors.New("osrun: intent id required")
	}
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	return c.rdb.Set(ctx, c.topupKey(o.IntentID), b, c.topupTTL).Err()
}

// Topup returns the purchase started for a Treasury intent; ok is false when none (not ours,
// expired or already resumed).
func (c *Codes) Topup(ctx context.Context, intentID string) (*BuyOffer, bool) {
	if c == nil || c.rdb == nil || intentID == "" {
		return nil, false
	}
	b, err := c.rdb.Get(ctx, c.topupKey(intentID)).Bytes()
	if err != nil {
		return nil, false
	}
	var o BuyOffer
	if json.Unmarshal(b, &o) != nil {
		return nil, false
	}
	return &o, true
}

// FinishTopup removes a purchase once its action resumed; only the caller that removed it may
// resume (a repeated payment event finds nothing).
func (c *Codes) FinishTopup(ctx context.Context, intentID string) bool {
	if c == nil || c.rdb == nil {
		return false
	}
	n, err := c.rdb.Del(ctx, c.topupKey(intentID)).Result()
	return err == nil && n == 1
}
