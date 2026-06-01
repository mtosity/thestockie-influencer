// Package config loads runtime configuration from environment variables (with
// an optional .env file) and the influencer list from a JSON file.
package config

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mtosity/thestockie-influencer/internal/models"
)

type Config struct {
	// Convex
	ConvexSiteURL string // e.g. https://exciting-bee-603.convex.site
	IngestSecret  string

	// Ollama Cloud
	OllamaHost   string
	OllamaAPIKey string
	OllamaModel  string

	// Transcription toolchain
	WhisperBin     string
	WhisperModel   string // path to ggml-large-v3-turbo.bin
	WhisperThreads int
	WhisperLang    string // "auto" or ISO code
	YtDlpBin       string
	FfmpegBin      string // optional; passed to yt-dlp --ffmpeg-location if set

	// YouTube auth to beat "confirm you're not a bot" (one or the other):
	CookiesFromBrowser string // e.g. "chrome", "safari", "firefox"
	CookiesFile        string // path to an exported cookies.txt

	// Behaviour
	WorkDir             string
	WindowDays          int
	MaxVideosPerChannel int
	MaxVideoAgeDays     int
	RetentionDays       int // drop videos/mentions older than this each run
	KeepAudio           bool

	InfluencersFile string
}

func getenv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getenvBool(key string, def bool) bool {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
	}
	return def
}

// LoadDotEnv reads KEY=VALUE lines from path and sets them into the process
// environment unless already set. Missing file is not an error.
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		// Strip matching surrounding quotes.
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') ||
				(val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
	return sc.Err()
}

// Load builds a Config from the environment and applies defaults.
func Load() *Config {
	return &Config{
		ConvexSiteURL: strings.TrimRight(getenv("CONVEX_SITE_URL", ""), "/"),
		IngestSecret:  getenv("INGEST_SECRET", ""),

		OllamaHost:   strings.TrimRight(getenv("OLLAMA_HOST", "https://ollama.com"), "/"),
		OllamaAPIKey: getenv("OLLAMA_API_KEY", ""),
		OllamaModel:  getenv("OLLAMA_MODEL", "gpt-oss:120b"),

		WhisperBin:     getenv("WHISPER_BIN", "whisper-cli"),
		WhisperModel:   getenv("WHISPER_MODEL", "models/ggml-large-v3-turbo.bin"),
		WhisperThreads: getenvInt("WHISPER_THREADS", 8),
		WhisperLang:    getenv("WHISPER_LANG", "auto"),
		YtDlpBin:       getenv("YTDLP_BIN", "yt-dlp"),
		FfmpegBin:      getenv("FFMPEG_BIN", ""),

		CookiesFromBrowser: getenv("YTDLP_COOKIES_FROM_BROWSER", ""),
		CookiesFile:        getenv("YTDLP_COOKIES_FILE", ""),

		WorkDir:             getenv("WORK_DIR", filepath.Join(os.TempDir(), "thestockie-influencer")),
		WindowDays:          getenvInt("WINDOW_DAYS", 7),
		MaxVideosPerChannel: getenvInt("MAX_VIDEOS_PER_CHANNEL", 5),
		MaxVideoAgeDays:     getenvInt("MAX_VIDEO_AGE_DAYS", 14),
		RetentionDays:       getenvInt("RETENTION_DAYS", 60),
		KeepAudio:           getenvBool("KEEP_AUDIO", false),

		InfluencersFile: getenv("INFLUENCERS_FILE", "config/influencers.json"),
	}
}

// Validate checks that the fields required for a real (non-dry) run are set.
// transcribe controls whether the Whisper model path is required.
func (c *Config) Validate(needLLM, needTranscribe bool) error {
	var missing []string
	if c.ConvexSiteURL == "" {
		missing = append(missing, "CONVEX_SITE_URL")
	}
	if c.IngestSecret == "" {
		missing = append(missing, "INGEST_SECRET")
	}
	if needLLM && c.OllamaAPIKey == "" {
		missing = append(missing, "OLLAMA_API_KEY")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required env: %s", strings.Join(missing, ", "))
	}
	if needTranscribe {
		if _, err := os.Stat(c.WhisperModel); err != nil {
			return fmt.Errorf("whisper model not found at %q (set WHISPER_MODEL): %w", c.WhisperModel, err)
		}
	}
	return nil
}

// LoadInfluencers reads the influencer list from a JSON file. A missing file is
// not an error (returns nil) — the job then relies on whatever is already
// registered in Convex.
func LoadInfluencers(path string) ([]models.Influencer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var list []models.Influencer
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return list, nil
}
