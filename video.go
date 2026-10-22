package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// videoMeta is the subset of ffprobe output we surface to the model.
type videoMeta struct {
	Path       string  `json:"path"`
	DurationS  float64 `json:"duration_s"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	VideoCodec string  `json:"video_codec"`
	HasAudio   bool    `json:"has_audio"`
	SheetPath  string  `json:"sheet"`
	AudioPath  string  `json:"audio"`
	Transcript string  `json:"transcript,omitempty"`
	OutDir     string  `json:"out_dir"`
}

func runVideo(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ronin video analyze <file> [--out DIR] [--fps RATE] [--tile WxH]")
		return 2
	}
	switch args[0] {
	case "analyze":
		return videoAnalyze(args[1:])
	default:
		fmt.Fprintln(os.Stderr, "unknown video subcommand:", args[0])
		return 2
	}
}

func videoAnalyze(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ronin video analyze <file> [--out DIR] [--fps RATE] [--tile WxH]")
		return 2
	}
	src := args[0]
	outDir := ""
	fps := "1/15" // one frame every 15s
	tile := "4x3"
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--out":
			if i+1 < len(args) {
				outDir = args[i+1]
				i++
			}
		case "--fps":
			if i+1 < len(args) {
				fps = args[i+1]
				i++
			}
		case "--tile":
			if i+1 < len(args) {
				tile = args[i+1]
				i++
			}
		}
	}
	abs, err := filepath.Abs(src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad path:", err)
		return 2
	}
	if _, err := os.Stat(abs); err != nil {
		fmt.Fprintln(os.Stderr, "cannot read:", abs, err)
		return 2
	}
	if outDir == "" {
		outDir = filepath.Join(os.TempDir(), "ronin-video-"+sanitizeBase(abs))
	}
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "mkdir:", err)
		return 1
	}

	meta, err := probeVideo(abs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ffprobe:", err)
		return 1
	}
	meta.OutDir = outDir

	sheet := filepath.Join(outDir, "contact-sheet.png")
	// fps=1/15 then tile=4x3 packs 12 frames per image, scaling each to 320 wide.
	vf := fmt.Sprintf("fps=%s,scale=320:-1,tile=%s", fps, tile)
	cmd := exec.Command("ffmpeg", "-y", "-loglevel", "error", "-i", abs, "-vf", vf, "-frames:v", "1", sheet)
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "ffmpeg tile failed: %v\n%s\n", err, lastLines(string(out), 20))
	} else {
		meta.SheetPath = sheet
	}

	if meta.HasAudio {
		wav := filepath.Join(outDir, "audio.wav")
		cmd := exec.Command("ffmpeg", "-y", "-loglevel", "error", "-i", abs, "-vn", "-ac", "1", "-ar", "16000", wav)
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "ffmpeg audio failed: %v\n%s\n", err, lastLines(string(out), 20))
		} else {
			meta.AudioPath = wav
			meta.Transcript = tryTranscribe(wav, outDir)
		}
	}

	manPath := filepath.Join(outDir, "manifest.json")
	if b, err := json.MarshalIndent(meta, "", "  "); err == nil {
		_ = os.WriteFile(manPath, b, 0o600)
	}

	fmt.Printf("video: %s\n", meta.Path)
	fmt.Printf("duration: %.1fs  %dx%d  codec=%s  audio=%v\n", meta.DurationS, meta.Width, meta.Height, meta.VideoCodec, meta.HasAudio)
	if meta.SheetPath != "" {
		fmt.Printf("contact sheet: %s\n", meta.SheetPath)
	}
	if meta.AudioPath != "" {
		fmt.Printf("audio: %s\n", meta.AudioPath)
	}
	if meta.Transcript != "" {
		fmt.Printf("transcript: %s\n", meta.Transcript)
	}
	fmt.Printf("manifest: %s\n", manPath)
	return 0
}

func probeVideo(path string) (videoMeta, error) {
	var m videoMeta
	m.Path = path
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "format=duration:stream=codec_type,codec_name,width,height",
		"-of", "json", path).Output()
	if err != nil {
		return m, err
	}
	var raw struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return m, err
	}
	if d, err := strconv.ParseFloat(raw.Format.Duration, 64); err == nil {
		m.DurationS = d
	}
	for _, s := range raw.Streams {
		switch s.CodecType {
		case "video":
			if m.VideoCodec == "" {
				m.VideoCodec = s.CodecName
				m.Width = s.Width
				m.Height = s.Height
			}
		case "audio":
			m.HasAudio = true
		}
	}
	return m, nil
}

// tryTranscribe runs whisper if it happens to be installed; otherwise it
// returns empty and the caller just skips the transcript.
func tryTranscribe(wav, outDir string) string {
	bin, err := exec.LookPath("whisper")
	if err != nil {
		return ""
	}
	txt := filepath.Join(outDir, "transcript.txt")
	cmd := exec.Command(bin, wav, "--output_format", "txt", "--output_dir", outDir)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return ""
	}
	if b, err := os.ReadFile(txt); err == nil {
		return txt + "\n" + strings.TrimSpace(string(b))
	}
	return txt
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func sanitizeBase(p string) string {
	b := filepath.Base(p)
	b = strings.TrimSuffix(b, filepath.Ext(b))
	var sb strings.Builder
	for _, r := range b {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('_')
		}
	}
	return sb.String()
}
