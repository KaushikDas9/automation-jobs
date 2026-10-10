package slot

import (
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestStart(t *testing.T) {
	cases := []struct{ now, want string }{
		{"2026-10-10T06:30:00Z", "2026-10-10T06:30:00Z"},
		{"2026-10-10T07:00:00Z", "2026-10-10T06:30:00Z"},
		{"2026-10-10T06:29:59Z", "2026-10-09T18:30:00Z"},
		{"2026-10-10T18:30:00Z", "2026-10-10T18:30:00Z"},
		{"2026-10-10T19:00:00Z", "2026-10-10T18:30:00Z"},
		{"2026-10-10T18:29:59Z", "2026-10-10T06:30:00Z"},
		{"2026-10-11T00:10:00Z", "2026-10-10T18:30:00Z"},
		{"2026-10-10T23:59:00Z", "2026-10-10T18:30:00Z"},
	}
	for _, tc := range cases {
		if got := Start(mustTime(t, tc.now)); !got.Equal(mustTime(t, tc.want)) {
			t.Errorf("Start(%s) = %s, want %s", tc.now, got.Format(time.RFC3339), tc.want)
		}
	}
}

func TestStartConvertsNonUTCInput(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+30*60)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, ist) // 06:30 UTC
	if got := Start(now); !got.Equal(time.Date(2026, 10, 10, 6, 30, 0, 0, time.UTC)) {
		t.Fatalf("got %s", got)
	}
}

func TestOffsetsMatchTheCronSchedule(t *testing.T) {
	// Cumulative offsets from the revision: +0, +30, +75, +135, +210, +300 minutes.
	want := []time.Duration{0, 30, 75, 135, 210, 300}
	for i, w := range want {
		if Offsets[i] != w*time.Minute {
			t.Fatalf("offset %d = %v, want %v", i+1, Offsets[i], w*time.Minute)
		}
	}
	if MaxAttempts != 6 || Window != 6*time.Hour {
		t.Fatal("MaxAttempts or Window changed")
	}
}

func TestAttemptForExactOffsets(t *testing.T) {
	for i, off := range Offsets {
		attempt, inWindow := AttemptFor(off)
		if attempt != i+1 || !inWindow {
			t.Fatalf("offset %v -> attempt %d inWindow %v, want %d true", off, attempt, inWindow, i+1)
		}
	}
}

func TestAttemptForToleranceAndLateRuns(t *testing.T) {
	cases := []struct {
		name    string
		elapsed time.Duration
		want    int
	}{
		{"within tolerance of attempt 2", 25 * time.Minute, 2},
		{"15 minutes late for attempt 3", 90 * time.Minute, 3},
		{"22 minutes late, outside tolerance, attributed to the attempt that started", 97 * time.Minute, 3},
		{"late attempt 6 at 340 minutes", 340 * time.Minute, 6},
		{"attempt 4 started 20 minutes late", 155 * time.Minute, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, inWindow := AttemptFor(tc.elapsed)
			if got != tc.want || !inWindow {
				t.Fatalf("elapsed %v -> attempt %d inWindow %v, want %d", tc.elapsed, got, inWindow, tc.want)
			}
		})
	}
}

func TestAttemptForOutOfWindow(t *testing.T) {
	if _, ok := AttemptFor(-time.Minute); ok {
		t.Fatal("negative elapsed must be out of window")
	}
	if _, ok := AttemptFor(Window + time.Minute); ok {
		t.Fatal("elapsed beyond 6h must be out of window")
	}
	if _, ok := AttemptFor(Window); !ok {
		t.Fatal("exactly 6h is still inside the window")
	}
}

func TestPlanForScheduledRuns(t *testing.T) {
	slotA := mustTime(t, "2026-10-10T18:30:00Z")

	p := PlanFor(mustTime(t, "2026-10-10T19:00:00Z"), nil, 0, false)
	if p.Attempt != 2 || p.Skip || p.Final() || !p.Slot.Equal(slotA) {
		t.Fatalf("19:00 plan = %+v", p)
	}

	p = PlanFor(mustTime(t, "2026-10-10T23:30:00Z"), nil, 0, false)
	if p.Attempt != 6 || !p.Final() {
		t.Fatalf("23:30 should be the final attempt, got %+v", p)
	}

	// 07:30 UTC is 60 minutes into the 06:30 slot. That is 15 minutes from the
	// 75-minute attempt (07:45), inside tolerance, so it maps to attempt 3.
	p = PlanFor(mustTime(t, "2026-10-10T07:30:00Z"), nil, 0, false)
	if p.Attempt != 3 || p.Skip {
		t.Fatalf("07:30 plan = %+v", p)
	}
}

func TestPlanForOutsideWindowSkips(t *testing.T) {
	// 13:00 UTC: 6h30m after the 06:30 slot start. Outside the window.
	p := PlanFor(mustTime(t, "2026-10-10T13:00:00Z"), nil, 0, false)
	if !p.Skip {
		t.Fatalf("scheduled run outside the window must be skipped, got %+v", p)
	}
}

func TestPlanForManualRunSkipsWindowAndIsNotFinal(t *testing.T) {
	p := PlanFor(mustTime(t, "2026-10-10T23:40:00Z"), nil, 0, true)
	if p.Skip {
		t.Fatal("manual runs must skip the window check")
	}
	if p.Attempt != 0 || p.Final() {
		t.Fatalf("manual run without ATTEMPT must be attempt 0 and not final, got %+v", p)
	}
}

func TestPlanForAttemptOverride(t *testing.T) {
	p := PlanFor(mustTime(t, "2026-10-10T13:00:00Z"), nil, 6, false)
	if p.Attempt != 6 || !p.Final() || p.Skip {
		t.Fatalf("ATTEMPT=6 must be final and not skipped, got %+v", p)
	}
}

func TestPlanForSlotOverrideSkipsWindowCheck(t *testing.T) {
	override := mustTime(t, "2026-10-01T06:30:00Z")
	p := PlanFor(mustTime(t, "2026-10-10T13:00:00Z"), &override, 0, false)
	if p.Skip || !p.Overridden || !p.Slot.Equal(override) {
		t.Fatalf("SLOT_START override must not skip, got %+v", p)
	}
}

func TestIDAndTitleFormatting(t *testing.T) {
	s := mustTime(t, "2026-10-10T18:30:00Z")
	if got := ID(s); got != "2026-10-10 18:30Z" {
		t.Fatalf("ID = %q", got)
	}
	ist := time.FixedZone("IST", 5*3600+30*60)
	if got := ID(s.In(ist)); got != "2026-10-10 18:30Z" {
		t.Fatalf("ID must be UTC regardless of input zone, got %q", got)
	}
	want := "GameDeal Radar: slot 2026-10-10 18:30Z failed after 6 attempts"
	if got := Title(s); got != want {
		t.Fatalf("Title = %q, want %q", got, want)
	}
}
