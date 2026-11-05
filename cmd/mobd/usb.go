package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/xrdavies/mobd/internal/usb"
)

// usbCommand is `mobd usb`: whether this computer can import shared USB devices, and setting up
// the root helper that attaches them (docs/usb-redirection.md).
func usbCommand(args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	if len(args) != 0 {
		return errors.New("usage: mobd usb [status | setup | remove]")
	}
	switch sub {
	case "status":
		r := usb.New(usb.NewImporter(), nil).Readiness()
		if r.Ready {
			fmt.Printf("USB import: ready (%s)\n", r.Backend)
		} else {
			fmt.Printf("USB import: not ready: %s\n", r.Detail)
		}
		return nil
	case "setup", "remove":
		uid, err := usb.SetupUser()
		if err != nil {
			return err
		}
		if sub == "setup" {
			return usb.Setup(uid, os.Stdout)
		}
		return usb.Remove(uid, os.Stdout)
	default:
		return errors.New("usage: mobd usb [status | setup | remove]")
	}
}

// usbHelper is the root helper itself, started by systemd (mobd-usb-helper@UID.service).
func usbHelper(args []string) error {
	fs := flag.NewFlagSet("usb-helper", flag.ContinueOnError)
	uid := fs.Int("uid", -1, "the user mobd runs as")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *uid <= 0 {
		return errors.New("usage: mobd usb-helper --uid UID (run by `sudo mobd usb setup`)")
	}
	return usb.RunHelper(*uid, usb.HelperSocket(*uid))
}
