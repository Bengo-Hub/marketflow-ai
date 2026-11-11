package handlers

import (
	"testing"
	"time"
)

// Sprint 17 #49: WhatsApp assistant turns are kept in chat_sessions, one conversation per user
// per day, so the 30-day retention job removes them like any chat.
func TestWhatsAppSessionID(t *testing.T) {
	day := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	a := whatsAppSessionID("t1", "u1", day)
	if a != whatsAppSessionID("t1", "u1", day.Add(10*time.Hour)) {
		t.Fatal("same user, same UTC day: one session")
	}
	if a == whatsAppSessionID("t1", "u1", day.Add(24*time.Hour)) || a == whatsAppSessionID("t1", "u2", day) || a == whatsAppSessionID("t2", "u1", day) {
		t.Fatal("a new day, user or tenant starts a new session")
	}
	if a == leadSessionID("t1", "u1") {
		t.Fatal("assistant and lead sessions never collide")
	}
}
