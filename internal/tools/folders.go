package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kacperkwapisz/mail-mcp/internal/config"
	"github.com/kacperkwapisz/mail-mcp/internal/mailbox"
)

type listFoldersInput struct {
	accountInput
}

type createFolderInput struct {
	accountInput
	Name string `json:"name" jsonschema:"folder name; use the account's hierarchy delimiter for nesting, e.g. INBOX/Projects"`
}

type renameFolderInput struct {
	accountInput
	OldName string `json:"old_name" jsonschema:"current folder name"`
	NewName string `json:"new_name" jsonschema:"new folder name"`
}

type deleteFolderInput struct {
	accountInput
	Name    string `json:"name" jsonschema:"folder to delete"`
	Confirm bool   `json:"confirm" jsonschema:"must be true. Deleting a folder discards every message in it and cannot be undone"`
}

type listFoldersOutput struct {
	Summary string           `json:"summary" jsonschema:"one-line description of the result"`
	Folders []mailbox.Folder `json:"folders" jsonschema:"every folder visible to the account"`
}

type folderOutput struct {
	Summary   string `json:"summary" jsonschema:"one-line description of the result"`
	Status    string `json:"status" jsonschema:"resulting state: created, renamed, or deleted"`
	AccountID string `json:"account_id" jsonschema:"account that was modified"`
	Name      string `json:"name" jsonschema:"folder the operation applied to"`
}

func (s *Server) registerFolders(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "list_folders",
		Title: "List folders",
		Description: "List an account's folders, each tagged with its role (inbox, sent, drafts, archive, trash, junk) where the server reports one. " +
			"Call this before searching or moving to learn the exact folder names, which vary by provider and language.",
		Annotations: readOnlyTool(),
	}, s.listFolders)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "create_folder",
		Title:       "Create a folder",
		Description: "Create a new folder.",
		Annotations: writeTool(),
	}, s.createFolder)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "rename_folder",
		Title:       "Rename a folder",
		Description: "Rename a folder. Any message_id referring to the old folder becomes invalid.",
		Annotations: writeTool(),
	}, s.renameFolder)

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "delete_folder",
		Title: "Delete a folder",
		Description: "Delete a folder and everything in it. Requires confirm: true and an account with deletion enabled. " +
			"Refuses to touch INBOX or a special-use folder such as Sent, Drafts, or Trash.",
		Annotations: destructiveTool(),
	}, s.deleteFolder)
}

func (s *Server) listFolders(ctx context.Context, _ *mcp.CallToolRequest, in listFoldersInput) (*mcp.CallToolResult, listFoldersOutput, error) {
	acc, err := s.resolveAccount(in.AccountID)
	if err != nil {
		return nil, listFoldersOutput{}, err
	}

	var folders []mailbox.Folder
	err = s.withSession(ctx, acc, func(sess *mailbox.Session) error {
		var err error
		folders, err = sess.ListMailboxes()
		return err
	})
	if err != nil {
		return nil, listFoldersOutput{}, err
	}

	return nil, listFoldersOutput{
		Summary: pluralize(len(folders), "folder", "folders") + " in " + acc.ID,
		Folders: folders,
	}, nil
}

func (s *Server) createFolder(ctx context.Context, _ *mcp.CallToolRequest, in createFolderInput) (*mcp.CallToolResult, folderOutput, error) {
	acc, err := s.resolveAccount(in.AccountID)
	if err != nil {
		return nil, folderOutput{}, err
	}
	if err := validateFolderName(in.Name); err != nil {
		return nil, folderOutput{}, err
	}

	err = s.withSession(ctx, acc, func(sess *mailbox.Session) error {
		return sess.CreateMailbox(in.Name)
	})
	if err != nil {
		return nil, folderOutput{}, err
	}

	return nil, folderOutput{
		Summary:   fmt.Sprintf("created folder %s", in.Name),
		Status:    "created",
		AccountID: acc.ID,
		Name:      in.Name,
	}, nil
}

func (s *Server) renameFolder(ctx context.Context, _ *mcp.CallToolRequest, in renameFolderInput) (*mcp.CallToolResult, folderOutput, error) {
	acc, err := s.resolveAccount(in.AccountID)
	if err != nil {
		return nil, folderOutput{}, err
	}
	if err := validateFolderName(in.NewName); err != nil {
		return nil, folderOutput{}, err
	}
	if err := s.guardProtectedFolder(ctx, acc, in.OldName, "rename"); err != nil {
		return nil, folderOutput{}, err
	}

	err = s.withSession(ctx, acc, func(sess *mailbox.Session) error {
		return sess.RenameMailbox(in.OldName, in.NewName)
	})
	if err != nil {
		return nil, folderOutput{}, err
	}

	return nil, folderOutput{
		Summary:   fmt.Sprintf("renamed %s to %s", in.OldName, in.NewName),
		Status:    "renamed",
		AccountID: acc.ID,
		Name:      in.NewName,
	}, nil
}

func (s *Server) deleteFolder(ctx context.Context, _ *mcp.CallToolRequest, in deleteFolderInput) (*mcp.CallToolResult, folderOutput, error) {
	acc, err := s.resolveAccount(in.AccountID)
	if err != nil {
		return nil, folderOutput{}, err
	}
	if err := s.requireDelete(acc); err != nil {
		return nil, folderOutput{}, err
	}
	if !in.Confirm {
		return nil, folderOutput{}, fmt.Errorf(
			"delete_folder requires confirm: true. Every message in %q would be discarded, "+
				"so the user must confirm explicitly", in.Name)
	}
	if err := s.guardProtectedFolder(ctx, acc, in.Name, "delete"); err != nil {
		return nil, folderOutput{}, err
	}

	err = s.withSession(ctx, acc, func(sess *mailbox.Session) error {
		return sess.DeleteMailbox(in.Name)
	})
	if err != nil {
		return nil, folderOutput{}, err
	}

	return nil, folderOutput{
		Summary:   fmt.Sprintf("deleted folder %s", in.Name),
		Status:    "deleted",
		AccountID: acc.ID,
		Name:      in.Name,
	}, nil
}

// guardProtectedFolder blocks operations on INBOX and special-use folders.
//
// The role is read from the live folder list rather than a hardcoded name
// list, so a Polish "Wysłane" or a Gmail "[Gmail]/Sent Mail" is protected
// just as well as "Sent".
func (s *Server) guardProtectedFolder(ctx context.Context, acc *config.Account, name, verb string) error {
	if strings.EqualFold(strings.TrimSpace(name), "INBOX") {
		return fmt.Errorf("refusing to %s INBOX", verb)
	}

	var role string
	err := s.withSession(ctx, acc, func(sess *mailbox.Session) error {
		folders, err := sess.ListMailboxes()
		if err != nil {
			return err
		}
		for _, f := range folders {
			if f.Name == name {
				role = f.Role
				return nil
			}
		}
		return fmt.Errorf("folder %q does not exist", name)
	})
	if err != nil {
		return err
	}
	if role != "" && role != "archive" {
		return fmt.Errorf("refusing to %s %q: it is the account's %s folder", verb, name, role)
	}
	return nil
}

func validateFolderName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("folder name is required")
	}
	if strings.ContainsAny(name, "\r\n\x00") {
		return fmt.Errorf("folder name must not contain control characters")
	}
	if len(name) > 255 {
		return fmt.Errorf("folder name is %d characters, over the 255 limit", len(name))
	}
	return nil
}
