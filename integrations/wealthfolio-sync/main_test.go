package main

import (
	"os"
	"path/filepath"
	"testing"

	"go.kenn.io/msgvault/internal/query"
)

func TestMakeSeenKey(t *testing.T) {
	got := makeSeenKey("msg-1", "abc123")
	if got != "msg-1:abc123" {
		t.Fatalf("unexpected key: %s", got)
	}
}

func TestAttachmentMatchesRuleRequiresPDF(t *testing.T) {
	rule := compiledRule{}
	if attachmentMatchesRule(query.AttachmentInfo{
		Filename: "statement.txt",
		MimeType: "text/plain",
	}, rule) {
		t.Fatal("expected non-pdf attachment to be rejected")
	}
}

func TestSaveAndLoadState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	state := &syncState{
		Version: 1,
		Attachments: map[string]seenAttachment{
			"msg:hash": {
				RuleName:             "hsbc",
				SourceMessageID:      "msg",
				AttachmentContentSHA: "hash",
				WealthfolioAccountID: "acc1",
			},
		},
	}

	if err := saveState(path, state); err != nil {
		t.Fatalf("saveState failed: %v", err)
	}

	loaded, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState failed: %v", err)
	}
	if loaded.Attachments["msg:hash"].WealthfolioAccountID != "acc1" {
		t.Fatalf("unexpected loaded state: %+v", loaded.Attachments["msg:hash"])
	}
}

func TestBuildOutputNameUsesPDFExtension(t *testing.T) {
	msg := &query.MessageDetail{}
	att := query.AttachmentInfo{
		Filename:    "HSBC Statement.PDF",
		ContentHash: "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
	}

	name := buildOutputName(msg, att)
	if filepath.Ext(name) != ".pdf" {
		t.Fatalf("expected .pdf extension, got %s", name)
	}
	if name == "" {
		t.Fatal("expected non-empty output name")
	}
}

func TestExpandPathLeavesPlainPathUntouched(t *testing.T) {
	const path = "/tmp/example"
	if got := expandPath(path); got != path {
		t.Fatalf("unexpected expanded path: %s", got)
	}
}

func TestLoadStateMissingFileReturnsEmptyState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.json")
	state, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState failed: %v", err)
	}
	if state == nil || len(state.Attachments) != 0 {
		t.Fatalf("unexpected state: %+v", state)
	}
}

func TestWriteJSONAtomicCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meta.json")
	if err := writeJSONAtomic(path, map[string]string{"ok": "yes"}); err != nil {
		t.Fatalf("writeJSONAtomic failed: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file to exist: %v", err)
	}
}
