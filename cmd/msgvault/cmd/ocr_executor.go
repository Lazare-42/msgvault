package cmd

import (
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/ocr"
)

var (
	ocrExecutorSocket  string
	ocrExecutorTempDir string
	ocrExecutorDPI     int
	ocrExecutorTimeout time.Duration
)

var ocrExecutorCmd = &cobra.Command{
	Use:   "ocr-executor",
	Short: "Run the stateless local attachment OCR executor",
	Long:  "Run a single-concurrency Poppler/Tesseract executor on a Unix socket. It never opens a msgvault archive or database.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		socket := cfg.OCR.Socket
		if ocrExecutorSocket != "" {
			socket = ocrExecutorSocket
		}
		timeout := cfg.OCR.RequestTimeout
		if ocrExecutorTimeout > 0 {
			timeout = ocrExecutorTimeout
		}
		return ocr.ServeExecutor(cmd.Context(), ocr.ExecutorConfig{
			Socket: socket, Languages: cfg.OCR.Languages, DPI: ocrExecutorDPI,
			Timeout: timeout, TempDir: ocrExecutorTempDir,
			MinImageSide: cfg.OCR.MinImageSide, MaxImageScale: cfg.OCR.MaxImageScale,
			Limits: ocr.Limits{MaxFileBytes: cfg.OCR.MaxFileBytes, MaxPages: cfg.OCR.MaxPages,
				MaxPixels: cfg.OCR.MaxPixels, MaxPreprocessBytes: cfg.OCR.MaxPreprocessBytes,
				MaxOutputBytes: cfg.OCR.MaxOutputBytes},
		})
	},
}

func init() {
	ocrExecutorCmd.Flags().StringVar(&ocrExecutorSocket, "socket", "", "Unix socket path (defaults to [ocr] socket)")
	ocrExecutorCmd.Flags().StringVar(&ocrExecutorTempDir, "temp-dir", "", "temporary file directory")
	ocrExecutorCmd.Flags().IntVar(&ocrExecutorDPI, "dpi", 200, "PDF rasterization DPI")
	ocrExecutorCmd.Flags().DurationVar(&ocrExecutorTimeout, "timeout", 0, "hard extraction timeout")
	rootCmd.AddCommand(ocrExecutorCmd)
}
