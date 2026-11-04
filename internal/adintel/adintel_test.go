package adintel

import (
	"testing"
	"time"
)

func TestRankWinners(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	ads := []Ad{
		{AdvertiserID: "A", CreativeID: "short", Format: "video", DaysShown: 3, LastShown: now.Add(-30 * 24 * time.Hour)},
		{AdvertiserID: "A", CreativeID: "long-live", Format: "video", DaysShown: 40, LastShown: now.Add(-time.Hour)},
		{AdvertiserID: "B", CreativeID: "long-dead", Format: "image", DaysShown: 45, LastShown: now.Add(-20 * 24 * time.Hour)},
		{AdvertiserID: "C", CreativeID: "dates-only", Format: "text", FirstShown: now.Add(-10 * 24 * time.Hour), LastShown: now.Add(-time.Hour)},
	}
	Rank(ads, now)
	if ads[0].CreativeID != "long-live" || !ads[0].StillRunning {
		t.Fatalf("a long ad still running wins: %+v", ads[0])
	}
	if ads[1].CreativeID != "long-dead" {
		t.Fatalf("a long but stopped ad comes next: %+v", ads[1])
	}
	for _, a := range ads {
		if a.CreativeID == "dates-only" && a.Score < 20 {
			t.Fatalf("days are computed from the dates when total_days_shown is missing: %+v", a)
		}
	}
}
