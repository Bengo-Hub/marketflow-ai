package osrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/bengobox/marketflow-ai/internal/promptopt"
	"github.com/bengobox/marketflow-ai/internal/registry"
	"github.com/bengobox/marketflow-ai/internal/team"
)

// Channels a turn came in on.
const (
	ChannelPortal   = "portal"
	ChannelWhatsApp = "whatsapp"
)

// Run is one quoted or approved media turn as the owner saw it.
type Run struct {
	RunID    string `json:"run_id"`
	TenantID string `json:"tenant_id"`
	// TenantSlug and PortalURL build the links in WhatsApp messages.
	TenantSlug string `json:"tenant_slug,omitempty"`
	PortalURL  string `json:"portal_url,omitempty"`
	// UserID is the auth-api user who asked; Sender is their verified WhatsApp number.
	UserID  string `json:"user_id,omitempty"`
	Sender  string `json:"sender,omitempty"`
	Channel string `json:"channel"`
	// Text, Tier and Platforms are the request as asked, so approving runs the same turn.
	Text      string   `json:"text"`
	Tier      string   `json:"tier,omitempty"`
	Platforms []string `json:"platforms,omitempty"`

	Playbook  string                 `json:"playbook,omitempty"`
	Entities  promptopt.Entities     `json:"entities"`
	Plan      *registry.CreativePlan `json:"plan,omitempty"`
	Steps     []team.Step            `json:"steps,omitempty"`
	Shortlist []string               `json:"shortlist,omitempty"`
	// Approved is true once the owner approved the plan and the team wrote the drafts.
	Approved bool `json:"approved"`
	// ApprovedRunID is, on a quote, the approved turn its WhatsApp YES produced; a retried
	// approval reuses it instead of running and charging the turn again.
	ApprovedRunID string `json:"approved_run_id,omitempty"`
	// BriefID and VersionID are the Studio brief and video ad made from this run.
	BriefID   string    `json:"brief_id,omitempty"`
	VersionID string    `json:"version_id,omitempty"`
	Created   time.Time `json:"created"`
}

// ErrRunNotFound is a run that expired, never existed or belongs to another tenant.
var ErrRunNotFound = errors.New("osrun: run not found")

// Store keeps runs in Redis under "<namespace>:os:run:<run_id>".
type Store struct {
	rdb *redis.Client
	ns  string
	ttl time.Duration
	now func() time.Time
}

// NewStore builds a store; a zero ttl uses the default.
func NewStore(rdb *redis.Client, namespace string, ttl time.Duration) *Store {
	if ttl <= 0 {
		ttl = DefaultConfig().RunTTL
	}
	return &Store{rdb: rdb, ns: namespace, ttl: ttl, now: time.Now}
}

func (s *Store) key(runID string) string { return s.ns + ":os:run:" + runID }

// Save stores a new run for the store's TTL.
func (s *Store) Save(ctx context.Context, run *Run) error {
	if s == nil || s.rdb == nil {
		return errors.New("osrun: no redis")
	}
	if run.RunID == "" || run.TenantID == "" {
		return errors.New("osrun: run_id and tenant_id are required")
	}
	if run.Created.IsZero() {
		run.Created = s.now().UTC()
	}
	b, err := json.Marshal(run)
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, s.key(run.RunID), b, s.ttl).Err()
}

// Update rewrites a stored run and keeps the time it has left.
func (s *Store) Update(ctx context.Context, run *Run) error {
	if s == nil || s.rdb == nil {
		return errors.New("osrun: no redis")
	}
	b, err := json.Marshal(run)
	if err != nil {
		return err
	}
	ok, err := s.rdb.SetArgs(ctx, s.key(run.RunID), b, redis.SetArgs{KeepTTL: true, Mode: "XX"}).Result()
	if errors.Is(err, redis.Nil) || (err == nil && ok != "OK") {
		return ErrRunNotFound
	}
	return err
}

// unlockScript deletes a lock only while it still holds this holder's token.
var unlockScript = redis.NewScript(`if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) end return 0`)

// Lock takes the single-flight lock "<namespace>:os:lock:<name>" for ttl, so one make-media (or
// one render start) runs at a time for a run or a version. ok is false while another holder has
// it. release gives it back; a holder that dies loses it after ttl.
func (s *Store) Lock(ctx context.Context, name string, ttl time.Duration) (release func(), ok bool, err error) {
	if s == nil || s.rdb == nil {
		return func() {}, false, errors.New("osrun: no redis")
	}
	if ttl <= 0 {
		ttl = DefaultConfig().MediaTimeout
	}
	token, err := fourDigits()
	if err != nil {
		return func() {}, false, err
	}
	token = fmt.Sprintf("%s-%d", token, s.now().UnixNano())
	key := s.ns + ":os:lock:" + name
	ok, err = s.rdb.SetNX(ctx, key, token, ttl).Result()
	if err != nil || !ok {
		return func() {}, false, err
	}
	return func() { unlockScript.Run(context.WithoutCancel(ctx), s.rdb, []string{key}, token) }, true, nil
}

// Get returns the tenant's run. Another tenant's run is not found.
func (s *Store) Get(ctx context.Context, tenantID, runID string) (*Run, error) {
	if s == nil || s.rdb == nil {
		return nil, ErrRunNotFound
	}
	b, err := s.rdb.Get(ctx, s.key(runID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}
	var run Run
	if err := json.Unmarshal(b, &run); err != nil {
		return nil, fmt.Errorf("osrun: stored run unreadable: %w", err)
	}
	if run.TenantID != tenantID {
		return nil, ErrRunNotFound
	}
	return &run, nil
}
