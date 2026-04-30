package subscription

import "testing"

func TestDetectTargetFromUA(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ua   string
		want string
	}{
		{name: "empty falls back to v2rayn", ua: "", want: "v2rayn"},
		{name: "unknown falls back to v2rayn", ua: "Mozilla/5.0", want: "v2rayn"},

		{name: "clash for windows", ua: "ClashforWindows/0.20.39", want: "clash"},
		{name: "clash verge", ua: "Clash-verge/1.4.6", want: "clash"},
		{name: "stash takes clash precedence", ua: "Stash/2.6.0 (clash)", want: "clash"},
		{name: "clashx pro", ua: "ClashX Pro/1.72.0.4 (com.west2online.ClashXPro; build:1.72.0.4)", want: "clash"},

		{name: "shadowrocket", ua: "Shadowrocket/2155 CFNetwork/1240.0.4 Darwin/20.5.0", want: "shadowrocket"},
		{name: "quantumult x", ua: "Quantumult%20X/1.0.30 CFNetwork/1474 Darwin/23.0.0", want: "shadowrocket"},
		{name: "surge", ua: "Surge iOS/2796 CFNetwork", want: "shadowrocket"},
		{name: "loon", ua: "Loon/644 CFNetwork/1410", want: "shadowrocket"},

		{name: "sing-box dash", ua: "sing-box/1.8.0", want: "singbox"},
		{name: "singbox android (SFA)", ua: "SFA/1.8.0 (12345; sing-box 1.8.0)", want: "singbox"},
		{name: "singbox ios (SFI)", ua: "SFI/1.8.0", want: "singbox"},

		{name: "v2rayng", ua: "v2rayNG/1.8.5", want: "v2rayn"},
		{name: "v2rayn windows", ua: "V2rayN/6.31", want: "v2rayn"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := DetectTargetFromUA(tc.ua); got != tc.want {
				t.Fatalf("DetectTargetFromUA(%q) = %q, want %q", tc.ua, got, tc.want)
			}
		})
	}
}
