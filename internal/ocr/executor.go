package ocr

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
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
	Socket    string
	Languages string
	DPI       int
	Timeout   time.Duration
	Limits    Limits
	TempDir   string
	PDFInfo   string
	PDFToText string
	PDFToPPM  string
	Tesseract string
}

func (c *ExecutorConfig) defaults() {
	if c.Languages == "" {
		c.Languages = "fra+eng"
	}
	if c.DPI <= 0 {
		c.DPI = 200
	}
	if c.Timeout <= 0 {
		c.Timeout = 10 * time.Minute
	}
	if c.Limits.MaxFileBytes <= 0 {
		c.Limits.MaxFileBytes = 100 << 20
	}
	if c.Limits.MaxPages <= 0 {
		c.Limits.MaxPages = 200
	}
	if c.Limits.MaxPixels <= 0 {
		c.Limits.MaxPixels = 40_000_000
	}
	if c.Limits.MaxOutputBytes <= 0 {
		c.Limits.MaxOutputBytes = 16 << 20
	}
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
	result := Response{Pages: make([]store.OCRPage, 0, pages)}
	native, ocrCount := 0, 0
	var outputBytes int64
	for page := 1; page <= pages; page++ {
		text, err := runBounded(ctx, cfg.Limits.MaxOutputBytes, cfg.PDFToText, "-f", strconv.Itoa(page), "-l", strconv.Itoa(page), "-layout", input, "-")
		if err != nil {
			return commandFailure(ctx, "pdf_text_failed", err)
		}
		clean := strings.TrimSpace(string(text))
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
		pageInfo, infoErr := runBounded(ctx, 64<<10, cfg.PDFInfo, "-f", strconv.Itoa(page), "-l", strconv.Itoa(page), input)
		if infoErr != nil {
			return commandFailure(ctx, "invalid_pdf", infoErr)
		}
		if pixels := estimatedPDFPixels(string(pageInfo), cfg.DPI); pixels <= 0 || pixels > cfg.Limits.MaxPixels {
			return failure("too_many_pixels", fmt.Sprintf("rendered page would have %d pixels; limit is %d", pixels, cfg.Limits.MaxPixels), true)
		}
		_, err = runBounded(ctx, 64<<10, cfg.PDFToPPM, "-f", strconv.Itoa(page), "-l", strconv.Itoa(page), "-singlefile", "-gray", "-r", strconv.Itoa(cfg.DPI), "-png", input, prefix)
		if err != nil {
			return commandFailure(ctx, "pdf_render_failed", err)
		}
		ocrPage, err := tesseractPage(ctx, cfg, prefix+".png", page)
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
	f, err := os.Open(input)
	if err != nil {
		return failure("invalid_image", err.Error(), true)
	}
	ic, _, err := image.DecodeConfig(f)
	_ = f.Close()
	if err != nil {
		return failure("unsupported_image", err.Error(), true)
	}
	pixels := int64(ic.Width) * int64(ic.Height)
	if pixels <= 0 || pixels > cfg.Limits.MaxPixels {
		return failure("too_many_pixels", fmt.Sprintf("image has %d pixels; limit is %d", pixels, cfg.Limits.MaxPixels), true)
	}
	page, err := tesseractPage(ctx, cfg, input, 1)
	if err != nil {
		return commandFailure(ctx, "ocr_failed", err)
	}
	return Response{Method: "ocr", Pages: []store.OCRPage{page}}
}

func tesseractPage(ctx context.Context, cfg ExecutorConfig, imagePath string, page int) (store.OCRPage, error) {
	f, err := os.Open(imagePath)
	if err != nil {
		return store.OCRPage{}, err
	}
	ic, _, err := image.DecodeConfig(f)
	_ = f.Close()
	if err != nil {
		return store.OCRPage{}, fmt.Errorf("decode rendered page: %w", err)
	}
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

func estimatedPDFPixels(info string, dpi int) int64 {
	for _, line := range strings.Split(info, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "Page size") {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) < 3 {
			return 0
		}
		width, errW := strconv.ParseFloat(fields[0], 64)
		height, errH := strconv.ParseFloat(fields[2], 64)
		if errW != nil || errH != nil {
			return 0
		}
		return int64(width*float64(dpi)/72) * int64(height*float64(dpi)/72)
	}
	return 0
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
