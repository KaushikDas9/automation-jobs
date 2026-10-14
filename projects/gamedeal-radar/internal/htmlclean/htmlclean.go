// Package htmlclean makes Steam HTML safe to store and render.
package htmlclean

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/microcosm-cc/bluemonday"
)

// policy is built once at package level. Only formatting tags survive;
// everything else is dropped, and script/style take their contents with them.
var policy = newPolicy()

func newPolicy() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowElements("strong", "b", "em", "i", "br", "ul", "ol", "li", "p", "span", "u", "a")
	p.AllowAttrs("class").Matching(regexp.MustCompile(`^bb_ul$`)).OnElements("ul")
	p.AllowAttrs("href").OnElements("a")
	p.AllowURLSchemes("https")
	p.RequireParseableURLs(true)
	p.RequireNoFollowOnLinks(true)
	p.RequireNoReferrerOnLinks(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)
	return p
}

// Clean returns s with only an allowlist of formatting tags kept.
func Clean(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return strings.TrimSpace(policy.Sanitize(s))
}

var steamRoots = []string{"steamstatic.com", "steampowered.com"}

// SafeSteamURL reports whether u is an https URL on a Steam asset host, and returns it.
func SafeSteamURL(u string) (string, bool) {
	parsed, err := url.Parse(u)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil {
		return "", false
	}
	if port := parsed.Port(); port != "" && port != "443" {
		return "", false
	}
	host := strings.ToLower(parsed.Hostname())
	for _, root := range steamRoots {
		if host == root || strings.HasSuffix(host, "."+root) {
			return u, true
		}
	}
	return "", false
}
