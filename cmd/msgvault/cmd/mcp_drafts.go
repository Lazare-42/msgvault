package cmd

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
)

var mcpDraftCommandConstructors = map[string]func() *cobra.Command{
	api.CLIRunDraftReplyCommand:   newDraftReplyCommand,
	api.CLIRunDraftComposeCommand: newDraftComposeCommand,
	api.CLIRunDraftForwardCommand: newDraftForwardCommand,
	api.CLIRunDraftGetCommand:     newDraftGetCommand,
	api.CLIRunDraftEditCommand:    newDraftEditCommand,
	api.CLIRunDraftDeleteCommand:  newDraftDeleteCommand,
	api.CLIRunDraftRecoverCommand: newDraftRecoverCommand,
	api.CLIRunDraftSendAsCommand:  newDraftSendAsCommand,
}

func mcpDraftCommands(delegated bool) []string {
	commands := slices.Sorted(maps.Keys(mcpDraftCommandConstructors))
	if delegated {
		commands = slices.DeleteFunc(commands, func(command string) bool { return !agentDelegatedCommand(command) })
	}
	return commands
}

type daemonMCPDraftRunner struct{ client *daemonclient.Client }

func (r daemonMCPDraftRunner) RunDraftCommand(ctx context.Context, request mcpserver.DraftCommandRequest) (mcpserver.DraftCommandResult, error) {
	var result mcpserver.DraftCommandResult
	constructor, ok := mcpDraftCommandConstructors[request.Command]
	if !ok {
		return result, fmt.Errorf("unknown draft command %q", request.Command)
	}
	if strings.HasPrefix(request.Positional, "-") {
		return result, &mcpserver.DraftCommandError{Message: "invalid_args: positional value must not begin with \"-\""}
	}
	command := constructor()
	for _, name := range slices.Sorted(maps.Keys(request.Flags)) {
		for _, value := range request.Flags[name] {
			if err := command.Flags().Set(name, value); err != nil {
				return result, &mcpserver.DraftCommandError{Message: err.Error()}
			}
		}
	}
	if err := command.Flags().Set("json", "true"); err != nil {
		return result, fmt.Errorf("set draft JSON output: %w", err)
	}
	var positional []string
	if request.Positional != "" {
		positional = []string{request.Positional}
	}
	if err := command.ValidateArgs(positional); err != nil {
		return result, &mcpserver.DraftCommandError{Message: err.Error()}
	}
	if err := command.ValidateRequiredFlags(); err != nil {
		return result, &mcpserver.DraftCommandError{Message: err.Error()}
	}
	args, err := daemonCLIArgsFromCobra(command, positional)
	if err != nil {
		return result, err
	}
	var stdout, stderr strings.Builder
	err = r.client.RunCLICommand(ctx, daemonclient.CLIRunRequest{Args: args}, func(stream, data string) error {
		switch stream {
		case cliStreamStdout:
			stdout.WriteString(data)
		case cliStreamStderr:
			stderr.WriteString(data)
		}
		return nil
	})
	result = mcpserver.DraftCommandResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if _, reported := errors.AsType[*daemonclient.CLIRunError](err); reported {
		return result, &mcpserver.DraftCommandError{Message: err.Error(), Stderr: result.Stderr}
	}
	return result, err
}

// hideUnusedManagedDraftTools drops the daemon-managed draft_* tools from an
// owner session that already has the live mail tools (create_draft and
// friends) and no enabled [[imap.drafts]], [[gmail.drafts]], or
// [[beeper.drafts]] grant. Without a grant, creating or editing a managed
// draft returns draft_disabled, and listing both families leads agents to the
// wrong one. draft_send_as needs no grant, so it stays listed. Only a local
// daemon is known to read this same config; a remote daemon keeps its own
// grants, so its draft tools stay listed.
func hideUnusedManagedDraftTools(opts *mcpserver.ServeOptions, cfg *config.Config) {
	if opts.GmailFactory == nil || cfg == nil || cfg.Remote.URL != "" || managedDraftGrantsEnabled(cfg) {
		return
	}
	opts.DraftCommands = slices.DeleteFunc(opts.DraftCommands, func(command string) bool { return command != "draft-send-as" })
}

func managedDraftGrantsEnabled(cfg *config.Config) bool {
	for _, grant := range cfg.IMAP.Drafts {
		if grant.Enabled {
			return true
		}
	}
	for _, grants := range [][]config.GmailDraftSource{cfg.Gmail.Drafts, cfg.Beeper.Drafts} {
		for _, grant := range grants {
			if grant.Enabled {
				return true
			}
		}
	}
	return false
}
