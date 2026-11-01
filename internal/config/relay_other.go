//go:build !unix

package config

import "os"

// No desktop backend runs on these systems yet; the file is only required to be readable.
func checkSecretFile(os.FileInfo) error { return nil }
