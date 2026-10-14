//go:build !darwin

package actuate

func requireDarwin() error { return ErrUnsupported }

// CurrentPermissions is unavailable off macOS.
func CurrentPermissions() (Permissions, error) { return Permissions{}, ErrUnsupported }

func click(x, y float64, right bool, count int) error { return ErrUnsupported }

func pressKey(code int, modifiers []string) error { return ErrUnsupported }

func displayScale() float64 { return 1 }

// ListWindows is unavailable off macOS.
func ListWindows(onScreenOnly bool) ([]Window, error) { return nil, ErrUnsupported }
