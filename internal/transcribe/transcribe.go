// Package transcribe turns a YouTube video into text: yt-dlp pulls the audio,
// ffmpeg (via yt-dlp) downsamples to 16 kHz mono, and whisper.cpp transcribes.
package transcribe

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Transcriber struct {
	YtDlp              string
	Ffmpeg             string // optional; passed as --ffmpeg-location
	CookiesFromBrowser string // e.g. "chrome"; beats YouTube bot checks
	CookiesFile        string // path to exported cookies.txt
	WhisperBin         string
	WhisperModel       string
	Threads            int
	Lang               string
	WorkDir            string
	KeepAudio          bool
	Log                *slog.Logger
}

type whisperJSON struct {
	Transcription []struct {
		Text string `json:"text"`
	} `json:"transcription"`
}

// Transcribe downloads and transcribes a single video, returning the full text.
func (t *Transcriber) Transcribe(ctx context.Context, videoID, videoURL string) (string, error) {
	if err := os.MkdirAll(t.WorkDir, 0o755); err != nil {
		return "", fmt.Errorf("workdir: %w", err)
	}
	wav := filepath.Join(t.WorkDir, videoID+".wav")
	jsonPath := filepath.Join(t.WorkDir, videoID+".json")
	outTemplate := filepath.Join(t.WorkDir, videoID+".%(ext)s")
	outBase := filepath.Join(t.WorkDir, videoID)

	if !t.KeepAudio {
		defer os.Remove(wav)
		defer os.Remove(jsonPath)
	}

	// 1) Download bestaudio and let yt-dlp's ffmpeg emit 16 kHz mono PCM wav.
	ytArgs := []string{
		"-x", "--audio-format", "wav",
		"--postprocessor-args", "ffmpeg:-ar 16000 -ac 1 -acodec pcm_s16le",
		"--no-playlist", "--no-progress", "--quiet", "--no-warnings",
		"--remote-components", "ejs:github",
		"--js-runtimes", "node",
		"-o", outTemplate,
	}
	if t.Ffmpeg != "" {
		ytArgs = append(ytArgs, "--ffmpeg-location", t.Ffmpeg)
	}
	if t.CookiesFromBrowser != "" {
		ytArgs = append(ytArgs, "--cookies-from-browser", t.CookiesFromBrowser)
	} else if t.CookiesFile != "" {
		ytArgs = append(ytArgs, "--cookies", t.CookiesFile)
	}
	ytArgs = append(ytArgs, videoURL)
	// YouTube intermittently bot-challenges even authenticated requests under
	// load; a fresh invocation usually clears it, so retry a few times.
	var ytOut string
	var ytErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt-1) * 5 * time.Second):
			}
		}
		if ytOut, ytErr = run(ctx, t.YtDlp, ytArgs...); ytErr == nil {
			break
		}
	}
	if ytErr != nil {
		return "", fmt.Errorf("yt-dlp: %w: %s", ytErr, ytOut)
	}
	if _, err := os.Stat(wav); err != nil {
		return "", fmt.Errorf("expected audio at %s: %w", wav, err)
	}

	// 2) Transcribe with whisper.cpp → <outBase>.json.
	wArgs := []string{
		"-m", t.WhisperModel,
		"-f", wav,
		"-l", t.Lang,
		"-t", strconv.Itoa(t.Threads),
		"-oj", "-of", outBase,
		"-np",
	}
	if out, err := run(ctx, t.WhisperBin, wArgs...); err != nil {
		return "", fmt.Errorf("whisper: %w: %s", err, out)
	}

	// 3) Parse the JSON output into a single transcript string.
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return "", fmt.Errorf("read whisper json: %w", err)
	}
	var wj whisperJSON
	if err := json.Unmarshal(data, &wj); err != nil {
		return "", fmt.Errorf("parse whisper json: %w", err)
	}
	var b strings.Builder
	for _, seg := range wj.Transcription {
		b.WriteString(strings.TrimSpace(seg.Text))
		b.WriteByte(' ')
	}
	transcript := strings.Join(strings.Fields(b.String()), " ")
	if transcript == "" {
		return "", fmt.Errorf("empty transcript for %s", videoID)
	}
	return transcript, nil
}

// run executes a command and returns trimmed combined output (for diagnostics
// on failure).
func run(ctx context.Context, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH=/usr/local/lib")
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if len(s) > 2000 {
		s = s[len(s)-2000:]
	}
	return s, err
}
