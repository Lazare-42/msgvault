package ocr

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.kenn.io/msgvault/internal/store"
)

type ExecutorConfig struct {
	Socket        string
	Languages     string
	DPI           int
	MinImageSide  int
	MaxImageScale int
	Timeout       time.Duration
	Limits        Limits
	TempDir       string
	PDFInfo       string
	PDFToText     string
	PDFToPPM      string
	Tesseract     string
}

func (c *ExecutorConfig) defaults() {
	if c.Languages == "" {
		c.Languages = DefaultLanguages
	}
	if c.DPI <= 0 {
		c.DPI = DefaultDPI
	}
	if c.MinImageSide <= 0 {
		c.MinImageSide = DefaultMinImageSide
	}
	if c.MaxImageScale <= 0 {
		c.MaxImageScale = DefaultMaxImageScale
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultRequestTimeout
	}
	ApplyLimitDefaults(&c.Limits)
	if c.PDFInfo == "" {
		c.PDFInfo = "pdfinfo"
	}
	if c.PDFToText == "" {
		c.PDFToText = "pdftotext"
	}
	if c.PDFToPPM == "" {
		c.PDFToPPM = "pdftoppm"
	}
	if c.Tesseract == "" {
		c.Tesseract = "tesseract"
	}
}

// ServeExecutor starts a stateless, single-concurrency extraction service.
// Request bodies are attachment bytes; this process has no archive path or DB.
func ServeExecutor(ctx context.Context, cfg ExecutorConfig) error {
	cfg.defaults()
	if !filepath.IsAbs(cfg.Socket) {
		return errors.New("OCR socket must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Socket), 0o700); err != nil {
		return fmt.Errorf("create OCR socket dir: %w", err)
	}
	if info, err := os.Lstat(cfg.Socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("refusing to replace non-socket %s", cfg.Socket)
		}
		if err := os.Remove(cfg.Socket); err != nil {
			return fmt.Errorf("remove stale OCR socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect OCR socket: %w", err)
	}
	ln, err := net.Listen("unix", cfg.Socket)
	if err != nil {
		return fmt.Errorf("listen OCR socket: %w", err)
	}
	defer func() { _ = ln.Close(); _ = os.Remove(cfg.Socket) }()
	if err := os.Chmod(cfg.Socket, 0o600); err != nil {
		return fmt.Errorf("protect OCR socket: %w", err)
	}
	sem := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /extract", func(w http.ResponseWriter, r *http.Request) {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-r.Context().Done():
			return
		}
		resp := extractRequest(r, cfg)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func extractRequest(r *http.Request, cfg ExecutorConfig) Response {
	mimeType := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	filename := r.Header.Get("X-Filename")
	if r.ContentLength > cfg.Limits.MaxFileBytes {
		return failure("too_large", "attachment exceeds byte limit", true)
	}
	dir, err := os.MkdirTemp(cfg.TempDir, "msgvault-ocr-")
	if err != nil {
		return failure("temporary_storage", err.Error(), false)
	}
	defer os.RemoveAll(dir)
	input := filepath.Join(dir, "input")
	f, err := os.OpenFile(input, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return failure("temporary_storage", err.Error(), false)
	}
	n, copyErr := io.Copy(f, io.LimitReader(r.Body, cfg.Limits.MaxFileBytes+1))
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		return failure("read_input", errors.Join(copyErr, closeErr).Error(), false)
	}
	if n > cfg.Limits.MaxFileBytes {
		return failure("too_large", "attachment exceeds byte limit", true)
	}
	jobCtx, cancel := context.WithTimeout(r.Context(), cfg.Timeout)
	defer cancel()
	if mimeType == "application/pdf" || strings.HasSuffix(strings.ToLower(filename), ".pdf") {
		return extractPDF(jobCtx, cfg, input, dir)
	}
	if strings.HasPrefix(mimeType, "image/") {
		return extractImage(jobCtx, cfg, input)
	}
	return failure("unsupported_type", "only PDF and image attachments are supported", true)
}

func extractPDF(ctx context.Context, cfg ExecutorConfig, input, dir string) Response {
	info, err := runBounded(ctx, cfg.Limits.MaxOutputBytes, cfg.PDFInfo, input)
	if err != nil {
		return commandFailure(ctx, "invalid_pdf", err)
	}
	pages := parsePDFPages(string(info))
	if pages < 1 {
		return failure("invalid_pdf", "pdfinfo did not report a positive page count", true)
	}
	if pages > cfg.Limits.MaxPages {
		return failure("too_many_pages", fmt.Sprintf("PDF has %d pages; limit is %d", pages, cfg.Limits.MaxPages), true)
	}
	textOutput, err := runBounded(ctx, cfg.Limits.MaxOutputBytes, cfg.PDFToText,
		"-f", "1", "-l", strconv.Itoa(pages), "-layout", input, "-")
	if err != nil {
		return commandFailure(ctx, "pdf_text_failed", err)
	}
	nativePages := splitPDFText(textOutput, pages)
	needsOCR := make([]int, 0, pages)
	for page, text := range nativePages {
		if !usefulText(strings.TrimSpace(text)) {
			needsOCR = append(needsOCR, page+1)
		}
	}
	pageInfo := info
	if len(needsOCR) > 0 {
		pageInfo, err = runBounded(ctx, max(int64(64<<10), int64(pages)*1024), cfg.PDFInfo,
			"-f", "1", "-l", strconv.Itoa(pages), "-box", input)
		if err != nil {
			return commandFailure(ctx, "invalid_pdf", err)
		}
	}
	result := Response{Pages: make([]store.OCRPage, 0, pages)}
	native, ocrCount := 0, 0
	var outputBytes int64
	for page := 1; page <= pages; page++ {
		clean := strings.TrimSpace(nativePages[page-1])
		if usefulText(clean) {
			outputBytes += int64(len(clean))
			if outputBytes > cfg.Limits.MaxOutputBytes {
				return failure("output_too_large", "extracted text exceeds output limit", true)
			}
			result.Pages = append(result.Pages, store.OCRPage{PageNumber: page, Method: "native", Text: clean, Confidence: 100})
			native++
			continue
		}
		prefix := filepath.Join(dir, fmt.Sprintf("page-%04d", page))
		if pixels := estimatedPDFPixels(string(pageInfo), page, cfg.DPI); pixels <= 0 || pixels > cfg.Limits.MaxPixels {
			return failure("too_many_pixels", fmt.Sprintf("rendered page would have %d pixels; limit is %d", pixels, cfg.Limits.MaxPixels), true)
		}
		_, err = runBounded(ctx, 64<<10, cfg.PDFToPPM, "-f", strconv.Itoa(page), "-l", strconv.Itoa(page), "-singlefile", "-gray", "-r", strconv.Itoa(cfg.DPI), "-png", input, prefix)
		if err != nil {
			return commandFailure(ctx, "pdf_render_failed", err)
		}
		rendered := prefix + ".png"
		info, err := inspectImage(ctx, rendered)
		if err != nil {
			return commandFailure(ctx, "pdf_render_failed", err)
		}
		ocrInput, ocrInfo, permanent, err := prepareImageForOCR(ctx, cfg, rendered, info)
		if err != nil {
			return failure("image_preprocess_failed", err.Error(), permanent)
		}
		ocrPage, err := tesseractPage(ctx, cfg, ocrInput, page, ocrInfo)
		if err != nil {
			return commandFailure(ctx, "ocr_failed", err)
		}
		result.Pages = append(result.Pages, ocrPage)
		outputBytes += int64(len(ocrPage.Text))
		if outputBytes > cfg.Limits.MaxOutputBytes {
			return failure("output_too_large", "extracted text exceeds output limit", true)
		}
		ocrCount++
	}
	switch {
	case native > 0 && ocrCount > 0:
		result.Method = "mixed"
	case ocrCount > 0:
		result.Method = "ocr"
	default:
		result.Method = "native"
	}
	return result
}

func extractImage(ctx context.Context, cfg ExecutorConfig, input string) Response {
	ic, err := inspectImage(ctx, input)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return commandFailure(ctx, "image_inspection_failed", err)
		}
		return failure("unsupported_image", err.Error(), true)
	}
	pixels := int64(ic.Width) * int64(ic.Height)
	if pixels <= 0 || pixels > cfg.Limits.MaxPixels {
		return failure("too_many_pixels", fmt.Sprintf("image has %d pixels; limit is %d", pixels, cfg.Limits.MaxPixels), true)
	}
	ocrInput, ocrInfo, permanent, err := prepareImageForOCR(ctx, cfg, input, ic)
	if err != nil {
		return failure("image_preprocess_failed", err.Error(), permanent)
	}
	page, err := tesseractPage(ctx, cfg, ocrInput, 1, ocrInfo)
	if err != nil {
		return commandFailure(ctx, "ocr_failed", err)
	}
	return Response{Method: "ocr", Pages: []store.OCRPage{page}}
}

// smallImageScale brings short screenshots and table strips into Tesseract's
// useful character-size range. Scaling is bounded both by a small fixed factor
// and the configured decoded-pixel ceiling.
func smallImageScale(width, height, minSide, maxScale int, maxPixels, maxBytes int64) int {
	shortSide := min(width, height)
	if shortSide <= 0 || shortSide >= minSide || maxScale <= 1 {
		return 1
	}
	scale := min(maxScale, (minSide+shortSide-1)/shortSide)
	pixels := int64(width) * int64(height)
	for scale > 1 && (pixels*int64(scale)*int64(scale) > maxPixels ||
		pixels*int64(scale)*int64(scale)*4 > maxBytes) {
		scale--
	}
	return scale
}

func inspectImage(ctx context.Context, path string) (image.Config, error) {
	if err := ctx.Err(); err != nil {
		return image.Config{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return image.Config{}, err
	}
	info, _, decodeErr := image.DecodeConfig(contextReader{ctx: ctx, reader: f})
	closeErr := f.Close()
	if err := ctx.Err(); err != nil {
		return image.Config{}, errors.Join(err, closeErr)
	}
	if decodeErr != nil || closeErr != nil {
		return image.Config{}, errors.Join(decodeErr, closeErr)
	}
	return info, nil
}

func prepareImageForOCR(ctx context.Context, cfg ExecutorConfig, input string, info image.Config) (string, image.Config, bool, error) {
	scale := smallImageScale(info.Width, info.Height, cfg.MinImageSide, cfg.MaxImageScale,
		cfg.Limits.MaxPixels, cfg.Limits.MaxPreprocessBytes)
	if scale == 1 {
		return input, info, false, nil
	}
	return upscaleImage(ctx, input, scale)
}

func upscaleImage(ctx context.Context, input string, scale int) (string, image.Config, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", image.Config{}, false, err
	}
	f, err := os.Open(input)
	if err != nil {
		return "", image.Config{}, false, fmt.Errorf("open image for upscaling: %w", err)
	}
	source, _, decodeErr := image.Decode(contextReader{ctx: ctx, reader: f})
	closeErr := f.Close()
	if err := ctx.Err(); err != nil {
		return "", image.Config{}, false, errors.Join(err, closeErr)
	}
	if decodeErr != nil {
		permanent := !errors.Is(decodeErr, context.Canceled) && !errors.Is(decodeErr, context.DeadlineExceeded)
		return "", image.Config{}, permanent, fmt.Errorf("decode image for upscaling: %w", decodeErr)
	}
	if closeErr != nil {
		return "", image.Config{}, false, fmt.Errorf("close image after decode: %w", closeErr)
	}
	bounds := source.Bounds()
	target := image.NewNRGBA(image.Rect(0, 0, bounds.Dx()*scale, bounds.Dy()*scale))
	for sourceY := range bounds.Dy() {
		if err := ctx.Err(); err != nil {
			return "", image.Config{}, false, err
		}
		for sourceX := range bounds.Dx() {
			pixel := color.NRGBAModel.Convert(source.At(bounds.Min.X+sourceX, bounds.Min.Y+sourceY)).(color.NRGBA)
			for dy := range scale {
				row := (sourceY*scale+dy)*target.Stride + sourceX*scale*4
				for dx := range scale {
					offset := row + dx*4
					target.Pix[offset], target.Pix[offset+1] = pixel.R, pixel.G
					target.Pix[offset+2], target.Pix[offset+3] = pixel.B, pixel.A
				}
			}
		}
	}
	path := strings.TrimSuffix(input, filepath.Ext(input)) + "-upscaled.png"
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", image.Config{}, false, fmt.Errorf("create upscaled image: %w", err)
	}
	encodeErr := png.Encode(contextWriter{ctx: ctx, writer: out}, target)
	closeErr = out.Close()
	if encodeErr != nil || closeErr != nil {
		return "", image.Config{}, false, fmt.Errorf("write upscaled image: %w", errors.Join(encodeErr, closeErr))
	}
	return path, image.Config{ColorModel: color.NRGBAModel, Width: bounds.Dx() * scale, Height: bounds.Dy() * scale}, false, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

type contextWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (w contextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.writer.Write(p)
}

func tesseractPage(ctx context.Context, cfg ExecutorConfig, imagePath string, page int, ic image.Config) (store.OCRPage, error) {
	if pixels := int64(ic.Width) * int64(ic.Height); pixels <= 0 || pixels > cfg.Limits.MaxPixels {
		return store.OCRPage{}, fmt.Errorf("rendered page has %d pixels; limit is %d", pixels, cfg.Limits.MaxPixels)
	}
	tsv, err := runBounded(ctx, cfg.Limits.MaxOutputBytes, cfg.Tesseract, imagePath, "stdout", "-l", cfg.Languages, "tsv")
	if err != nil {
		return store.OCRPage{}, err
	}
	text, confidence := parseTSV(tsv)
	return store.OCRPage{PageNumber: page, Method: "ocr", Text: text, Confidence: confidence}, nil
}

func parsePDFPages(info string) int {
	for _, line := range strings.Split(info, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(key), "Pages") {
			n, _ := strconv.Atoi(strings.TrimSpace(value))
			return n
		}
	}
	return 0
}

func splitPDFText(text []byte, pages int) []string {
	parts := strings.Split(string(text), "\f")
	if len(parts) > pages {
		parts = parts[:pages]
	}
	for len(parts) < pages {
		parts = append(parts, "")
	}
	return parts
}

func estimatedPDFPixels(info string, page, dpi int) int64 {
	var fallback int64
	for _, line := range strings.Split(info, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		fields := strings.Fields(value)
		if len(fields) < 3 {
			continue
		}
		width, errW := strconv.ParseFloat(fields[0], 64)
		height, errH := strconv.ParseFloat(fields[2], 64)
		if errW != nil || errH != nil {
			continue
		}
		pixels := int64(width*float64(dpi)/72) * int64(height*float64(dpi)/72)
		if strings.EqualFold(key, "Page size") {
			fallback = pixels
			continue
		}
		keyFields := strings.Fields(key)
		if len(keyFields) == 3 && strings.EqualFold(keyFields[0], "Page") && strings.EqualFold(keyFields[2], "size") {
			n, parseErr := strconv.Atoi(keyFields[1])
			if parseErr == nil && n == page {
				return pixels
			}
		}
	}
	return fallback
}

func usefulText(text string) bool {
	letters := 0
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			letters++
		}
	}
	return letters >= 20
}

func parseTSV(data []byte) (string, float64) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	var lines []string
	var words []string
	lastLine := ""
	var total float64
	count := 0
	first := true
	for scanner.Scan() {
		if first {
			first = false
			continue
		}
		cols := strings.Split(scanner.Text(), "\t")
		if len(cols) < 12 || cols[0] != "5" || strings.TrimSpace(cols[11]) == "" {
			continue
		}
		lineKey := strings.Join(cols[1:5], "/")
		if lastLine != "" && lineKey != lastLine {
			lines = append(lines, strings.Join(words, " "))
			words = words[:0]
		}
		lastLine = lineKey
		words = append(words, cols[11])
		if conf, err := strconv.ParseFloat(cols[10], 64); err == nil && conf >= 0 {
			total += conf
			count++
		}
	}
	if len(words) > 0 {
		lines = append(lines, strings.Join(words, " "))
	}
	if count == 0 {
		return strings.TrimSpace(strings.Join(lines, "\n")), 0
	}
	return strings.TrimSpace(strings.Join(lines, "\n")), total / float64(count)
}

type cappedBuffer struct {
	bytes.Buffer
	max int64
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if int64(b.Len()+len(p)) > b.max {
		return 0, errors.New("command output exceeds limit")
	}
	return b.Buffer.Write(p)
}

func runBounded(ctx context.Context, max int64, name string, args ...string) ([]byte, error) {
	var stdout cappedBuffer
	stdout.max = max
	var stderr cappedBuffer
	stderr.max = 256 << 10
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w: %s", filepath.Base(name), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func commandFailure(ctx context.Context, code string, err error) Response {
	if ctx.Err() != nil {
		return failure("timeout", ctx.Err().Error(), false)
	}
	return failure(code, err.Error(), code == "invalid_pdf")
}
func failure(code, detail string, permanent bool) Response {
	return Response{ErrorCode: code, Error: detail, Permanent: permanent}
}
