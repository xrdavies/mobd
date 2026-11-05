//go:build !linux

package usb

import (
	"errors"
	"io"
	"runtime"

	"github.com/xrdavies/mobd/internal/usbip"
)

// NewImporter has no importer outside Linux yet (docs/usb-redirection.md#milestones-and-acceptance).
func NewImporter() Importer { return none{} }

type none struct{}

var errUnsupported = errors.New(unsupported())

func unsupported() string {
	if runtime.GOOS == "darwin" {
		return "importing USB devices on macOS needs Apple's USB host-controller entitlement, not available yet; Linux office computers support it"
	}
	return "importing USB devices is not supported on this system yet; Linux office computers support it"
}

func (none) Backend() string                       { return "" }
func (none) Ready() (bool, string)                 { return false, unsupported() }
func (none) Attach(int, usbip.Device) (int, error) { return 0, errUnsupported }
func (none) LocalBusID(int) (string, error)        { return "", nil }
func (none) Detach(int) error                      { return nil }
func (none) Busy(int) string                       { return "" }
func SetupUser() (int, error)                      { return 0, errUnsupported }
func Setup(int, io.Writer) error                   { return errUnsupported }
func Remove(int, io.Writer) error                  { return errUnsupported }
func RunHelper(int, string) error                  { return errUnsupported }
func HelperSocket(int) string                      { return "" }
