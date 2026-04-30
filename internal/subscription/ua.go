package subscription

import "strings"

// DetectTargetFromUA returns a subscription target appropriate for the given
// client User-Agent. It only considers UA strings that are clearly a known
// proxy client; anything unrecognized falls through to "v2rayn", matching the
// historical default.
//
// Multi-client matches use the first wins-by-specificity ordering: e.g. "stash"
// is checked before "clash" because Stash UAs typically include both words.
func DetectTargetFromUA(ua string) string {
	lower := strings.ToLower(strings.TrimSpace(ua))
	if lower == "" {
		return "v2rayn"
	}
	switch {
	case strings.Contains(lower, "stash"):
		return "clash"
	case strings.Contains(lower, "clash"):
		return "clash"
	case strings.Contains(lower, "sing-box"),
		strings.Contains(lower, "singbox"),
		strings.Contains(lower, "sfa/"),
		strings.Contains(lower, "sfi/"),
		strings.Contains(lower, "sfm/"),
		strings.Contains(lower, "sfw/"):
		return "singbox"
	case strings.Contains(lower, "shadowrocket"):
		return "shadowrocket"
	case strings.Contains(lower, "quantumult"),
		strings.Contains(lower, "surge"),
		strings.Contains(lower, "loon"):
		return "shadowrocket"
	case strings.Contains(lower, "v2rayng"),
		strings.Contains(lower, "v2rayn"):
		return "v2rayn"
	}
	return "v2rayn"
}
