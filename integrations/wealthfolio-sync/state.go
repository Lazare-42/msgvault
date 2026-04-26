package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type syncState struct {
	Version     int                       `json:"version"`
	Attachments map[string]seenAttachment `json:"attachments"`
}

type seenAttachment struct {
	RuleName             string    `json:"rule_name"`
	SourceMessageID      string    `json:"source_message_id"`
	MessageID            int64     `json:"message_id"`
	AttachmentFilename   string    `json:"attachment_filename"`
	AttachmentContentSHA string    `json:"attachment_content_sha256"`
	WealthfolioAccountID string    `json:"wealthfolio_account_id"`
	PDFPath              string    `json:"pdf_path"`
	MetadataPath         string    `json:"metadata_path"`
	ExportedAt           time.Time `json:"exported_at"`
}

func loadState(path string) (*syncState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &syncState{
				Version:     1,
				Attachments: make(map[string]seenAttachment),
			}, nil
		}
		return nil, fmt.Errorf("read state: %w", err)
	}

	var st syncState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	if st.Version == 0 {
		st.Version = 1
	}
	if st.Attachments == nil {
		st.Attachments = make(map[string]seenAttachment)
	}
	return &st, nil
}

func saveState(path string, st *syncState) error {
	if st.Version == 0 {
		st.Version = 1
	}
	if st.Attachments == nil {
		st.Attachments = make(map[string]seenAttachment)
	}

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "wealthfolio-sync-state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp state file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp state file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}
	return nil
}

func makeSeenKey(sourceMessageID, contentHash string) string {
	return sourceMessageID + ":" + contentHash
}
