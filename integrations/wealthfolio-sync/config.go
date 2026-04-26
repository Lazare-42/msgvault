package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
	mvconfig "github.com/wesm/msgvault/internal/config"
)

const defaultPageSize = 100

type Config struct {
	MsgvaultHome string         `toml:"msgvault_home"`
	OutputDir    string         `toml:"output_dir"`
	StateFile    string         `toml:"state_file"`
	PageSize     int            `toml:"page_size"`
	Delivery     DeliveryConfig `toml:"delivery"`
	Rules        []RuleConfig   `toml:"rule"`
}

type DeliveryConfig struct {
	RsyncTarget string   `toml:"rsync_target"`
	RsyncArgs   []string `toml:"rsync_args"`
}

type RuleConfig struct {
	Name                    string `toml:"name"`
	Query                   string `toml:"query"`
	MsgvaultAccount         string `toml:"msgvault_account"`
	WealthfolioAccountID    string `toml:"wealthfolio_account_id"`
	AttachmentFilenameRegex string `toml:"attachment_filename_regex"`
	SubjectRegex            string `toml:"subject_regex"`
	FromRegex               string `toml:"from_regex"`
	MaxMessages             int    `toml:"max_messages"`
}

type compiledConfig struct {
	Config
	MsgvaultHomeAbs string
	OutputDirAbs    string
	StateFileAbs    string
	DatabasePath    string
	AttachmentsDir  string
	Rules           []compiledRule
}

type compiledRule struct {
	RuleConfig
	AttachmentFilenamePattern *regexp.Regexp
	SubjectPattern            *regexp.Regexp
	FromPattern               *regexp.Regexp
}

func loadConfig(path string) (*compiledConfig, error) {
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}

	if cfg.MsgvaultHome == "" {
		cfg.MsgvaultHome = mvconfig.DefaultHome()
	}
	if cfg.OutputDir == "" {
		cfg.OutputDir = filepath.Join(cfg.MsgvaultHome, "wealthfolio-outbox")
	}
	if cfg.StateFile == "" {
		cfg.StateFile = filepath.Join(cfg.MsgvaultHome, "wealthfolio-sync-state.json")
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = defaultPageSize
	}
	if len(cfg.Rules) == 0 {
		return nil, fmt.Errorf("config must contain at least one [[rule]]")
	}
	if cfg.PageSize > 1000 {
		return nil, fmt.Errorf("page_size too large: %d", cfg.PageSize)
	}

	msgvaultHomeAbs, err := filepath.Abs(expandPath(cfg.MsgvaultHome))
	if err != nil {
		return nil, fmt.Errorf("resolve msgvault_home: %w", err)
	}
	outputDirAbs, err := filepath.Abs(expandPath(cfg.OutputDir))
	if err != nil {
		return nil, fmt.Errorf("resolve output_dir: %w", err)
	}
	stateFileAbs, err := filepath.Abs(expandPath(cfg.StateFile))
	if err != nil {
		return nil, fmt.Errorf("resolve state_file: %w", err)
	}

	cc := &compiledConfig{
		Config:          cfg,
		MsgvaultHomeAbs: msgvaultHomeAbs,
		OutputDirAbs:    outputDirAbs,
		StateFileAbs:    stateFileAbs,
		DatabasePath:    filepath.Join(msgvaultHomeAbs, "msgvault.db"),
		AttachmentsDir:  filepath.Join(msgvaultHomeAbs, "attachments"),
	}

	if _, err := os.Stat(cc.DatabasePath); err != nil {
		return nil, fmt.Errorf("msgvault database not found at %s: %w", cc.DatabasePath, err)
	}
	if _, err := os.Stat(cc.AttachmentsDir); err != nil {
		return nil, fmt.Errorf("msgvault attachments dir not found at %s: %w", cc.AttachmentsDir, err)
	}

	cc.Rules = make([]compiledRule, 0, len(cfg.Rules))
	for idx, rule := range cfg.Rules {
		cr, err := compileRule(rule)
		if err != nil {
			return nil, fmt.Errorf("rule %d (%q): %w", idx+1, rule.Name, err)
		}
		cc.Rules = append(cc.Rules, cr)
	}

	if err := os.MkdirAll(cc.OutputDirAbs, 0o755); err != nil {
		return nil, fmt.Errorf("create output_dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cc.StateFileAbs), 0o755); err != nil {
		return nil, fmt.Errorf("create state_file dir: %w", err)
	}

	return cc, nil
}

func compileRule(rule RuleConfig) (compiledRule, error) {
	if strings.TrimSpace(rule.Name) == "" {
		return compiledRule{}, fmt.Errorf("name is required")
	}
	if strings.TrimSpace(rule.Query) == "" {
		return compiledRule{}, fmt.Errorf("query is required")
	}
	if strings.TrimSpace(rule.WealthfolioAccountID) == "" {
		return compiledRule{}, fmt.Errorf("wealthfolio_account_id is required")
	}
	if rule.MaxMessages < 0 {
		return compiledRule{}, fmt.Errorf("max_messages cannot be negative")
	}

	cr := compiledRule{RuleConfig: rule}
	var err error
	if rule.AttachmentFilenameRegex != "" {
		cr.AttachmentFilenamePattern, err = regexp.Compile(rule.AttachmentFilenameRegex)
		if err != nil {
			return compiledRule{}, fmt.Errorf("compile attachment_filename_regex: %w", err)
		}
	}
	if rule.SubjectRegex != "" {
		cr.SubjectPattern, err = regexp.Compile(rule.SubjectRegex)
		if err != nil {
			return compiledRule{}, fmt.Errorf("compile subject_regex: %w", err)
		}
	}
	if rule.FromRegex != "" {
		cr.FromPattern, err = regexp.Compile(rule.FromRegex)
		if err != nil {
			return compiledRule{}, fmt.Errorf("compile from_regex: %w", err)
		}
	}
	return cr, nil
}

func expandPath(path string) string {
	if path == "" || path[0] != '~' {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}
