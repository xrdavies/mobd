//go:build windows

package control

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// privateDir gives the socket's directory a protected DACL: full control for this user and
// SYSTEM, nothing inherited, so other users cannot reach the socket. The socket file inherits it.
func privateDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("control socket directory is not a directory")
	}
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:P(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)", user.User.Sid))
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

// privateSocket: the socket inherits the directory's DACL; Unix permission bits do not apply.
func privateSocket(string) error { return nil }
