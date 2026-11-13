// Package osrun keeps a Marketing Manager media turn after it answers, so approving it and
// making its media read what the owner saw instead of quoting again (Sprint 15 #134/#138,
// Sprint 17). It also holds the WhatsApp approval codes: "Reply YES <code> to approve".
// Everything lives in Redis with a time limit; marketflow-api owns the briefs and versions.
package osrun

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Config sets how long runs and codes are kept. The defaults live here and nowhere else.
type Config struct {
	// RunTTL is how long a quoted or approved turn can still be approved or made into media.
	RunTTL time.Duration
	// CodeTTL is how long a WhatsApp approval code works (owner, 2026-11-12: 30 minutes).
	CodeTTL time.Duration
	// CodeAttempts is how many wrong codes cancel a pending one.
	CodeAttempts int
	// MediaTimeout bounds the background work after a WhatsApp YES: the approved turn and the
	// Studio calls, or the storyboard approval and render start. It is also how long the
	// single-flight lock on a run or a version lasts.
	MediaTimeout time.Duration
}

// DefaultConfig is 24 hours for a run, 30 minutes and 3 tries for a code, 5 minutes for the
// background media work.
func DefaultConfig() Config {
	return Config{RunTTL: 24 * time.Hour, CodeTTL: 30 * time.Minute, CodeAttempts: 3, MediaTimeout: 5 * time.Minute}
}

// ConfigFromEnv reads MF_OS_RUN_TTL, MF_OS_APPROVAL_CODE_TTL, MF_OS_MEDIA_TIMEOUT (Go durations
// such as "24h" or "30m") and MF_OS_APPROVAL_CODE_ATTEMPTS. A bad value keeps its default; the error lists it.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg := DefaultConfig()
	var bad []string
	dur := func(key string, dst *time.Duration) {
		v := strings.TrimSpace(getenv(key))
		if v == "" {
			return
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			bad = append(bad, key+" must be a positive duration such as 30m")
			return
		}
		*dst = d
	}
	dur("MF_OS_RUN_TTL", &cfg.RunTTL)
	dur("MF_OS_APPROVAL_CODE_TTL", &cfg.CodeTTL)
	dur("MF_OS_MEDIA_TIMEOUT", &cfg.MediaTimeout)
	if v := strings.TrimSpace(getenv("MF_OS_APPROVAL_CODE_ATTEMPTS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 10 {
			bad = append(bad, "MF_OS_APPROVAL_CODE_ATTEMPTS must be 1 to 10")
		} else {
			cfg.CodeAttempts = n
		}
	}
	if len(bad) > 0 {
		return cfg, fmt.Errorf("osrun: %s", strings.Join(bad, "; "))
	}
	return cfg, nil
}
