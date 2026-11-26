package transcribe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// errNoCaptions means the video has no usable English caption track (yet).
// Auto-captions can appear hours after upload, so whisper is the fallback.
var errNoCaptions = errors.New("no usable English captions")

// minCaptionChars guards against stub tracks (music-only, intros) that would
// otherwise pass as a "transcript".
const minCaptionChars = 300

// Captions returns the video's own YouTube transcript and the track it used.
//
// Track preference:
//  1. creator-uploaded English subtitles ("en", "en-US", ...);
//  2. the original-language speech-recognition track ("en-orig", "en-US-orig"...)
//     when the video is in English.
//
// The plain auto "en" track is deliberately never used: for English videos
// YouTube serves it as a machine-translated rewrite of the ASR, not the words
// actually spoken.
func (t *Transcriber) Captions(ctx context.Context, videoID, videoURL string) (text, track string, err error) {
	// 1) List the available tracks.
	args := append(t.baseArgs(), "--skip-download", "-J", "--no-playlist", "--no-warnings", videoURL)
	out, err := t.output(ctx, args...)
	if err != nil {
		return "", "", fmt.Errorf("yt-dlp list captions: %w", err)
	}
	var info struct {
		Language          string                     `json:"language"`
		Subtitles         map[string]json.RawMessage `json:"subtitles"`
		AutomaticCaptions map[string]json.RawMessage `json:"automatic_captions"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return "", "", fmt.Errorf("parse yt-dlp json: %w", err)
	}
	track, auto := pickTrack(info.Language, keys(info.Subtitles), keys(info.AutomaticCaptions))
	if track == "" {
		return "", "", errNoCaptions
	}

	// 2) Download just that track as WebVTT.
	base := filepath.Join(t.WorkDir, videoID+".cap")
	vtt := base + "." + track + ".vtt"
	defer os.Remove(vtt)
	writeFlag := "--write-subs"
	if auto {
		writeFlag = "--write-auto-subs"
	}
	args = append(t.baseArgs(), "--skip-download", writeFlag, "--sub-langs", track,
		"--sub-format", "vtt", "--no-playlist", "--no-progress", "--quiet", "--no-warnings",
		"-o", base+".%(ext)s", videoURL)
	if msg, err := run(ctx, t.YtDlp, args...); err != nil {
		return "", "", fmt.Errorf("yt-dlp download captions %s: %w: %s", track, err, msg)
	}
	data, err := os.ReadFile(vtt)
	if err != nil {
		return "", "", fmt.Errorf("read captions %s: %w", track, err)
	}
	text = vttToText(string(data))
	if len(text) < minCaptionChars {
		return "", "", fmt.Errorf("%w: track %s has only %d chars", errNoCaptions, track, len(text))
	}
	return text, track, nil
}

// pickTrack chooses a caption track; auto reports whether it is an
// automatic (speech-recognition) track.
func pickTrack(lang string, manual, automatic []string) (track string, auto bool) {
	isEn := func(k string) bool { return k == "en" || strings.HasPrefix(k, "en-") }
	for _, k := range manual {
		if isEn(k) && !strings.HasSuffix(k, "-orig") {
			return k, false
		}
	}
	// Only trust ASR for English-language videos; otherwise let whisper
	// (WHISPER_LANG=auto) handle the original speech.
	if lang != "" && !isEn(lang) {
		return "", false
	}
	if lang != "" {
		for _, k := range automatic {
			if k == lang+"-orig" {
				return k, true
			}
		}
	}
	for _, k := range automatic {
		if isEn(k) && strings.HasSuffix(k, "-orig") {
			return k, true
		}
	}
	return "", false
}

var (
	vttTag    = regexp.MustCompile(`<[^>]*>`)
	vttTiming = regexp.MustCompile(`^\d{2}:\d{2}[:.]\d{2}.*-->`)
)

// vttToText flattens WebVTT into plain text. Auto-captions "roll": each cue
// repeats the previous line before adding a new one, so consecutive duplicate
// lines are collapsed.
func vttToText(vtt string) string {
	var lines []string
	last := ""
	for _, raw := range strings.Split(vtt, "\n") {
		l := strings.TrimSpace(raw)
		if l == "" || l == "WEBVTT" || vttTiming.MatchString(l) ||
			strings.HasPrefix(l, "Kind:") || strings.HasPrefix(l, "Language:") ||
			strings.HasPrefix(l, "NOTE") || strings.HasPrefix(l, "STYLE") {
			continue
		}
		l = strings.TrimSpace(htmlUnescape(vttTag.ReplaceAllString(l, "")))
		if l == "" || l == last {
			continue
		}
		lines = append(lines, l)
		last = l
	}
	return strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
}

var htmlEntities = strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'", "&nbsp;", " ")

func htmlUnescape(s string) string { return htmlEntities.Replace(s) }

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// baseArgs are the yt-dlp flags shared by every YouTube call: JS challenge
// solving, cookies, and any extra args from the environment.
func (t *Transcriber) baseArgs() []string {
	args := []string{"--remote-components", "ejs:github", "--js-runtimes", "node"}
	if t.CookiesFromBrowser != "" {
		args = append(args, "--cookies-from-browser", t.CookiesFromBrowser)
	} else if t.CookiesFile != "" {
		args = append(args, "--cookies", t.CookiesFile)
	}
	if t.ExtraArgs != "" {
		args = append(args, strings.Fields(t.ExtraArgs)...)
	}
	return args
}

// output runs yt-dlp and returns stdout only (stderr is kept for errors).
func (t *Transcriber) output(ctx context.Context, args ...string) ([]byte, error) {
	var stderr strings.Builder
	cmd := exec.CommandContext(ctx, t.YtDlp, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 1000 {
			msg = msg[len(msg)-1000:]
		}
		return nil, fmt.Errorf("%w: %s", err, msg)
	}
	return out, nil
}
