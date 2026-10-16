package storeprices

import (
	"fmt"
	"strings"
)

// Line renders the summary as one key=value line for the job log.
func (s Summary) Line(store string) string {
	return fmt.Sprintf(
		"summary store=%s dry_run=%t already_complete=%t blocked=%t cancelled=%t games=%d "+
			"matched=%d with_price_in=%d with_price_us=%d not_listed_us=%d not_listed_in=%d "+
			"listed_no_price_us=%d listed_no_price_in=%d no_epic_deal_in=%d no_epic_deal_us=%d "+
			"no_match=%d title_mismatch=%d slug_links=%d search_links=%d "+
			"skipped_manual_games=%d skipped_sanity_rows=%d kept_manual_rows=%d written=%d "+
			"failed=%d retried=%d coverage_pct=%.1f",
		store, s.DryRun, s.AlreadyComplete, s.Blocked, s.Cancelled, s.Games,
		s.Matched, s.WithPrice[RegionIN], s.WithPrice[RegionUS], s.NotListed[RegionUS], s.NotListed[RegionIN],
		s.ListedNoPrice[RegionUS], s.ListedNoPrice[RegionIN], s.NoPrice[RegionIN], s.NoPrice[RegionUS],
		s.NoMatch, s.TitleMismatch, s.SlugLinks, s.SearchLinks,
		s.SkippedManual, s.SkippedSanity, s.KeptManual, s.Written,
		s.Failed, s.Retried, s.Coverage*100,
	)
}

// Detail is the short line used in Slack.
func (s Summary) Detail() string {
	var parts []string
	if s.DryRun {
		parts = append(parts, "dry run")
	}
	if s.AlreadyComplete {
		return "already complete, nothing fetched"
	}
	if s.Blocked {
		parts = append(parts, "blocked by the store")
	}
	if s.Cancelled {
		parts = append(parts, "cancelled")
	}
	parts = append(parts,
		fmt.Sprintf("%d games", s.Games),
		fmt.Sprintf("%d rows written", s.Written),
		fmt.Sprintf("%d failed", s.Failed),
		fmt.Sprintf("%.1f%% coverage", s.Coverage*100),
	)
	return strings.Join(parts, ", ")
}
