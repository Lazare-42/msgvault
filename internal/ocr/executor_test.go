package ocr

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseTSVPreservesLinesAndConfidence(t *testing.T) {
	tsv := []byte("level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\n" +
		"5\t1\t1\t1\t1\t1\t0\t0\t1\t1\t90\tSynthetic\n" +
		"5\t1\t1\t1\t1\t2\t0\t0\t1\t1\t80\tinvoice\n" +
		"5\t1\t1\t1\t2\t1\t0\t0\t1\t1\t70\ttotal\n")
	text, confidence := parseTSV(tsv)
	assert.Equal(t, "Synthetic invoice\ntotal", text)
	assert.Equal(t, 80.0, confidence)
}

func TestExtractImageWithRealTesseract(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	tesseract, err := exec.LookPath("tesseract")
	if err != nil {
		t.Skip("tesseract not installed")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "synthetic.png")
	img := image.NewGray(image.Rect(0, 0, 260, 60))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	drawBlockText(img, 10, 10, "TEST123", 4)
	f, err := os.Create(path)
	requirements.NoError(err)
	requirements.NoError(png.Encode(f, img))
	requirements.NoError(f.Close())

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cfg := ExecutorConfig{
		Languages: "eng", Tesseract: tesseract,
		Limits: Limits{MaxPixels: 1_000_000, MaxPreprocessBytes: 8 << 20, MaxOutputBytes: 1 << 20},
	}
	cfg.defaults()
	resp := extractImage(ctx, cfg, path)
	assertions.Empty(resp.ErrorCode, resp.Error)
	assertions.Equal("ocr", resp.Method)
	if assertions.Len(resp.Pages, 1) {
		assertions.Equal("ocr", resp.Pages[0].Method)
		assertions.Contains(resp.Pages[0].Text, "TEST")
	}
	_, err = os.Stat(filepath.Join(dir, "synthetic-upscaled.png"))
	assertions.NoError(err, "small-image integration must exercise preprocessing")
}

func TestExtractLargeImageWithRealTesseract(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	tesseract, err := exec.LookPath("tesseract")
	if err != nil {
		t.Skip("tesseract not installed")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "large.png")
	img := image.NewGray(image.Rect(0, 0, 520, 400))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	drawBlockText(img, 30, 35, "TEST123", 12)
	f, err := os.Create(path)
	requirements.NoError(err)
	requirements.NoError(png.Encode(f, img))
	requirements.NoError(f.Close())
	cfg := ExecutorConfig{
		Languages: "eng", Tesseract: tesseract,
		Limits: Limits{MaxPixels: 1_000_000, MaxPreprocessBytes: 8 << 20, MaxOutputBytes: 1 << 20},
	}
	cfg.defaults()
	resp := extractImage(t.Context(), cfg, path)
	assertions.Empty(resp.ErrorCode, resp.Error)
	if assertions.Len(resp.Pages, 1) {
		assertions.Contains(resp.Pages[0].Text, "TEST")
	}
	_, err = os.Stat(filepath.Join(dir, "large-upscaled.png"))
	assertions.ErrorIs(err, os.ErrNotExist, "normal-size image must keep direct OCR path")
}

func TestSmallImageScale(t *testing.T) {
	tests := []struct {
		name      string
		width     int
		height    int
		maxPixels int64
		maxBytes  int64
		wantScale int
	}{
		{name: "short table strip", width: 858, height: 80, maxPixels: 40_000_000, maxBytes: 64 << 20, wantScale: 4},
		{name: "ordinary image", width: 800, height: 600, maxPixels: 40_000_000, maxBytes: 64 << 20, wantScale: 1},
		{name: "pixel limit", width: 10_000, height: 80, maxPixels: 5_000_000, maxBytes: 64 << 20, wantScale: 2},
		{name: "memory limit", width: 858, height: 80, maxPixels: 40_000_000, maxBytes: 2 << 20, wantScale: 2},
		{name: "invalid dimensions", width: 0, height: 80, maxPixels: 40_000_000, maxBytes: 64 << 20, wantScale: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantScale, smallImageScale(tt.width, tt.height,
				DefaultMinImageSide, DefaultMaxImageScale, tt.maxPixels, tt.maxBytes))
		})
	}
}

func TestUpscaleImagePreservesPixels(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	dir := t.TempDir()
	input := filepath.Join(dir, "input.png")
	img := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	img.Set(0, 0, color.NRGBA{R: 255, A: 255})
	img.Set(1, 0, color.NRGBA{B: 255, A: 255})
	f, err := os.Create(input)
	requirements.NoError(err)
	requirements.NoError(png.Encode(f, img))
	requirements.NoError(f.Close())

	output, info, permanent, err := upscaleImage(t.Context(), input, 4)
	requirements.NoError(err)
	assertions.False(permanent)
	assertions.Equal(8, info.Width)
	assertions.Equal(4, info.Height)
	out, err := os.Open(output)
	requirements.NoError(err)
	upscaled, _, err := image.Decode(out)
	requirements.NoError(err)
	requirements.NoError(out.Close())
	assertions.Equal(image.Rect(0, 0, 8, 4), upscaled.Bounds())
	assertions.Equal(color.NRGBAModel.Convert(img.At(0, 0)), color.NRGBAModel.Convert(upscaled.At(3, 3)))
	assertions.Equal(color.NRGBAModel.Convert(img.At(1, 0)), color.NRGBAModel.Convert(upscaled.At(4, 0)))
}

func TestPrepareImageCancellationIsRetryable(t *testing.T) {
	requirements := require.New(t)
	dir := t.TempDir()
	input := filepath.Join(dir, "input.png")
	img := image.NewGray(image.Rect(0, 0, 20, 20))
	f, err := os.Create(input)
	requirements.NoError(err)
	requirements.NoError(png.Encode(f, img))
	requirements.NoError(f.Close())
	cfg := ExecutorConfig{}
	cfg.defaults()
	info, err := inspectImage(t.Context(), input)
	requirements.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, permanent, err := prepareImageForOCR(ctx, cfg, input, info)
	requirements.ErrorIs(err, context.Canceled)
	assert.False(t, permanent)
}

func TestPrepareImageDecodeFailureIsPermanent(t *testing.T) {
	requirements := require.New(t)
	dir := t.TempDir()
	input := filepath.Join(dir, "truncated.png")
	img := image.NewGray(image.Rect(0, 0, 20, 20))
	var encoded bytes.Buffer
	requirements.NoError(png.Encode(&encoded, img))
	data := encoded.Bytes()
	requirements.NoError(os.WriteFile(input, data[:len(data)/2], 0o600))
	info, err := inspectImage(t.Context(), input)
	requirements.NoError(err, "PNG header must remain readable")
	cfg := ExecutorConfig{}
	cfg.defaults()
	_, _, permanent, err := prepareImageForOCR(t.Context(), cfg, input, info)
	requirements.Error(err)
	assert.True(t, permanent)
}

func TestExtractPDFNativeWithRealPoppler(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	pdfinfo, err := exec.LookPath("pdfinfo")
	if err != nil {
		t.Skip("pdfinfo not installed")
	}
	pdftotext, err := exec.LookPath("pdftotext")
	if err != nil {
		t.Skip("pdftotext not installed")
	}
	path := filepath.Join(t.TempDir(), "synthetic.pdf")
	requirements.NoError(os.WriteFile(path, syntheticTextPDF("SYNTHETIC ORCHID INVOICE 417"), 0o600))
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	resp := extractPDF(ctx, ExecutorConfig{
		PDFInfo: pdfinfo, PDFToText: pdftotext,
		Limits: Limits{MaxPages: 10, MaxPixels: 40_000_000, MaxOutputBytes: 1 << 20},
		DPI:    200,
	}, path, t.TempDir())
	assertions.Empty(resp.ErrorCode, resp.Error)
	assertions.Equal("native", resp.Method)
	if assertions.Len(resp.Pages, 1) {
		assertions.Equal("native", resp.Pages[0].Method)
		assertions.Contains(resp.Pages[0].Text, "ORCHID")
	}
}

func syntheticTextPDF(text string) []byte {
	content := fmt.Sprintf("BT /F1 24 Tf 72 720 Td (%s) Tj ET", text)
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
	}
	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects)+1)
	for i, object := range objects {
		offsets[i+1] = pdf.Len()
		fmt.Fprintf(&pdf, "%d 0 obj\n%s\nendobj\n", i+1, object)
	}
	xref := pdf.Len()
	fmt.Fprintf(&pdf, "xref\n0 %d\n0000000000 65535 f \n", len(offsets))
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&pdf, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&pdf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xref)
	return pdf.Bytes()
}

func drawBlockText(img *image.Gray, x, y int, text string, scale int) {
	glyphs := map[rune][]string{
		'T': {"11111", "00100", "00100", "00100", "00100", "00100", "00100"},
		'E': {"11111", "10000", "10000", "11110", "10000", "10000", "11111"},
		'S': {"11111", "10000", "10000", "11111", "00001", "00001", "11111"},
		'1': {"00100", "01100", "00100", "00100", "00100", "00100", "01110"},
		'2': {"11110", "00001", "00001", "01110", "10000", "10000", "11111"},
		'3': {"11110", "00001", "00001", "01110", "00001", "00001", "11110"},
	}
	black := color.Gray{Y: 0}
	for _, r := range text {
		for row, bits := range glyphs[r] {
			for col, bit := range bits {
				if bit == '1' {
					for dy := range scale {
						for dx := range scale {
							img.SetGray(x+col*scale+dx, y+row*scale+dy, black)
						}
					}
				}
			}
		}
		x += 6 * scale
	}
}

func TestUsefulTextThreshold(t *testing.T) {
	assert.False(t, usefulText("page 1"))
	assert.True(t, usefulText("A synthetic native PDF paragraph 12345"))
}

func TestSplitPDFTextAndPageMetrics(t *testing.T) {
	assertions := assert.New(t)
	pages := splitPDFText([]byte("first\fsecond\f"), 2)
	assertions.Equal([]string{"first", "second"}, pages)
	info := "Page    1 size: 612 x 792 pts\nPage    2 size: 300 x 400 pts\n"
	assertions.Equal(int64(2_082_500), estimatedPDFPixels(info, 2, 300))
}
