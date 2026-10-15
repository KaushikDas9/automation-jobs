package steam

import "testing"

func TestSafeWebsite(t *testing.T) {
	cases := map[string]string{
		"https://www.cyberpunk.net": "https://www.cyberpunk.net",
		"  http://example.com/a  ":  "http://example.com/a",
		"javascript:alert(1)":       "",
		"data:text/html,<b>x</b>":   "",
		"//evil.example":            "",
		"/relative":                 "",
		"":                          "",
		"ftp://example.com":         "",
	}
	for in, want := range cases {
		if got := safeWebsite(in); got != want {
			t.Errorf("safeWebsite(%q) = %q, want %q", in, got, want)
		}
	}
}
