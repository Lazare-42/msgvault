package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

type syncRunner struct {
	cfg    *compiledConfig
	state  *syncState
	dryRun bool
}

type syncResult struct {
	MessagesScanned    int
	AttachmentsSeen    int
	AttachmentsNew     int
	AttachmentsSkipped int
}

type wealthfolioMetadata struct {
	Version               int       `json:"version"`
	RuleName              string    `json:"ruleName"`
	WealthfolioAccountID  string    `json:"wealthfolioAccountId"`
	MsgvaultAccount       string    `json:"msgvaultAccount,omitempty"`
	MessageID             int64     `json:"messageId"`
	SourceMessageID       string    `json:"sourceMessageId"`
	ConversationID        int64     `json:"conversationId"`
	SourceConversationID  string    `json:"sourceConversationId"`
	Subject               string    `json:"subject"`
	From                  []string  `json:"from"`
	To                    []string  `json:"to,omitempty"`
	SentAt                string    `json:"sentAt,omitempty"`
	ReceivedAt            string    `json:"receivedAt,omitempty"`
	AttachmentFilename    string    `json:"attachmentFilename"`
	AttachmentMimeType    string    `json:"attachmentMimeType"`
	AttachmentContentHash string    `json:"attachmentContentHash"`
	ExportedAt            time.Time `json:"exportedAt"`
}

func newSyncRunner(cfg *compiledConfig, st *syncState, dryRun bool) *syncRunner {
	return &syncRunner{cfg: cfg, state: st, dryRun: dryRun}
}

func (r *syncRunner) run(ctx context.Context) (*syncResult, error) {
	s, err := store.Open(r.cfg.DatabasePath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.InitSchema(); err != nil {
		return nil, fmt.Errorf("init schema: %w", err)
	}

	engine := query.NewSQLiteEngine(s.DB())
	defer func() { _ = engine.Close() }()

	accountIDs, err := resolveAccountIdentifiers(ctx, engine)
	if err != nil {
		return nil, err
	}

	result := &syncResult{}
	for _, rule := range r.cfg.Rules {
		exported, err := r.processRule(ctx, engine, accountIDs, rule, result)
		if err != nil {
			return nil, err
		}
		if exported > 0 && !r.dryRun {
			if err := saveState(r.cfg.StateFileAbs, r.state); err != nil {
				return nil, err
			}
		}
	}

	if r.cfg.Delivery.RsyncTarget != "" {
		if err := r.runRsync(ctx); err != nil {
			return result, err
		}
	}

	return result, nil
}

func (r *syncRunner) processRule(
	ctx context.Context,
	engine query.Engine,
	accountIDs map[string]int64,
	rule compiledRule,
	result *syncResult,
) (int, error) {
	q := search.Parse(rule.Query)
	if rule.MsgvaultAccount != "" {
		accountID, ok := accountIDs[rule.MsgvaultAccount]
		if !ok {
			return 0, fmt.Errorf("rule %q: msgvault account %q not found", rule.Name, rule.MsgvaultAccount)
		}
		q.AccountIDs = []int64{accountID}
	}

	var exported int
	offset := 0
	scannedForRule := 0
	for {
		limit := r.cfg.PageSize
		if rule.MaxMessages > 0 {
			remaining := rule.MaxMessages - scannedForRule
			if remaining <= 0 {
				return exported, nil
			}
			if remaining < limit {
				limit = remaining
			}
		}

		rows, err := engine.Search(ctx, q, limit, offset)
		if err != nil {
			return exported, fmt.Errorf("rule %q search failed: %w", rule.Name, err)
		}
		if len(rows) == 0 {
			return exported, nil
		}

		for _, row := range rows {
			result.MessagesScanned++
			scannedForRule++
			added, err := r.processMessage(ctx, engine, rule, row)
			if err != nil {
				return exported, err
			}
			exported += added
			result.AttachmentsNew += added
		}

		if len(rows) < limit {
			return exported, nil
		}
		offset += len(rows)
	}
}

func (r *syncRunner) processMessage(
	ctx context.Context,
	engine query.Engine,
	rule compiledRule,
	row query.MessageSummary,
) (int, error) {
	msg, err := engine.GetMessage(ctx, row.ID)
	if err != nil {
		return 0, fmt.Errorf("get message %d: %w", row.ID, err)
	}
	if msg == nil {
		return 0, nil
	}
	if !messageMatchesRule(msg, rule) {
		return 0, nil
	}

	fromAddrs := formatAddresses(msg.From)
	toAddrs := formatAddresses(msg.To)
	sort.Strings(fromAddrs)
	sort.Strings(toAddrs)

	var exported int
	for _, att := range msg.Attachments {
		if !attachmentMatchesRule(att, rule) {
			continue
		}
		key := makeSeenKey(msg.SourceMessageID, att.ContentHash)
		if _, seen := r.state.Attachments[key]; seen {
			continue
		}

		resultPath, metaPath, err := r.exportAttachment(msg, att, rule, fromAddrs, toAddrs)
		if err != nil {
			return exported, fmt.Errorf("export attachment %s from message %d: %w", att.Filename, msg.ID, err)
		}

		r.state.Attachments[key] = seenAttachment{
			RuleName:             rule.Name,
			SourceMessageID:      msg.SourceMessageID,
			MessageID:            msg.ID,
			AttachmentFilename:   att.Filename,
			AttachmentContentSHA: att.ContentHash,
			WealthfolioAccountID: rule.WealthfolioAccountID,
			PDFPath:              resultPath,
			MetadataPath:         metaPath,
			ExportedAt:           time.Now().UTC(),
		}
		exported++
	}

	return exported, nil
}

func (r *syncRunner) exportAttachment(
	msg *query.MessageDetail,
	att query.AttachmentInfo,
	rule compiledRule,
	fromAddrs []string,
	toAddrs []string,
) (string, string, error) {
	pdfName := buildOutputName(msg, att)
	pdfPath := filepath.Join(r.cfg.OutputDirAbs, pdfName)
	metaPath := strings.TrimSuffix(pdfPath, filepath.Ext(pdfPath)) + ".json"

	if r.dryRun {
		fmt.Printf("[dry-run] export %s -> %s\n", att.Filename, pdfPath)
		return pdfPath, metaPath, nil
	}

	srcPath, err := export.StoragePath(r.cfg.AttachmentsDir, att.ContentHash)
	if err != nil {
		return "", "", fmt.Errorf("resolve storage path: %w", err)
	}
	if err := copyFileAtomic(srcPath, pdfPath); err != nil {
		return "", "", err
	}

	meta := wealthfolioMetadata{
		Version:               1,
		RuleName:              rule.Name,
		WealthfolioAccountID:  rule.WealthfolioAccountID,
		MsgvaultAccount:       rule.MsgvaultAccount,
		MessageID:             msg.ID,
		SourceMessageID:       msg.SourceMessageID,
		ConversationID:        msg.ConversationID,
		SourceConversationID:  msg.SourceConversationID,
		Subject:               msg.Subject,
		From:                  fromAddrs,
		To:                    toAddrs,
		AttachmentFilename:    att.Filename,
		AttachmentMimeType:    att.MimeType,
		AttachmentContentHash: att.ContentHash,
		ExportedAt:            time.Now().UTC(),
	}
	if !msg.SentAt.IsZero() {
		meta.SentAt = msg.SentAt.UTC().Format(time.RFC3339)
	}
	if msg.ReceivedAt != nil {
		meta.ReceivedAt = msg.ReceivedAt.UTC().Format(time.RFC3339)
	}

	if err := writeJSONAtomic(metaPath, meta); err != nil {
		return "", "", fmt.Errorf("write metadata: %w", err)
	}

	return pdfPath, metaPath, nil
}

func (r *syncRunner) runRsync(ctx context.Context) error {
	args := append([]string{}, r.cfg.Delivery.RsyncArgs...)
	if len(args) == 0 {
		args = []string{"-a"}
	}
	args = append(args, r.cfg.OutputDirAbs+string(os.PathSeparator), r.cfg.Delivery.RsyncTarget)

	cmd := exec.CommandContext(ctx, "rsync", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if r.dryRun {
		fmt.Printf("[dry-run] rsync %s\n", strings.Join(args, " "))
		return nil
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rsync delivery failed: %w", err)
	}
	return nil
}

func resolveAccountIdentifiers(ctx context.Context, engine query.Engine) (map[string]int64, error) {
	accounts, err := engine.ListAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	out := make(map[string]int64, len(accounts))
	for _, account := range accounts {
		out[account.Identifier] = account.ID
	}
	return out, nil
}

func messageMatchesRule(msg *query.MessageDetail, rule compiledRule) bool {
	if rule.SubjectPattern != nil && !rule.SubjectPattern.MatchString(msg.Subject) {
		return false
	}
	if rule.FromPattern != nil {
		for _, addr := range msg.From {
			if rule.FromPattern.MatchString(addr.Email) || rule.FromPattern.MatchString(addr.Name) {
				return true
			}
		}
		return false
	}
	return true
}

func attachmentMatchesRule(att query.AttachmentInfo, rule compiledRule) bool {
	if !isPDFAttachment(att) {
		return false
	}
	if rule.AttachmentFilenamePattern != nil && !rule.AttachmentFilenamePattern.MatchString(att.Filename) {
		return false
	}
	return true
}

func isPDFAttachment(att query.AttachmentInfo) bool {
	if strings.EqualFold(att.MimeType, "application/pdf") {
		return true
	}
	return strings.EqualFold(filepath.Ext(att.Filename), ".pdf")
}

func formatAddresses(addrs []query.Address) []string {
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		if strings.TrimSpace(addr.Name) != "" {
			out = append(out, fmt.Sprintf("%s <%s>", addr.Name, addr.Email))
			continue
		}
		out = append(out, addr.Email)
	}
	return out
}

func buildOutputName(msg *query.MessageDetail, att query.AttachmentInfo) string {
	stamp := "unknown-date"
	if !msg.SentAt.IsZero() {
		stamp = msg.SentAt.UTC().Format("2006-01-02")
	}
	base := sanitizeFilename(strings.TrimSuffix(att.Filename, filepath.Ext(att.Filename)))
	if base == "" {
		base = "attachment"
	}
	sourceID := sanitizeFilename(msg.SourceMessageID)
	if sourceID == "" {
		sourceID = "message"
	}
	if len(sourceID) > 24 {
		sourceID = sourceID[:24]
	}
	return fmt.Sprintf("%s_%s_%s_%s.pdf", stamp, sourceID, att.ContentHash[:12], base)
}

func sanitizeFilename(name string) string {
	name = export.SanitizeFilename(strings.TrimSpace(name))
	name = strings.ReplaceAll(name, " ", "_")
	name = strings.Trim(name, "._")
	if name == "" {
		return ""
	}
	if len(name) > 80 {
		return name[:80]
	}
	return name
}

func copyFileAtomic(srcPath, dstPath string) error {
	if _, err := os.Stat(dstPath); err == nil {
		return fmt.Errorf("destination already exists: %s", dstPath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat destination: %w", err)
	}

	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open source attachment: %w", err)
	}
	defer func() { _ = src.Close() }()

	dir := filepath.Dir(dstPath)
	tmp, err := os.CreateTemp(dir, ".wealthfolio-sync-*.pdf")
	if err != nil {
		return fmt.Errorf("create temp output: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("copy attachment data: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp output: %w", err)
	}
	if err := os.Rename(tmpName, dstPath); err != nil {
		return fmt.Errorf("move attachment into outbox: %w", err)
	}
	return nil
}

func writeJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".wealthfolio-sync-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
