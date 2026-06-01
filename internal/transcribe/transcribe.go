// Package transcribe turns a YouTube video into text: yt-dlp pulls the audio,
// ffmpeg (via yt-dlp) downsamples to 16 kHz mono, and whisper.cpp transcribes.
// For long videos, audio is split into chunks to avoid OOM.
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
	WhisperModel       string // default model (large-v3-turbo)
	WhisperModelMedium string // fallback for long videos (>15min)
	LongVideoThreshold int    // seconds; default 15*60=900
	ChunkDuration      int    // seconds per chunk; default 10*60=600
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

	// Rate-limit: pause briefly before each download to avoid triggering
	// YouTube's bot detection on shared/cloud IPs.
	time.Sleep(2 * time.Second)

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

	// 1b) Detect duration — choose model and chunking strategy.
	model := t.WhisperModel
	dur, err := audioDuration(wav)
	if err != nil {
		t.Log.Warn("could not detect audio duration", "err", err)
	} else {
		if t.WhisperModelMedium != "" && t.LongVideoThreshold > 0 && dur > t.LongVideoThreshold {
			model = t.WhisperModelMedium
			t.Log.Info("using medium model for long video", "videoId", videoID,
				"duration", fmt.Sprintf("%dm%ds", dur/60, dur%60),
				"threshold", fmt.Sprintf("%dm", t.LongVideoThreshold/60))
		}
		if dur > 0 {
			t.Log.Info("audio duration", "videoId", videoID,
				"duration", fmt.Sprintf("%dm%ds", dur/60, dur%60))
		}
	}

	// 2) Transcribe — chunk if very long to avoid OOM.
	chunkSize := t.ChunkDuration
	if chunkSize <= 0 {
		chunkSize = 600 // 10 minutes default
	}

	if dur > 0 && dur > chunkSize {
		return t.transcribeChunks(ctx, videoID, wav, model, dur, chunkSize)
	}
	return t.transcribeSingle(ctx, videoID, wav, model, outBase)
}

// transcribeSingle runs whisper on the full file (for short videos).
func (t *Transcriber) transcribeSingle(ctx context.Context, videoID, wav, model, outBase string) (string, error) {
	wArgs := []string{
		"-m", model,
		"-f", wav,
		"-l", t.Lang,
		"-t", strconv.Itoa(t.Threads),
		"-oj", "-of", outBase,
		"-np",
	}
	if out, err := run(ctx, t.WhisperBin, wArgs...); err != nil {
		return "", fmt.Errorf("whisper: %w: %s", err, out)
	}
	return parseWhisperJSON(outBase + ".json")
}

// transcribeChunks splits long audio into N-minute chunks, transcribes each,
// and concatenates the results.
func (t *Transcriber) transcribeChunks(ctx context.Context, videoID, wav, model string, dur, chunkSize int) (string, error) {
	chunkCount := (dur + chunkSize - 1) / chunkSize
	t.Log.Info("chunking long video", "videoId", videoID,
		"chunks", chunkCount, "chunkDuration", fmt.Sprintf("%dm", chunkSize/60))

	var parts []string
	for i := 0; i < chunkCount; i++ {
		offset := i * chunkSize * 1000 // milliseconds
		length := chunkSize * 1000
		if i == chunkCount-1 {
			length = (dur - i*chunkSize) * 1000
		}
		chunkBase := filepath.Join(t.WorkDir, fmt.Sprintf("%s_chunk%d", videoID, i))
		chunkJSON := chunkBase + ".json"
		if !t.KeepAudio {
			defer os.Remove(chunkJSON)
		}

		wArgs := []string{
			"-m", model,
			"-f", wav,
			"-l", t.Lang,
			"-t", strconv.Itoa(t.Threads),
			"-ot", strconv.Itoa(offset),
			"-d", strconv.Itoa(length),
			"-oj", "-of", chunkBase,
			"-np",
		}
		t.Log.Debug("transcribing chunk", "videoId", videoID, "chunk", i+1, "offset", fmt.Sprintf("%dm", offset/60000))
		if out, err := run(ctx, t.WhisperBin, wArgs...); err != nil {
			return "", fmt.Errorf("whisper chunk %d: %w: %s", i, err, out)
		}
		text, err := parseWhisperJSON(chunkJSON)
		if err != nil {
			return "", fmt.Errorf("chunk %d parse: %w", i, err)
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, " "), nil
}

// parseWhisperJSON reads a whisper output file and returns the concatenated text.
func parseWhisperJSON(path string) (string, error) {
	data, err := os.ReadFile(path)
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
		return "", fmt.Errorf("empty transcript")
	}
	return transcript, nil
}

// audioDuration returns the duration in seconds of a WAV file using ffprobe.
func audioDuration(path string) (int, error) {
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", path).Output()
	if err != nil {
		return 0, err
	}
	sec, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0, err
	}
	return int(sec), nil
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
