package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
	"github.com/xrdavies/mobd/internal/control"
	"github.com/xrdavies/mobd/internal/pairing"
)

func manage(command string, args []string) error {
	socket, err := control.DefaultPath()
	if err != nil {
		return err
	}
	if value := os.Getenv("MOBD_CONTROL_SOCKET"); value != "" {
		socket = value
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.StringVar(&socket, "socket", socket, "Running mobd management socket")
	var address string
	var closeInvites, confirm bool
	if command == "link" {
		fs.StringVar(&address, "url", "", "Client-accessible HTTP or HTTPS origin")
		fs.BoolVar(&closeInvites, "close", false, "Invalidate all unused invitations")
	}
	if command == "auth" {
		fs.BoolVar(&confirm, "yes", false, "Confirm reset of all client credentials")
	}
	// Subcommands precede flags: clients revoke ID --socket PATH, auth reset --yes.
	var action, id string
	if command == "clients" || command == "auth" {
		if len(args) == 0 {
			return fmt.Errorf("%s requires a subcommand", command)
		}
		action, args = args[0], args[1:]
		if command == "clients" && (action == "revoke" || action == "delete") {
			if len(args) == 0 {
				return errors.New("client ID is required")
			}
			id, args = args[0], args[1:]
		}
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected management arguments")
	}
	switch command {
	case "link":
		if closeInvites {
			if address != "" {
				return errors.New("use either --close or --url")
			}
			if err := control.Call(socket, "DELETE", "/invitations", nil, nil); err != nil {
				return err
			}
			fmt.Println("Unused invitations closed; paired clients remain authorized.")
			return nil
		}
		if address == "" {
			return errors.New("usage: mobd link --url https://office.example.com [--socket PATH]")
		}
		var invite pairing.Invitation
		if err := control.Call(socket, "POST", "/invitations", map[string]string{"url": address}, &invite); err != nil {
			return err
		}
		code, err := qrcode.New(invite.URL, qrcode.Medium)
		if err != nil {
			return err
		}
		if strings.HasPrefix(invite.URL, "http://") {
			fmt.Fprintln(os.Stderr, "Warning: HTTP does not encrypt pairing invitations, device credentials or session data. Without an encrypted tunnel such as Tailscale or SSH, network observers may capture them. Pairing authentication is still required.")
		}
		fmt.Printf("%s\n%s\nExpires: %s (one use)\n", code.ToSmallString(false), invite.URL, invite.ExpiresAt.Format(time.RFC3339))
		return nil
	case "connections":
		return printManagement(socket, "/connections")
	case "clients":
		switch action {
		case "list":
			return printManagement(socket, "/clients")
		case "revoke", "delete":
			if id == "" {
				return errors.New("client ID is required")
			}
			path, method := "/clients/"+url.PathEscape(id), "DELETE"
			if action == "revoke" {
				path += "/revoke"
				method = "POST"
			}
			if err := control.Call(socket, method, path, nil, nil); err != nil {
				return err
			}
			fmt.Printf("Client %s: %s completed.\n", id, action)
			return nil
		}
	case "auth":
		if action != "reset" || !confirm {
			return errors.New("usage: mobd auth reset --yes [--socket PATH] (invalidates all clients and invitations)")
		}
		if err := control.Call(socket, "POST", "/reset", nil, nil); err != nil {
			return err
		}
		fmt.Println("Authentication reset; all clients must pair again. Conversations and tasks are preserved.")
		return nil
	}
	return errors.New("usage: mobd clients list | revoke CLIENT_ID | delete CLIENT_ID")
}

func printManagement(socket, path string) error {
	var result json.RawMessage
	if err := control.Call(socket, "GET", path, nil, &result); err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}
