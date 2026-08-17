package tools

import (
	"context"
	"fmt"
	"strings"

	imap "github.com/emersion/go-imap/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kacperkwapisz/mail-mcp/internal/mailbox"
	"github.com/kacperkwapisz/mail-mcp/internal/msgid"
)

type archiveInput struct {
	messageInput
}

type moveInput struct {
	messageInput
	ToFolder string `json:"to_folder" jsonschema:"destination folder; must already exist unless you call create_folder first"`
}

type markInput struct {
	messageInput
	Action string `json:"action" jsonschema:"one of: read, unread, flagged, unflagged, answered, unanswered"`
}

type deleteInput struct {
	messageInput
	Confirm bool `json:"confirm" jsonschema:"must be true. Deleting requires explicit confirmation from the user, not an assumption"`
}

type moveOutput struct {
	Summary   string `json:"summary" jsonschema:"one-line description of the result"`
	Status    string `json:"status" jsonschema:"always \"moved\" on success"`
	AccountID string `json:"account_id" jsonschema:"account the message belongs to"`
	From      string `json:"from_folder" jsonschema:"folder the message came from"`
	To        string `json:"to_folder" jsonschema:"folder the message now lives in"`
	// NewMessageID is the handle in the destination folder, when the server
	// reported one. Absent servers force a fresh search to obtain it.
	NewMessageID string `json:"new_message_id,omitempty" jsonschema:"updated handle in the destination folder; the old handle is now invalid"`
}

type markOutput struct {
	Summary   string `json:"summary" jsonschema:"one-line description of the result"`
	Status    string `json:"status" jsonschema:"always \"updated\" on success"`
	AccountID string `json:"account_id" jsonschema:"account the message belongs to"`
	Action    string `json:"action" jsonschema:"action that was applied"`
}

func (s *Server) registerManage(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "archive_email",
		Title: "Archive a message",
		Description: "Move a message out of its current folder into the account's Archive folder, creating that folder if the account has none. " +
			"Prefer this over delete_email for anything the user might want later.",
		Annotations: writeTool(),
	}, s.archiveEmail)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "move_email",
		Title:       "Move a message",
		Description: "Move a message to another folder. The message_id changes as a result; use the returned new_message_id, or search again.",
		Annotations: writeTool(),
	}, s.moveEmail)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "mark_email",
		Title:       "Change message flags",
		Description: "Mark a message read, unread, flagged, unflagged, answered, or unanswered.",
		Annotations: writeTool(),
	}, s.markEmail)

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "delete_email",
		Title: "Delete a message",
		Description: "Move a message to Trash. Requires confirm: true and an account with deletion enabled. " +
			"Nothing is erased — the message goes to the Trash folder — but prefer archive_email unless the user explicitly asked to delete.",
		Annotations: destructiveTool(),
	}, s.deleteEmail)
}

func (s *Server) archiveEmail(ctx context.Context, _ *mcp.CallToolRequest, in archiveInput) (*mcp.CallToolResult, moveOutput, error) {
	return s.relocate(ctx, in.MessageID, relocateOptions{
		role:          "archive",
		fallbackName:  "Archive",
		createMissing: true,
		verb:          "archived",
	})
}

func (s *Server) moveEmail(ctx context.Context, _ *mcp.CallToolRequest, in moveInput) (*mcp.CallToolResult, moveOutput, error) {
	if strings.TrimSpace(in.ToFolder) == "" {
		return nil, moveOutput{}, fmt.Errorf("to_folder is required")
	}
	return s.relocate(ctx, in.MessageID, relocateOptions{
		explicitFolder: in.ToFolder,
		verb:           "moved",
	})
}

func (s *Server) deleteEmail(ctx context.Context, _ *mcp.CallToolRequest, in deleteInput) (*mcp.CallToolResult, moveOutput, error) {
	if !in.Confirm {
		return nil, moveOutput{}, fmt.Errorf(
			"delete_email requires confirm: true. Ask the user to confirm the deletion, " +
				"or use archive_email if they only want it out of the inbox")
	}
	_, acc, err := s.resolveMessage(in.MessageID)
	if err != nil {
		return nil, moveOutput{}, err
	}
	if err := s.requireDelete(acc); err != nil {
		return nil, moveOutput{}, err
	}
	return s.relocate(ctx, in.MessageID, relocateOptions{
		role:          "trash",
		fallbackName:  "Trash",
		createMissing: true,
		verb:          "moved to Trash",
	})
}

type relocateOptions struct {
	// explicitFolder wins when set; otherwise role is looked up.
	explicitFolder string
	role           string
	fallbackName   string
	createMissing  bool
	verb           string
}

// relocate is the shared implementation behind archive, move, and delete.
func (s *Server) relocate(ctx context.Context, handle string, opts relocateOptions) (*mcp.CallToolResult, moveOutput, error) {
	id, acc, err := s.resolveMessage(handle)
	if err != nil {
		return nil, moveOutput{}, err
	}

	out := moveOutput{Status: "moved", AccountID: acc.ID, From: id.Mailbox}

	err = s.withSession(ctx, acc, func(sess *mailbox.Session) error {
		dest := opts.explicitFolder
		if dest == "" {
			detected, err := sess.FolderForRole(opts.role)
			if err != nil {
				return err
			}
			dest = firstNonEmpty(detected, opts.fallbackName)
		}
		if dest == id.Mailbox {
			return fmt.Errorf("the message is already in %q", dest)
		}
		if opts.createMissing {
			if err := sess.EnsureMailbox(dest); err != nil {
				return err
			}
		}

		if err := sess.SelectFor(id, false); err != nil {
			return err
		}
		newUID, err := sess.Move(imap.UID(id.UID), dest)
		if err != nil {
			return err
		}

		out.To = dest
		if newUID != 0 {
			// A fresh UIDVALIDITY read is needed: the destination folder is a
			// different mailbox with its own numbering.
			if destValidity, err := sess.Select(dest, true); err == nil {
				out.NewMessageID = msgid.Encode(acc.ID, dest, destValidity, uint32(newUID))
			}
		}
		return nil
	})
	if err != nil {
		return nil, moveOutput{}, err
	}

	out.Summary = fmt.Sprintf("%s to %s", opts.verb, out.To)
	if out.NewMessageID == "" {
		out.Summary += " (search again for its new message_id)"
	}
	return nil, out, nil
}

// flagActions maps a user-facing verb to the flag and whether to add it.
var flagActions = map[string]struct {
	flag imap.Flag
	add  bool
}{
	"read":       {imap.FlagSeen, true},
	"unread":     {imap.FlagSeen, false},
	"flagged":    {imap.FlagFlagged, true},
	"unflagged":  {imap.FlagFlagged, false},
	"answered":   {imap.FlagAnswered, true},
	"unanswered": {imap.FlagAnswered, false},
}

func (s *Server) markEmail(ctx context.Context, _ *mcp.CallToolRequest, in markInput) (*mcp.CallToolResult, markOutput, error) {
	action := strings.ToLower(strings.TrimSpace(in.Action))
	spec, ok := flagActions[action]
	if !ok {
		valid := make([]string, 0, len(flagActions))
		for k := range flagActions {
			valid = append(valid, k)
		}
		return nil, markOutput{}, fmt.Errorf("unknown action %q; use one of: %s", in.Action, strings.Join(valid, ", "))
	}

	id, acc, err := s.resolveMessage(in.MessageID)
	if err != nil {
		return nil, markOutput{}, err
	}

	err = s.withSession(ctx, acc, func(sess *mailbox.Session) error {
		if err := sess.SelectFor(id, false); err != nil {
			return err
		}
		return sess.StoreFlags(imap.UID(id.UID), []imap.Flag{spec.flag}, spec.add)
	})
	if err != nil {
		return nil, markOutput{}, err
	}

	return nil, markOutput{
		Summary:   fmt.Sprintf("marked %s", action),
		Status:    "updated",
		AccountID: acc.ID,
		Action:    action,
	}, nil
}
