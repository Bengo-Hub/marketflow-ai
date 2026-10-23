package team

import "context"

// Progress is one stage of a request, for the console's live status: which
// member is working on what. Stages: understanding, routed, quoting, planning,
// step, reviewing.
type Progress struct {
	Stage  string `json:"stage"`
	Member string `json:"member,omitempty"`
	Label  string `json:"label"`
}

type progressKey struct{}

// WithProgress makes Ask report each stage to fn. fn runs on the request's
// goroutine and must return quickly.
func WithProgress(ctx context.Context, fn func(Progress)) context.Context {
	return context.WithValue(ctx, progressKey{}, fn)
}

func report(ctx context.Context, p Progress) {
	if fn, ok := ctx.Value(progressKey{}).(func(Progress)); ok && fn != nil {
		fn(p)
	}
}
