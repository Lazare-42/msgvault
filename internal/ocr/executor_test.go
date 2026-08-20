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
	tesseract, err := exec.LookPath("tesseract")
	if err != nil {
		t.Skip("tesseract not installed")
	}
	path := filepath.Join(t.TempDir(), "synthetic.png")
	img := image.NewGray(image.Rect(0, 0, 520, 160))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	drawBlockText(img, 30, 35, "TEST123", 12)
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, png.Encode(f, img))
	require.NoError(t, f.Close())

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	resp := extractImage(ctx, ExecutorConfig{
		Languages: "eng", Tesseract: tesseract,
		Limits: Limits{MaxPixels: 1_000_000, MaxOutputBytes: 1 << 20},
	}, path)
	assert.Empty(t, resp.ErrorCode, resp.Error)
	assert.Equal(t, "ocr", resp.Method)
	if assert.Len(t, resp.Pages, 1) {
		assert.Equal(t, "ocr", resp.Pages[0].Method)
		assert.NotEmpty(t, resp.Pages[0].Text)
	}
}

func TestExtractPDFNativeWithRealPoppler(t *testing.T) {
	pdfinfo, err := exec.LookPath("pdfinfo")
	if err != nil {
		t.Skip("pdfinfo not installed")
	}
	pdftotext, err := exec.LookPath("pdftotext")
	if err != nil {
		t.Skip("pdftotext not installed")
	}
	path := filepath.Join(t.TempDir(), "synthetic.pdf")
	require.NoError(t, os.WriteFile(path, syntheticTextPDF("SYNTHETIC ORCHID INVOICE 417"), 0o600))
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	resp := extractPDF(ctx, ExecutorConfig{
		PDFInfo: pdfinfo, PDFToText: pdftotext,
		Limits: Limits{MaxPages: 10, MaxPixels: 40_000_000, MaxOutputBytes: 1 << 20},
		DPI:    200,
	}, path, t.TempDir())
	assert.Empty(t, resp.ErrorCode, resp.Error)
	assert.Equal(t, "native", resp.Method)
	if assert.Len(t, resp.Pages, 1) {
		assert.Equal(t, "native", resp.Pages[0].Method)
		assert.Contains(t, resp.Pages[0].Text, "ORCHID")
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
