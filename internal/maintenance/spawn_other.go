//go:build !darwin && !linux

package maintenance

import "errors"

func (h Helper) Start(string) error {
	return errors.New("restarting from the apps needs mobd's background service, not available on this system yet")
}
