// Package youtube discovers a channel's recent uploads via its public RSS feed
// (no API key / quota), and resolves @handles to channel ids.
package youtube

import (
	"bufio"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mtosity/thestockie-influencer/internal/models"
)

const (
	feedURL   = "https://www.youtube.com/feeds/videos.xml?channel_id="
	userAgent = "thestockie-influencer/1.0 (+https://thestockie.com)"
)

// CookieFile is the path to a Netscape-format cookies.txt exported from a
// browser.  When non-empty, DiscoverVideos and ResolveChannelID read it and
// send matching cookies on requests so that YouTube does not return 404 for
// RSS feeds on IPs that are otherwise challenged.
var CookieFile string

// youtubeCookies reads a Netscape cookies.txt and returns a single Cookie
// header string with all non-expired .youtube.com entries.
func youtubeCookies(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	now := time.Now()
	var parts []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 7 {
			continue
		}
		domain := fields[0]
		if !strings.HasSuffix(domain, ".youtube.com") {
			continue
		}
		expSec, err := strconv.ParseInt(fields[4], 10, 64)
		if err == nil && expSec > 0 && now.After(time.Unix(expSec, 0)) {
			continue // expired
		}
		name := fields[5]
		value := fields[6]
		parts = append(parts, fmt.Sprintf("%s=%s", name, value))
	}
	return strings.Join(parts, "; ")
}

// atomFeed mirrors the subset of the YouTube Atom feed we need. encoding/xml
// matches by local element name, so namespace prefixes (yt:, media:) are fine.
type atomFeed struct {
	Entries []struct {
		VideoID   string `xml:"videoId"`
		Title     string `xml:"title"`
		Published string `xml:"published"`
	} `xml:"entry"`
}

// DiscoverVideos returns the recent uploads for a channel, newest first.
func DiscoverVideos(ctx context.Context, hc *http.Client, channelID string) ([]models.VideoCandidate, error) {
	if !strings.HasPrefix(channelID, "UC") {
		return nil, fmt.Errorf("invalid channelId %q (expected UC...)", channelID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL+channelID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	if CookieFile != "" {
		if c := youtubeCookies(CookieFile); c != "" {
			req.Header.Set("Cookie", c)
		}
	}

	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("feed %s: status %d", channelID, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}

	var feed atomFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("parse feed %s: %w", channelID, err)
	}

	out := make([]models.VideoCandidate, 0, len(feed.Entries))
	for _, e := range feed.Entries {
		if e.VideoID == "" {
			continue
		}
		var publishedMs int64
		if t, err := time.Parse(time.RFC3339, e.Published); err == nil {
			publishedMs = t.UnixMilli()
		}
		out = append(out, models.VideoCandidate{
			VideoID:     e.VideoID,
			ChannelID:   channelID,
			Title:       strings.TrimSpace(e.Title),
			URL:         "https://www.youtube.com/watch?v=" + e.VideoID,
			PublishedAt: publishedMs,
		})
	}
	return out, nil
}

var (
	canonicalRe  = regexp.MustCompile(`rel="canonical" href="https://www\.youtube\.com/channel/(UC[0-9A-Za-z_-]{20,})"`)
	externalIDRe = regexp.MustCompile(`"externalId":"(UC[0-9A-Za-z_-]{20,})"`)
	channelIDRe  = regexp.MustCompile(`"channelId":"(UC[0-9A-Za-z_-]{20,})"`)
)

// ResolveChannelID fetches a channel page (by @handle, full URL, or bare
// handle) and extracts its UC channel id.
func ResolveChannelID(ctx context.Context, hc *http.Client, handleOrURL string) (string, error) {
	url := normalizeChannelURL(handleOrURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	if CookieFile != "" {
		if c := youtubeCookies(CookieFile); c != "" {
			req.Header.Set("Cookie", c)
		}
	}

	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolve %s: status %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 3<<20))
	if err != nil {
		return "", err
	}
	// Prefer the page's own channel: the canonical /channel/ link, then
	// externalId. Only fall back to a bare "channelId" (which can belong to a
	// featured/secondary channel that appears earlier in the HTML).
	for _, re := range []*regexp.Regexp{canonicalRe, externalIDRe, channelIDRe} {
		if m := re.FindSubmatch(body); m != nil {
			return string(m[1]), nil
		}
	}
	return "", fmt.Errorf("no channelId found at %s", url)
}

// VideoIDFromURL extracts the YouTube video id from a watch/shorts/embed/youtu.be URL.
func VideoIDFromURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	if strings.Contains(u.Host, "youtu.be") {
		return strings.Trim(u.Path, "/")
	}
	if v := u.Query().Get("v"); v != "" {
		return v
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 2 && (parts[0] == "shorts" || parts[0] == "embed") {
		return parts[1]
	}
	return ""
}

func normalizeChannelURL(s string) string {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(s, "http://"), strings.HasPrefix(s, "https://"):
		return s
	case strings.HasPrefix(s, "@"):
		return "https://www.youtube.com/" + s
	default:
		return "https://www.youtube.com/@" + s
	}
}
