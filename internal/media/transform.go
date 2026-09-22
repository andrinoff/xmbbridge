package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/disintegration/imaging"

	// Registers the WebP decoder with image.Decode.
	_ "golang.org/x/image/webp"
)

// ErrCannotFit reports that an attachment could not be compressed enough to
// satisfy a platform's limits. Callers drop the attachment and keep the text.
var ErrCannotFit = errors.New("media cannot be compressed to fit platform limits")

// Result is a re-encoded attachment ready to upload.
type Result struct {
	Bytes    []byte
	MimeType string
	Width    int
	Height   int
	Filename string
}

// ImageOptions bounds what an image is allowed to become.
type ImageOptions struct {
	MaxBytes     int
	MaxDimension int
	AltText      string
	BaseName     string
}

// VideoOptions bounds what a video is allowed to become.
type VideoOptions struct {
	MaxBytes     int
	MaxDuration  time.Duration
	MaxDimension int
	FFmpegPath   string
	Timeout      time.Duration
	AltText      string
	BaseName     string
}

// jpegQualities are tried in order until an image fits its byte budget.
var jpegQualities = []int{85, 75, 60, 45}

// FitImage returns the original bytes when they already satisfy the limits.
// Otherwise the image is downscaled and re-encoded as JPEG, progressively
// shrinking until it fits or the attempts are exhausted.
func FitImage(data []byte, opts ImageOptions) (Result, error) {
	if len(data) == 0 {
		return Result{}, fmt.Errorf("image is empty")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Result{}, fmt.Errorf("decode image header: %w", err)
	}
	width, height := cfg.Width, cfg.Height

	withinBytes := opts.MaxBytes <= 0 || len(data) <= opts.MaxBytes
	withinDimensions := opts.MaxDimension <= 0 || max(width, height) <= opts.MaxDimension
	if withinBytes && withinDimensions {
		return Result{
			Bytes:    data,
			MimeType: mimeForFormat(format),
			Width:    width,
			Height:   height,
			Filename: filenameFor(opts.BaseName, extForFormat(format)),
		}, nil
	}

	// Animated GIFs cannot be resized without losing the animation, so a GIF
	// that is over budget is passed through unchanged and allowed to fail at
	// upload rather than silently becoming a still frame.
	if format == "gif" && withinDimensions && !withinBytes {
		return Result{}, fmt.Errorf("%w: animated gif is %d bytes", ErrCannotFit, len(data))
	}

	img, err := imaging.Decode(bytes.NewReader(data), imaging.AutoOrientation(true))
	if err != nil {
		return Result{}, fmt.Errorf("decode image: %w", err)
	}

	dimensions := dimensionPlan(opts.MaxDimension, max(width, height))
	for _, dimension := range dimensions {
		frame := img
		if dimension > 0 && max(width, height) > dimension {
			frame = imaging.Fit(img, dimension, dimension, imaging.Lanczos)
		}
		bounds := frame.Bounds()
		for _, quality := range jpegQualities {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, frame, &jpeg.Options{Quality: quality}); err != nil {
				return Result{}, fmt.Errorf("encode jpeg: %w", err)
			}
			if opts.MaxBytes > 0 && buf.Len() > opts.MaxBytes {
				continue
			}
			return Result{
				Bytes:    buf.Bytes(),
				MimeType: "image/jpeg",
				Width:    bounds.Dx(),
				Height:   bounds.Dy(),
				Filename: filenameFor(opts.BaseName, "jpg"),
			}, nil
		}
	}
	return Result{}, fmt.Errorf("%w: image is %d bytes, budget is %d", ErrCannotFit, len(data), opts.MaxBytes)
}

// dimensionPlan lists the maximum long edges to try, largest first.
func dimensionPlan(maxDimension, current int) []int {
	if maxDimension <= 0 {
		return []int{0}
	}
	plan := []int{maxDimension, maxDimension * 3 / 4, maxDimension / 2, maxDimension / 3}
	out := plan[:0]
	for _, d := range plan {
		if d >= 320 && d < current {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		out = append(out, min(current, maxDimension))
	}
	return out
}

// TranscodeVideo re-encodes a video to H.264/AAC MP4, capping duration and
// resolution. It retries at lower bitrates until the result fits the byte
// budget. It requires an ffmpeg binary on the host.
func TranscodeVideo(ctx context.Context, data []byte, opts VideoOptions) (Result, error) {
	if len(data) == 0 {
		return Result{}, fmt.Errorf("video is empty")
	}
	if opts.FFmpegPath == "" {
		opts.FFmpegPath = "ffmpeg"
	}
	if opts.MaxDimension <= 0 {
		opts.MaxDimension = 1280
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Minute
	}
	if _, err := exec.LookPath(opts.FFmpegPath); err != nil {
		return Result{}, fmt.Errorf("ffmpeg not available at %q: %w", opts.FFmpegPath, err)
	}

	dir, err := os.MkdirTemp("", "xmbbridge-media-")
	if err != nil {
		return Result{}, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	inPath := filepath.Join(dir, "input")
	if err := os.WriteFile(inPath, data, 0o600); err != nil {
		return Result{}, fmt.Errorf("write video input: %w", err)
	}

	attempts := []struct {
		crf       int
		dimension int
	}{
		{23, opts.MaxDimension},
		{28, opts.MaxDimension * 2 / 3},
		{32, opts.MaxDimension / 2},
	}
	var lastErr error
	for _, attempt := range attempts {
		if attempt.dimension < 320 {
			continue
		}
		outPath := filepath.Join(dir, fmt.Sprintf("attempt-%d.mp4", attempt.crf))
		if err := runFFmpeg(ctx, opts, inPath, outPath, attempt.crf, attempt.dimension); err != nil {
			lastErr = err
			continue
		}
		out, err := os.ReadFile(outPath)
		if err != nil {
			lastErr = fmt.Errorf("read transcoded video: %w", err)
			continue
		}
		if opts.MaxBytes > 0 && len(out) > opts.MaxBytes {
			lastErr = fmt.Errorf("%w: video is %d bytes, budget is %d", ErrCannotFit, len(out), opts.MaxBytes)
			continue
		}
		return Result{
			Bytes:    out,
			MimeType: "video/mp4",
			Width:    attempt.dimension,
			Height:   attempt.dimension * 9 / 16,
			Filename: filenameFor(opts.BaseName, "mp4"),
		}, nil
	}
	if lastErr == nil {
		lastErr = ErrCannotFit
	}
	return Result{}, lastErr
}

func runFFmpeg(ctx context.Context, opts VideoOptions, inPath, outPath string, crf, dimension int) error {
	// Two scale filters in sequence: cap the long edge, then force even
	// dimensions because H.264 with 4:2:0 chroma cannot encode odd sizes.
	filter := fmt.Sprintf(
		"scale='min(%d,iw)':-2,scale=trunc(iw/2)*2:trunc(ih/2)*2", dimension)

	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", inPath,
	}
	if opts.MaxDuration > 0 {
		args = append(args, "-t", strconv.FormatFloat(opts.MaxDuration.Seconds(), 'f', 3, 64))
	}
	args = append(args,
		"-vf", filter,
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-crf", strconv.Itoa(crf),
		"-pix_fmt", "yuv420p",
		"-c:a", "aac",
		"-b:a", "128k",
		"-movflags", "+faststart",
		outPath,
	)

	runCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, opts.FFmpegPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func mimeForFormat(format string) string {
	switch strings.ToLower(format) {
	case "jpeg", "jpg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "tiff":
		return "image/tiff"
	case "bmp":
		return "image/bmp"
	default:
		if format == "" {
			return "application/octet-stream"
		}
		return "image/" + strings.ToLower(format)
	}
}

func extForFormat(format string) string {
	switch strings.ToLower(format) {
	case "jpeg":
		return "jpg"
	case "":
		return "bin"
	default:
		return strings.ToLower(format)
	}
}

func filenameFor(base, ext string) string {
	if base == "" {
		base = "media"
	}
	base = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, base)
	return base + "." + ext
}

// ExtensionForMime returns a plausible file extension for a MIME type.
func ExtensionForMime(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	case "video/mp4":
		return "mp4"
	case "video/quicktime":
		return "mov"
	case "video/webm":
		return "webm"
	default:
		return "bin"
	}
}
