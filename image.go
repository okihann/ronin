package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// imageSummary is a compact, text-only description of an image. The chat
// transport is text, so this is the cheap way to let the model reason
// about pixels without spending a turn budget on raw hex.
type imageSummary struct {
	Path       string         `json:"path"`
	Width      int            `json:"width"`
	Height     int            `json:"height"`
	Format     string         `json:"format"`
	Bytes      int64          `json:"bytes"`
	Brightness brightnessStat `json:"brightness"`
	Palette    []colorBucket  `json:"palette"`
	Preview    string         `json:"preview,omitempty"`
}

type brightnessStat struct {
	Min    int `json:"min"`
	Median int `json:"median"`
	Mean   int `json:"mean"`
	Max    int `json:"max"`
}

type colorBucket struct {
	Hex     string  `json:"hex"`
	Percent float64 `json:"percent"`
}

func runImage(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ronin image inspect <file> [--preview W] [--json] [--ocr]")
		return 2
	}
	switch args[0] {
	case "inspect":
		return imageInspect(args[1:])
	default:
		fmt.Fprintln(os.Stderr, "unknown image subcommand:", args[0])
		return 2
	}
}

func imageInspect(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ronin image inspect <file> [--preview W] [--json] [--ocr]")
		return 2
	}
	path := args[0]
	previewW := 0
	asJSON := false
	doOCR := false
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--preview":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &previewW)
				i++
			}
		case "--json":
			asJSON = true
		case "--ocr":
			doOCR = true
		}
	}
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		return 1
	}
	defer f.Close()
	st, _ := f.Stat()

	img, format, err := image.Decode(f)
	if err != nil {
		// WebP and other formats the stdlib cannot decode: fall back to ffmpeg.
		img, format, err = decodeViaFFmpeg(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "decode:", err)
			return 1
		}
	}

	s := summarise(img, format)
	s.Path = path
	if s.Width <= 0 || s.Height <= 0 {
		fmt.Fprintln(os.Stderr, "decode produced an empty image")
		return 1
	}
	if st != nil {
		s.Bytes = st.Size()
	}
	if previewW > 0 {
		s.Preview = asciiPreview(img, previewW)
	}

	if asJSON {
		printJSON(s)
	} else {
		printImageSummary(s)
	}
	if doOCR {
		if txt, err := ocrImage(path); err == nil {
			fmt.Println("\n--- ocr ---")
			fmt.Println(strings.TrimSpace(txt))
		} else {
			fmt.Fprintln(os.Stderr, "ocr:", err)
		}
	}
	return 0
}

func summarise(img image.Image, format string) imageSummary {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	s := imageSummary{Width: w, Height: h, Format: format}

	// Sample on a grid so huge images stay fast; every pixel of a 600x449
	// is fine, but a 6000x4000 should not be walked at full resolution.
	step := 1
	for (w/step)*(h/step) > 250000 {
		step++
	}
	hist := map[uint32]int{}
	var lums []int
	var sum int
	for y := b.Min.Y; y < b.Max.Y; y += step {
		for x := b.Min.X; x < b.Max.X; x += step {
			r, g, bl, _ := img.At(x, y).RGBA()
			r8, g8, b8 := int(r>>8), int(g>>8), int(bl>>8)
			l := (r8*299 + g8*587 + b8*114) / 1000
			lums = append(lums, l)
			sum += l
			key := uint32(r8>>4)<<8 | uint32(g8>>4)<<4 | uint32(b8>>4)
			hist[key]++
		}
	}
	if len(lums) == 0 {
		return s
	}
	sort.Ints(lums)
	s.Brightness = brightnessStat{
		Min:    lums[0],
		Median: lums[len(lums)/2],
		Mean:   sum / len(lums),
		Max:    lums[len(lums)-1],
	}
	total := len(lums)
	type kv struct {
		k uint32
		n int
	}
	var pairs []kv
	for k, n := range hist {
		pairs = append(pairs, kv{k, n})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].n > pairs[j].n })
	for i := 0; i < len(pairs) && i < 6; i++ {
		k := pairs[i].k
		r := int((k>>8)&0xF)*16 + 8
		g := int((k>>4)&0xF)*16 + 8
		bl := int(k&0xF)*16 + 8
		s.Palette = append(s.Palette, colorBucket{
			Hex:     fmt.Sprintf("#%02x%02x%02x", r, g, bl),
			Percent: float64(pairs[i].n) * 100 / float64(total),
		})
	}
	return s
}

func printImageSummary(s imageSummary) {
	fmt.Printf("%s\n", s.Path)
	fmt.Printf("  %dx%d %s, %d bytes\n", s.Width, s.Height, s.Format, s.Bytes)
	fmt.Printf("  brightness: min=%d median=%d mean=%d max=%d\n",
		s.Brightness.Min, s.Brightness.Median, s.Brightness.Mean, s.Brightness.Max)
	fmt.Printf("  palette:")
	for _, p := range s.Palette {
		fmt.Printf(" %s(%.1f%%)", p.Hex, p.Percent)
	}
	fmt.Println()
	if s.Preview != "" {
		fmt.Println()
		fmt.Println(s.Preview)
	}
}

// asciiPreview downsamples to a luminance ramp. Aspect is corrected for
// character cells being roughly twice as tall as they are wide.
func asciiPreview(img image.Image, w int) string {
	if w < 8 {
		w = 8
	}
	if w > 160 {
		w = 160
	}
	b := img.Bounds()
	ar := float64(b.Dy()) / float64(b.Dx())
	h := int(float64(w) * ar * 0.5)
	if h < 1 {
		h = 1
	}
	ramp := []byte(" .:-=+*#%@")
	var sb strings.Builder
	for row := 0; row < h; row++ {
		for col := 0; col < w; col++ {
			x := b.Min.X + col*b.Dx()/w
			y := b.Min.Y + row*b.Dy()/h
			r, g, bl, _ := img.At(x, y).RGBA()
			l := (int(r>>8)*299 + int(g>>8)*587 + int(bl>>8)*114) / 1000
			idx := l * (len(ramp) - 1) / 255
			sb.WriteByte(ramp[idx])
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// decodeViaFFmpeg handles formats the stdlib lacks (webp, avif). It asks
// ffmpeg for raw RGBA at the original size.
func decodeViaFFmpeg(path string) (image.Image, string, error) {
	probe, err := exec.Command("ffprobe", "-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height,codec_name",
		"-of", "json", path).Output()
	if err != nil {
		return nil, "", err
	}
	var info struct {
		Streams []struct {
			Width     int    `json:"width"`
			Height    int    `json:"height"`
			CodecName string `json:"codec_name"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(probe, &info); err != nil {
		return nil, "", fmt.Errorf("ffprobe json: %w", err)
	}
	if len(info.Streams) == 0 {
		return nil, "", fmt.Errorf("no image stream found")
	}
	st := info.Streams[0]
	if st.Width <= 0 || st.Height <= 0 {
		return nil, "", fmt.Errorf("ffprobe reported bad dimensions %dx%d", st.Width, st.Height)
	}
	raw, err := exec.Command("ffmpeg", "-v", "error", "-i", path,
		"-frames:v", "1", "-f", "rawvideo", "-pix_fmt", "rgba", "-").Output()
	if err != nil {
		return nil, "", err
	}
	want := st.Width * st.Height * 4
	if len(raw) < want {
		return nil, "", fmt.Errorf("ffmpeg short read: got %d bytes, want %d", len(raw), want)
	}
	img := image.NewNRGBA(image.Rect(0, 0, st.Width, st.Height))
	copy(img.Pix, raw[:want])
	return img, st.CodecName, nil
}

func ocrImage(path string) (string, error) {
	bin, err := exec.LookPath("tesseract")
	if err != nil {
		return "", fmt.Errorf("tesseract not installed")
	}
	out, err := exec.Command(bin, path, "-", "--psm", "6").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func printJSON(v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
	fmt.Print(buf.String())
}
