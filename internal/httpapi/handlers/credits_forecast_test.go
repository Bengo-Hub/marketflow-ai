package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/registry"
)

type forecastSpender struct {
	fakeSpender
	balance   int64
	unlimited bool
	err       error
}

func (f *forecastSpender) Balance(context.Context, uuid.UUID) (int64, bool, error) {
	return f.balance, f.unlimited, f.err
}

func forecast(t *testing.T, s CreditSpender, output string) (creativeQuote, int) {
	t.Helper()
	h := &OSHandler{logger: zap.NewNop(), registry: func(context.Context) registry.Registry { return registry.Defaults() }, credits: s}
	body, _ := json.Marshal(map[string]string{"tenant_id": uuid.NewString(), "output": output})
	rec := httptest.NewRecorder()
	h.ForecastCredits(rec, httptest.NewRequest(http.MethodPost, "/api/v1/internal/credits/forecast", strings.NewReader(string(body))))
	var q creativeQuote
	_ = json.Unmarshal(rec.Body.Bytes(), &q)
	return q, rec.Code
}

// A forecast spends nothing, says when the balance is short, and never blocks a run when the
// balance cannot be read: the charge still refuses what does not fit.
func TestForecastCredits(t *testing.T) {
	price, err := registry.Defaults().Credits(registry.Script)
	if err != nil {
		t.Fatal(err)
	}
	short := &forecastSpender{balance: int64(price) - 1}
	q, code := forecast(t, short, registry.Script)
	if code != http.StatusOK || q.Enough || q.Shortfall != 1 || q.CreditsNeeded != price {
		t.Fatalf("short balance: %d %+v", code, q)
	}
	if short.spent != 0 {
		t.Fatalf("a forecast spent %d credits", short.spent)
	}
	if q, _ := forecast(t, &forecastSpender{balance: int64(price)}, registry.Script); !q.Enough || q.Shortfall != 0 {
		t.Fatalf("exact balance: %+v", q)
	}
	if q, _ := forecast(t, &forecastSpender{unlimited: true}, registry.Script); !q.Enough || q.Balance != -1 {
		t.Fatalf("unlimited plan: %+v", q)
	}
	if q, _ := forecast(t, &forecastSpender{err: errors.New("down")}, registry.Script); !q.Enough || q.Balance != -1 {
		t.Fatalf("unreadable balance must not block: %+v", q)
	}
	if _, code := forecast(t, &forecastSpender{}, "nope"); code != http.StatusBadRequest {
		t.Fatalf("unknown output: %d", code)
	}
}
