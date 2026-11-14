package osrun

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// WhatsApp reply buttons and list rows (Sprint 17, owner 2026-11-14). Approvals are taps instead
// of typed codes, but the typed "YES <code>" keeps working. A button's id carries what it does,
// the run and, for an approval, the code, so a tap goes through the same code check as a typed
// YES (single use, the code TTL, bound to the user and number, CodeAttempts wrong tries).
//
// Ids are "mf:<kind>:<run>[:<code>]", or "mf:f:<field>:<row>" for a list row answering a missing
// TaskSpec field. Meta returns the id unchanged on the webhook (interactive.button_reply.id or
// list_reply.id); notifications-api passes it on as reply_id.

// Reply kinds.
const (
	ReplyYes     = "yes"     // approve: the quote's Approve, the storyboard's Make the video
	ReplyCheaper = "cheaper" // the quote's Cheaper option
	ReplyCancel  = "cancel"  // the quote's Cancel
	ReplyChange  = "change"  // the storyboard's Change
	ReplyDiscard = "discard" // the storyboard's Discard
	ReplyField   = "f"       // a list row answering a missing field
)

// replyPrefix marks our ids; anything else (a template quick reply such as "stop") is not ours.
const replyPrefix = "mf:"

// Reply is a parsed button or list-row id.
type Reply struct {
	Kind  string
	RunID string
	Code  string
	// Field and Row are set for ReplyField.
	Field string
	Row   int
}

// ReplyID builds a button id. code is only used by ReplyYes.
func ReplyID(kind, runID, code string) string {
	id := replyPrefix + kind + ":" + runID
	if kind == ReplyYes && code != "" {
		id += ":" + code
	}
	return id
}

// FieldRowID builds the id of a list row answering field.
func FieldRowID(field string, row int) string {
	return replyPrefix + ReplyField + ":" + field + ":" + strconv.Itoa(row)
}

// ParseReply reads a reply id. ok is false for an id that is not ours or is malformed.
func ParseReply(id string) (Reply, bool) {
	id = strings.TrimSpace(id)
	if !strings.HasPrefix(id, replyPrefix) || len(id) > 256 {
		return Reply{}, false
	}
	parts := strings.Split(strings.TrimPrefix(id, replyPrefix), ":")
	switch parts[0] {
	case ReplyField:
		if len(parts) != 3 || parts[1] == "" {
			return Reply{}, false
		}
		n, err := strconv.Atoi(parts[2])
		if err != nil || n < 0 {
			return Reply{}, false
		}
		return Reply{Kind: ReplyField, Field: parts[1], Row: n}, true
	case ReplyYes:
		if len(parts) != 3 || parts[1] == "" || len(parts[2]) != 4 {
			return Reply{}, false
		}
		if _, err := strconv.Atoi(parts[2]); err != nil {
			return Reply{}, false
		}
		return Reply{Kind: ReplyYes, RunID: parts[1], Code: parts[2]}, true
	case ReplyCheaper, ReplyCancel, ReplyChange, ReplyDiscard:
		if len(parts) != 2 || parts[1] == "" {
			return Reply{}, false
		}
		return Reply{Kind: parts[0], RunID: parts[1]}, true
	}
	return Reply{}, false
}

// FieldOption is one list row for a missing field: Title is shown (24 characters at most) and
// Value is what the answer adds to the request.
type FieldOption struct {
	Title       string
	Description string
	Value       string
}

// fieldOptions are the fields a list can answer; the others (the offering, the audience, a
// link) need the owner's own words and stay a typed question.
var fieldOptions = map[string][]FieldOption{
	"objective": {
		{Title: "Sales", Description: "People buy from the ad", Value: "objective: sales"},
		{Title: "Leads", Description: "People leave their contacts", Value: "objective: leads"},
		{Title: "WhatsApp messages", Description: "People message us on WhatsApp", Value: "objective: WhatsApp messages"},
		{Title: "Website visits", Description: "People open our website", Value: "objective: website visits"},
		{Title: "Awareness", Description: "More people know us", Value: "objective: awareness"},
	},
	"platforms": {
		{Title: "Facebook + Instagram", Value: "platforms: Facebook and Instagram"},
		{Title: "Facebook", Value: "platforms: Facebook"},
		{Title: "Instagram", Value: "platforms: Instagram"},
		{Title: "TikTok", Value: "platforms: TikTok"},
		{Title: "WhatsApp", Value: "platforms: WhatsApp"},
		{Title: "YouTube", Value: "platforms: YouTube"},
		{Title: "All of them", Value: "platforms: Facebook, Instagram, TikTok, WhatsApp and YouTube"},
	},
	"budget": {
		{Title: "KES 1,000", Value: "budget: KES 1,000"},
		{Title: "KES 2,500", Value: "budget: KES 2,500"},
		{Title: "KES 5,000", Value: "budget: KES 5,000"},
		{Title: "KES 10,000", Value: "budget: KES 10,000"},
		{Title: "KES 20,000", Value: "budget: KES 20,000"},
		{Title: "KES 50,000", Value: "budget: KES 50,000"},
	},
	"period": {
		{Title: "3 days", Value: "for 3 days"},
		{Title: "1 week", Value: "for 1 week"},
		{Title: "2 weeks", Value: "for 2 weeks"},
		{Title: "1 month", Value: "for 1 month"},
	},
	"language": {
		{Title: "English", Value: "language: English"},
		{Title: "Swahili", Value: "language: Swahili"},
		{Title: "Sheng", Value: "language: Sheng"},
	},
}

// FieldOptions returns the list rows for a missing field, or nil when the field needs typed words.
func FieldOptions(field string) []FieldOption {
	return fieldOptions[field]
}

// FieldAnswer is the text a tapped row adds to the request.
func FieldAnswer(field string, row int) (string, bool) {
	opts := fieldOptions[field]
	if row < 0 || row >= len(opts) {
		return "", false
	}
	return opts[row].Value, true
}

// Clarify is a request waiting for one missing field, kept so a tapped list row can complete it.
type Clarify struct {
	Text    string    `json:"text"`
	Field   string    `json:"field"`
	Sender  string    `json:"sender,omitempty"`
	Created time.Time `json:"created"`
}

func (c *Codes) clarifyKey(tenantID, userID string) string {
	return c.ns + ":os:wa-clarify:" + tenantID + ":" + userID
}

// SaveClarify keeps the request text that is waiting for field, for the code TTL. A newer
// question replaces it.
func (c *Codes) SaveClarify(ctx context.Context, tenantID, userID string, cl Clarify) error {
	if c == nil || c.rdb == nil {
		return errors.New("osrun: no redis")
	}
	if tenantID == "" || userID == "" || cl.Text == "" || cl.Field == "" {
		return errors.New("osrun: tenant, user, text and field are required")
	}
	cl.Created = c.now().UTC()
	b, err := json.Marshal(cl)
	if err != nil {
		return err
	}
	return c.rdb.Set(ctx, c.clarifyKey(tenantID, userID), b, c.ttl).Err()
}

// TakeClarify returns and removes the waiting request, when it is for field and was sent to this
// number. ok is false when nothing waits (it expired, or another answer took it).
func (c *Codes) TakeClarify(ctx context.Context, tenantID, userID, sender, field string) (*Clarify, bool) {
	if c == nil || c.rdb == nil {
		return nil, false
	}
	b, err := c.rdb.GetDel(ctx, c.clarifyKey(tenantID, userID)).Bytes()
	if err != nil {
		return nil, false
	}
	var cl Clarify
	if json.Unmarshal(b, &cl) != nil || cl.Field != field || (cl.Sender != "" && sender != "" && cl.Sender != sender) {
		return nil, false
	}
	return &cl, true
}

// ClarifyTenant returns which of the tenants has a request waiting for this user, for routing a
// tapped list row from someone with several businesses whose pick has expired.
func (c *Codes) ClarifyTenant(ctx context.Context, tenantIDs []string, userID string) (string, bool) {
	if c == nil || c.rdb == nil || userID == "" {
		return "", false
	}
	for _, tid := range tenantIDs {
		if n, err := c.rdb.Exists(ctx, c.clarifyKey(tid, userID)).Result(); err == nil && n == 1 {
			return tid, true
		}
	}
	return "", false
}

// PendingApproval returns the user's pending approval without using it or counting a try, so a
// Cheaper option or Change tap can show the same Approve button again.
func (c *Codes) PendingApproval(ctx context.Context, tenantID, userID string) (*Approval, bool) {
	if c == nil || c.rdb == nil {
		return nil, false
	}
	b, err := c.rdb.Get(ctx, c.key(tenantID, userID)).Bytes()
	if errors.Is(err, redis.Nil) || err != nil {
		return nil, false
	}
	var a Approval
	if json.Unmarshal(b, &a) != nil {
		return nil, false
	}
	return &a, true
}
