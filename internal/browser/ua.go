package browser

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
)

// UASpec is a persisted per-profile browser identity. It is generated once per
// profile so the same account does not change fingerprint on every restart.
type UASpec struct {
	UserAgent string `json:"userAgent,omitempty"`
	Platform  string `json:"uaPlatform,omitempty"`
	Browser   string `json:"uaBrowser,omitempty"`
}

type uaCombo struct {
	platform string
	browser  string
}

// The matrix only contains plausible pairs. Safari does not exist on Windows,
// Linux or Android, so requests for those pairs fall back to macOS/iOS Safari
// (or, if Safari is excluded entirely, to Chrome/Firefox).
var uaMatrix = []uaCombo{
	{"windows", "chrome"}, {"windows", "chromium"}, {"windows", "firefox"},
	{"linux", "chrome"}, {"linux", "chromium"}, {"linux", "firefox"},
	{"android", "chrome"}, {"android", "chromium"}, {"android", "firefox"},
	{"macos", "chrome"}, {"macos", "chromium"}, {"macos", "firefox"}, {"macos", "safari"},
	{"ios", "safari"}, {"ios", "chrome"},
}

var androidDevices = []string{
	"Pixel 8", "Pixel 8 Pro", "Pixel 7", "SM-S918B", "SM-S911B", "OnePlus 12", "CPH2451",
}

// RandomUA picks one profile from the requested platform/browser sets. Empty
// filters mean "any". Unknown or impossible combinations fall back to the
// closest plausible one instead of emitting a self-contradictory UA.
func RandomUA(platforms, browsers []string) UASpec {
	plats := normalizeSet(platforms)
	brs := normalizeSet(browsers)
	var candidates []uaCombo
	for _, c := range uaMatrix {
		if len(plats) > 0 && !plats[c.platform] {
			continue
		}
		if len(brs) > 0 && !brs[c.browser] {
			continue
		}
		candidates = append(candidates, c)
	}
	if len(candidates) == 0 {
		// The caller asked for a combination the web does not have (e.g.
		// Android+Safari). Fall back to the browser family on a plausible OS.
		for _, c := range uaMatrix {
			if len(brs) == 0 || brs[c.browser] {
				candidates = append(candidates, c)
			}
		}
	}
	if len(candidates) == 0 {
		candidates = uaMatrix
	}
	c := candidates[pick(len(candidates))]
	return UASpec{
		UserAgent: buildUA(c.platform, c.browser),
		Platform:  c.platform,
		Browser:   c.browser,
	}
}

func normalizeSet(items []string) map[string]bool {
	out := map[string]bool{}
	for _, s := range items {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" {
			out[s] = true
		}
	}
	return out
}

func buildUA(platform, browser string) string {
	switch browser {
	case "firefox":
		return firefoxUA(platform)
	case "safari":
		if platform == "ios" {
			return safariIOSUA()
		}
		return safariMacUA()
	default: // chrome / chromium share the same UA token
		return chromiumUA(platform)
	}
}

func chromiumUA(platform string) string {
	ver := 140 + pick(17) // 140-156
	switch platform {
	case "windows":
		return fmt.Sprintf("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.0.0 Safari/537.36", ver)
	case "linux":
		return fmt.Sprintf("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.0.0 Safari/537.36", ver)
	case "android":
		android := 13 + pick(3) // 13-15
		device := androidDevices[pick(len(androidDevices))]
		return fmt.Sprintf("Mozilla/5.0 (Linux; Android %d; %s) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.0.0 Mobile Safari/537.36", android, device, ver)
	case "ios":
		return fmt.Sprintf("Mozilla/5.0 (iPhone; CPU iPhone OS %d_%d like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/%d.0.0.0 Mobile/15E148 Safari/604.1", 16+pick(3), pick(7), ver)
	default: // macos
		return fmt.Sprintf("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.0.0 Safari/537.36", ver)
	}
}

func firefoxUA(platform string) string {
	ver := 128 + pick(18) // 128-145
	switch platform {
	case "windows":
		return fmt.Sprintf("Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:%d.0) Gecko/20100101 Firefox/%d.0", ver, ver)
	case "linux":
		return fmt.Sprintf("Mozilla/5.0 (X11; Ubuntu; Linux x86_64; rv:%d.0) Gecko/20100101 Firefox/%d.0", ver, ver)
	case "android":
		android := 13 + pick(3)
		return fmt.Sprintf("Mozilla/5.0 (Android %d; Mobile; rv:%d.0) Gecko/%d.0 Firefox/%d.0", android, ver, ver, ver)
	case "ios":
		return safariIOSUA()
	default:
		return fmt.Sprintf("Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:%d.0) Gecko/20100101 Firefox/%d.0", ver, ver)
	}
}

func safariMacUA() string {
	major := 17 + pick(2) // 17-18
	minor := pick(7)
	return fmt.Sprintf("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/%d.%d Safari/605.1.15", major, minor)
}

func safariIOSUA() string {
	major := 16 + pick(3) // 16-18
	minor := pick(7)
	return fmt.Sprintf("Mozilla/5.0 (iPhone; CPU iPhone OS %d_%d like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/%d.%d Mobile/15E148 Safari/604.1", major, minor, major, minor)
}

func pick(n int) int {
	if n <= 0 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}
