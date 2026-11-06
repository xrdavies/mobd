//go:build !unix

package usb

import (
	"errors"
	"os"
)

func socketPair() (kernel, bridge *os.File, err error) {
	return nil, nil, errors.New("USB import is not supported on this system")
}
