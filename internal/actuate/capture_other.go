//go:build !darwin

package actuate

import "context"

// Capture is unavailable off macOS.
func Capture(ctx context.Context) ([]byte, error) {
	return nil, ErrUnsupported
}
