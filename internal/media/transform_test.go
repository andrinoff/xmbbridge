package media

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os/exec"
	"testing"
	"time"

	"github.com/disintegration/imaging"
)

// bigPNG renders a noisy image large enough that it must be compressed.
func bigPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			// Pseudo-random colours resist compression.
			img.Set(x, y, color.RGBA{
				R: uint8((x * 7) % 256),
				G: uint8((y * 13) % 256),
				B: uint8((x*y + 31) % 256),
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func TestFitImagePassesThroughSmallImages(t *testing.T) {
	img := imaging.New(64, 48, color.RGBA{R: 10, G: 200, B: 90, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
	original := buf.Bytes()

	result, err := FitImage(original, ImageOptions{MaxBytes: 1 << 20, MaxDimension: 2000, BaseName: "small"})
	if err != nil {
		t.Fatalf("FitImage: %v", err)
	}
	// An image that already fits is not touched, which preserves PNG
	// transparency and avoids needless recompression.
	if !bytes.Equal(result.Bytes, original) {
		t.Fatal("a small image should be passed through unchanged")
	}
	if result.MimeType != "image/png" {
		t.Fatalf("mime = %q, want image/png", result.MimeType)
	}
	if result.Width != 64 || result.Height != 48 {
		t.Fatalf("dimensions = %dx%d, want 64x48", result.Width, result.Height)
	}
	if result.Filename != "small.png" {
		t.Fatalf("filename = %q", result.Filename)
	}
}

func TestFitImageCompressesToByteBudget(t *testing.T) {
	original := bigPNG(t, 1200, 900)
	const budget = 120 << 10
	if len(original) <= budget {
		t.Fatalf("test fixture is too small (%d bytes) to exercise compression", len(original))
	}

	result, err := FitImage(original, ImageOptions{
		MaxBytes:     budget,
		MaxDimension: 800,
		BaseName:     "big",
	})
	if err != nil {
		t.Fatalf("FitImage: %v", err)
	}
	if len(result.Bytes) > budget {
		t.Fatalf("result is %d bytes, budget is %d", len(result.Bytes), budget)
	}
	if result.MimeType != "image/jpeg" {
		t.Fatalf("mime = %q, want image/jpeg", result.MimeType)
	}
	if max(result.Width, result.Height) > 800 {
		t.Fatalf("result is %dx%d, exceeding the dimension cap", result.Width, result.Height)
	}
	if result.Filename != "big.jpg" {
		t.Fatalf("filename = %q", result.Filename)
	}

	// The output must be a decodable image, not just a small blob.
	cfg, format, err := image.DecodeConfig(bytes.NewReader(result.Bytes))
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if format != "jpeg" {
		t.Fatalf("format = %q, want jpeg", format)
	}
	if cfg.Width != result.Width || cfg.Height != result.Height {
		t.Fatalf("reported size %dx%d does not match the encoded image %dx%d",
			result.Width, result.Height, cfg.Width, cfg.Height)
	}
}

func TestFitImageRejectsImpossibleBudget(t *testing.T) {
	original := bigPNG(t, 400, 400)
	_, err := FitImage(original, ImageOptions{MaxBytes: 512, MaxDimension: 400, BaseName: "tiny"})
	if err == nil {
		t.Fatal("expected an error when the budget cannot be met")
	}
}

func TestFitImageRejectsGarbage(t *testing.T) {
	if _, err := FitImage([]byte("not an image"), ImageOptions{MaxBytes: 1000}); err == nil {
		t.Fatal("expected an error for undecodable input")
	}
	if _, err := FitImage(nil, ImageOptions{MaxBytes: 1000}); err == nil {
		t.Fatal("expected an error for empty input")
	}
}

func TestTranscodeVideo(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}

	// Build a two second test clip to transcode.
	source := generateTestClip(t, ffmpeg)
	if len(source) == 0 {
		t.Fatal("fixture clip is empty")
	}

	result, err := TranscodeVideo(context.Background(), source, VideoOptions{
		MaxBytes:     5 << 20,
		MaxDuration:  1 * time.Second,
		MaxDimension: 320,
		FFmpegPath:   ffmpeg,
		BaseName:     "clip",
	})
	if err != nil {
		t.Fatalf("TranscodeVideo: %v", err)
	}
	if len(result.Bytes) > 5<<20 {
		t.Fatalf("result is %d bytes, over budget", len(result.Bytes))
	}
	if result.MimeType != "video/mp4" {
		t.Fatalf("mime = %q, want video/mp4", result.MimeType)
	}
	if result.Filename != "clip.mp4" {
		t.Fatalf("filename = %q", result.Filename)
	}
	// The MP4 container signature must be present in the output.
	if !bytes.Contains(result.Bytes[:64], []byte("ftyp")) {
		t.Fatal("output does not look like an MP4 container")
	}
}

func generateTestClip(t *testing.T, ffmpeg string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, ffmpeg,
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=640x360:rate=15:duration=2",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-f", "mp4", "pipe:1",
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Skipf("could not generate a test clip (ffmpeg unavailable or incomplete): %v", err)
	}
	return out.Bytes()
}

func TestExtensionForMime(t *testing.T) {
	tests := map[string]string{
		"image/jpeg":  "jpg",
		"image/JPEG":  "jpg",
		"image/png":   "png",
		"video/mp4":   "mp4",
		"video/webm":  "webm",
		"image/gif":   "gif",
		"image/webp":  "webp",
		"":            "bin",
		"application": "bin",
	}
	for mime, want := range tests {
		if got := ExtensionForMime(mime); got != want {
			t.Errorf("ExtensionForMime(%q) = %q, want %q", mime, got, want)
		}
	}
}

func TestJPEGRoundTripSanity(t *testing.T) {
	// Guards against a bad quality constant making every image unreadable.
	img := imaging.New(32, 32, color.RGBA{R: 255, G: 0, B: 0, A: 255})
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQualities[0]}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, _, err := image.Decode(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("decode: %v", err)
	}
}
