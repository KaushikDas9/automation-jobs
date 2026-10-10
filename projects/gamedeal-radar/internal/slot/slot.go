// Package slot maps the clock to a scheduled check slot and attempt.
//
// Slots start at 06:30 UTC and 18:30 UTC. Each slot has up to six attempts at growing
// gaps (cumulative offsets 0, 30, 75, 135, 210 and 300 minutes). An attempt must
// finish within Window of the slot start, so nothing from one slot can reach the next.
package slot

import (
	"fmt"
	"time"
)

const (
	// MaxAttempts is the hard cap of attempts per slot.
	MaxAttempts = 6
	// Window is how long after the slot start an attempt may still run.
	Window = 6 * time.Hour
	// Tolerance covers GitHub starting scheduled runs late.
	Tolerance = 20 * time.Minute
)

// Offsets are the cumulative attempt start offsets from the slot start.
var Offsets = [MaxAttempts]time.Duration{
	0,
	30 * time.Minute,
	75 * time.Minute,
	135 * time.Minute,
	210 * time.Minute,
	300 * time.Minute,
}

// Start returns the most recent slot boundary (06:30 or 18:30 UTC) at or before now.
func Start(now time.Time) time.Time {
	now = now.UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	// Candidates in ascending order: yesterday 18:30, today 06:30, today 18:30.
	candidates := []time.Time{
		day.AddDate(0, 0, -1).Add(18*time.Hour + 30*time.Minute),
		day.Add(6*time.Hour + 30*time.Minute),
		day.Add(18*time.Hour + 30*time.Minute),
	}
	best := candidates[0]
	for _, c := range candidates {
		if !c.After(now) {
			best = c
		}
	}
	return best
}

// AttemptFor maps the time elapsed since the slot start to an attempt number 1..6.
// An elapsed time within Tolerance of a scheduled offset is that attempt. Otherwise
// the most recent attempt that has started is used. inWindow is false when elapsed
// is negative or beyond Window.
func AttemptFor(elapsed time.Duration) (attempt int, inWindow bool) {
	inWindow = elapsed >= 0 && elapsed <= Window

	nearest, nearestDist := -1, time.Duration(0)
	for i, off := range Offsets {
		d := abs(elapsed - off)
		if d <= Tolerance && (nearest < 0 || d < nearestDist) {
			nearest, nearestDist = i, d
		}
	}
	if nearest >= 0 {
		return nearest + 1, inWindow
	}

	best := 0
	for i, off := range Offsets {
		if elapsed >= off {
			best = i
		}
	}
	return best + 1, inWindow
}

// Plan describes how this run fits into the schedule.
type Plan struct {
	Slot       time.Time // slot start, UTC
	Attempt    int       // 1..MaxAttempts, or 0 for a manual run with no ATTEMPT set
	InWindow   bool
	Skip       bool // true when a scheduled run is outside the slot window
	Overridden bool // SLOT_START or ATTEMPT was set explicitly
}

// Final reports whether this is the last scheduled attempt of the slot.
func (p Plan) Final() bool { return p.Attempt == MaxAttempts }

// PlanFor decides the slot, attempt and whether to skip the run.
//
//   - slotOverride, when set, replaces the clock-derived slot start (testing).
//   - attemptOverride, when 1..6, replaces the clock-derived attempt.
//   - manual marks a workflow_dispatch run. Manual runs skip the window check and,
//     without an attempt override, are not counted as a scheduled attempt (attempt 0),
//     so they never trigger the final-failure notification.
func PlanFor(now time.Time, slotOverride *time.Time, attemptOverride int, manual bool) Plan {
	start := Start(now)
	overridden := slotOverride != nil || attemptOverride > 0
	if slotOverride != nil {
		start = slotOverride.UTC()
	}
	elapsed := now.UTC().Sub(start)
	attempt, inWindow := AttemptFor(elapsed)

	p := Plan{Slot: start, Attempt: attempt, InWindow: inWindow, Overridden: overridden}
	switch {
	case attemptOverride > 0:
		p.Attempt = attemptOverride
	case manual:
		p.Attempt = 0
	}
	if !manual && attemptOverride == 0 && slotOverride == nil && !inWindow {
		p.Skip = true
	}
	return p
}

// ID formats a slot start as "2006-01-02 15:04Z", for example "2026-10-10 18:30Z".
func ID(slotStart time.Time) string {
	return slotStart.UTC().Format("2006-01-02 15:04Z")
}

// Title is the GitHub issue title used for a final failure.
func Title(slotStart time.Time) string {
	return fmt.Sprintf("GameDeal Radar: slot %s failed after %d attempts", ID(slotStart), MaxAttempts)
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
