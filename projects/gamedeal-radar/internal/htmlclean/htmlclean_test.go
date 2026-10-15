package htmlclean

import "testing"

func TestClean(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"whitespace only", "   \n\t ", ""},
		{"trims input", "  <b>hi</b>  ", "<b>hi</b>"},
		{"script tag with content", `<p>ok</p><script>alert(1)</script>`, `<p>ok</p>`},
		{"script tag uppercase", `<SCRIPT>alert(1)</SCRIPT>after`, `after`},
		{"style tag with content", `<style>body{display:none}</style>text`, `text`},
		{"iframe dropped", `<iframe src="https://evil.example/"></iframe>x`, `x`},
		{"object and embed dropped", `<object data="x"></object><embed src="y">z`, `z`},
		{"form and input dropped", `<form><input name="a"></form>t`, `t`},
		{"img onerror dropped", `<img src=x onerror=alert(1)>`, ``},
		{"svg onload dropped", `<svg onload=alert(1)><circle r="1"/></svg>ok`, `ok`},
		{"onclick attribute removed", `<strong onclick="alert(1)">hi</strong>`, `<strong>hi</strong>`},
		{"onerror on allowed tag removed", `<b onerror="alert(1)">x</b>`, `<b>x</b>`},
		{"span style removed", `<span style="x:y">s</span>`, `<span>s</span>`},
		{"ul class bb_ul kept", `<ul class="bb_ul"><li>a</li></ul>`, `<ul class="bb_ul"><li>a</li></ul>`},
		{"ul other class removed", `<ul class="evil"><li>a</li></ul>`, `<ul><li>a</li></ul>`},
		{"javascript href dropped, text kept", `<a href="javascript:alert(1)">click</a>`, `click`},
		{"data href dropped, text kept", `<a href="data:text/html,<script>alert(1)</script>">x</a>`, `x`},
		{"http href dropped, text kept", `<a href="http://store.steampowered.com/">store</a>`, `store`},
		{"https href gets rel and target", `<a href="https://store.steampowered.com/app/1">store</a>`,
			`<a href="https://store.steampowered.com/app/1" rel="nofollow noreferrer noopener" target="_blank">store</a>`},
		// Malformed markup is passed through unrebalanced; every emitted tag is allowlisted, so this is safe.
		{"nested malformed tags", `<strong><em>a</strong></em>b<<b>c`, `<strong><em>a</strong></em>b&lt;<b>c`},
		{"unclosed strong", `<strong>Minimum:`, `<strong>Minimum:`},
		{"unclosed li", `<ul><li>one<li>two</ul>`, `<ul><li>one<li>two</ul>`},
		{"entity encoded script stays text", `&lt;script&gt;alert(1)&lt;/script&gt;`, `&lt;script&gt;alert(1)&lt;/script&gt;`},
		{"entity encoded img stays text", `&lt;img src=x onerror=alert(1)&gt;`, `&lt;img src=x onerror=alert(1)&gt;`},
		{"steam minimum requirements",
			`<strong>Minimum:</strong><br><ul class="bb_ul"><li><strong>OS:</strong> 64-bit Windows 10<br></li></ul>`,
			`<strong>Minimum:</strong><br><ul class="bb_ul"><li><strong>OS:</strong> 64-bit Windows 10<br></li></ul>`},
		{"steam language list",
			`English<strong>*</strong>, French<strong>*</strong>`,
			`English<strong>*</strong>, French<strong>*</strong>`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Clean(tt.in); got != tt.want {
				t.Errorf("Clean(%q)\n got: %q\nwant: %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSafeSteamURL(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{"akamai shared cdn", "https://shared.akamai.steamstatic.com/store_item_assets/x/header.jpg", "https://shared.akamai.steamstatic.com/store_item_assets/x/header.jpg", true},
		{"akamai video cdn", "https://video.akamai.steamstatic.com/store_trailers/1/movie.m3u8", "https://video.akamai.steamstatic.com/store_trailers/1/movie.m3u8", true},
		{"store host", "https://store.steampowered.com/app/1091500/", "https://store.steampowered.com/app/1091500/", true},
		{"bare steamstatic root", "https://steamstatic.com/x", "https://steamstatic.com/x", true},
		{"bare steampowered root", "https://steampowered.com/", "https://steampowered.com/", true},
		{"explicit 443 port", "https://shared.akamai.steamstatic.com:443/x.jpg", "https://shared.akamai.steamstatic.com:443/x.jpg", true},
		{"uppercase host", "https://SHARED.AKAMAI.STEAMSTATIC.COM/x.jpg", "https://SHARED.AKAMAI.STEAMSTATIC.COM/x.jpg", true},
		{"http scheme", "http://shared.akamai.steamstatic.com/x.jpg", "", false},
		{"javascript scheme", "javascript:alert(1)", "", false},
		{"protocol relative", "//shared.akamai.steamstatic.com/x.jpg", "", false},
		{"userinfo trick", "https://shared.akamai.steamstatic.com@evil.example/", "", false},
		{"userinfo with password", "https://user:pw@store.steampowered.com/", "", false},
		{"lookalike suffix domain", "https://steamstatic.com.evil.example/x.jpg", "", false},
		{"lookalike prefix host", "https://evilsteamstatic.com/x.jpg", "", false},
		{"lookalike powered", "https://evilsteampowered.com/", "", false},
		{"non-443 port", "https://shared.akamai.steamstatic.com:8443/x.jpg", "", false},
		{"empty", "", "", false},
		{"no host", "https:///x.jpg", "", false},
		{"unrelated host", "https://example.com/x.jpg", "", false},
		{"data uri", "data:image/png;base64,AAAA", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := SafeSteamURL(tt.in)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("SafeSteamURL(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
