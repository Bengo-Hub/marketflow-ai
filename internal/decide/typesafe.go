package decide

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// TypeSafeInputUSDPerM is Jev's list price for input tokens (output is free),
// read 2026-10-22. It only feeds the spend ceiling and cost reports.
const TypeSafeInputUSDPerM = 0.042

// TypeSafeConfig configures the Jev adapter.
type TypeSafeConfig struct {
	BaseURL string // default https://api.typesafe.ai/v1
	APIKey  string
	Model   string // default jev-latest
	Timeout time.Duration
}

// TypeSafe calls Jev's POST /v1/systemone.
type TypeSafe struct {
	cfg  TypeSafeConfig
	http *http.Client
}

// NewTypeSafe builds the adapter.
func NewTypeSafe(cfg TypeSafeConfig) *TypeSafe {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.typesafe.ai/v1"
	}
	if cfg.Model == "" {
		cfg.Model = "jev-latest"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	return &TypeSafe{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout}}
}

// Name implements Adapter.
func (t *TypeSafe) Name() string { return "typesafe" }

type tsQuestion struct {
	Type         Kind   `json:"type"`
	Instructions string `json:"instructions"`
	// Criteria is an option-to-description map for a choice and an ordered label
	// list for a score; a noul has none.
	Criteria any `json:"criteria,omitempty"`
}

type tsRequest struct {
	Model     string                `json:"model"`
	State     string                `json:"state"`
	Questions map[string]tsQuestion `json:"questions"`
}

type tsAnswer struct {
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
	Score         *float64           `json:"score"`
	Noul          *float64           `json:"noul"`
	Confidence    *float64           `json:"confidence"`
}

type tsResponse struct {
	Answers map[string]tsAnswer `json:"answers"`
	Usage   struct {
		InputTokens int `json:"input_tokens"`
	} `json:"usage"`
}

// Decide implements Adapter.
func (t *TypeSafe) Decide(ctx context.Context, state string, questions map[string]Question) (Answers, Usage, error) {
	req := tsRequest{Model: t.cfg.Model, State: state, Questions: make(map[string]tsQuestion, len(questions))}
	for id, q := range questions {
		tq := tsQuestion{Type: q.Type, Instructions: q.Instructions}
		switch q.Type {
		case KindChoice:
			tq.Criteria = q.Options
		case KindScore:
			tq.Criteria = q.Scale
		}
		req.Questions[id] = tq
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, Usage{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(t.cfg.BaseURL, "/")+"/systemone", bytes.NewReader(body))
	if err != nil {
		return nil, Usage{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+t.cfg.APIKey)
	resp, err := t.http.Do(hreq)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("typesafe: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, Usage{}, fmt.Errorf("typesafe: status %d: %s", resp.StatusCode, msg)
	}
	var out tsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, Usage{}, fmt.Errorf("typesafe: decode: %w", err)
	}

	answers := make(Answers, len(questions))
	for id, q := range questions {
		ta, ok := out.Answers[id]
		if !ok {
			return nil, Usage{}, fmt.Errorf("typesafe: no answer for %q", id)
		}
		a := Answer{Type: q.Type}
		switch q.Type {
		case KindChoice:
			a.Choice, a.Probabilities = ta.Choice, ta.Probabilities
			if ta.Confidence != nil {
				a.Confidence = *ta.Confidence
			} else {
				a.Confidence = ta.Probabilities[ta.Choice]
			}
		case KindScore:
			if ta.Score == nil {
				return nil, Usage{}, fmt.Errorf("typesafe: %q has no score", id)
			}
			a.Score, a.Label = *ta.Score, nearestLabel(q.Scale, *ta.Score)
			if ta.Confidence != nil {
				a.Confidence = *ta.Confidence
			}
		case KindNoul:
			if ta.Noul == nil {
				return nil, Usage{}, fmt.Errorf("typesafe: %q has no noul", id)
			}
			a.Prob = *ta.Noul
			a.Confidence = noulConfidence(a.Prob)
		}
		answers[id] = a
	}
	if err := complete(questions, answers); err != nil {
		return nil, Usage{}, err
	}
	u := Usage{
		Adapter:     t.Name(),
		InputTokens: out.Usage.InputTokens,
		CostUSD:     float64(out.Usage.InputTokens) * TypeSafeInputUSDPerM / 1e6,
	}
	return answers, u, nil
}
