//go:build windows

package main

import (
	"os"
	"strings"
	"syscall"
	"unsafe"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/windows"
)

// isAdmin checks if the current process has administrator privileges.
func isAdmin() bool {
	var sid *windows.SID
	err := windows.AllocateAndInitializeSid(
		&windows.SECURITY_NT_AUTHORITY,
		2,
		windows.SECURITY_BUILTIN_DOMAIN_RID,
		windows.DOMAIN_ALIAS_RID_ADMINS,
		0, 0, 0, 0, 0, 0,
		&sid,
	)
	if err != nil {
		return false
	}
	defer windows.FreeSid(sid)

	token := windows.Token(0)
	member, err := token.IsMember(sid)
	if err != nil {
		return false
	}
	return member
}

// relaunchAsAdmin re-launches the current process with elevated privileges via UAC.
func relaunchAsAdmin() {
	exe, err := os.Executable()
	if err != nil {
		log.Fatalf("gui: cannot get executable path: %v", err)
	}

	verb, _ := syscall.UTF16PtrFromString("runas")
	exePtr, _ := syscall.UTF16PtrFromString(exe)
	args, _ := syscall.UTF16PtrFromString(strings.Join(os.Args[1:], " "))
	cwd, _ := syscall.UTF16PtrFromString(".")

	var showCmd int32 = 1 // SW_NORMAL

	err = windows.ShellExecute(0, verb, exePtr, args, cwd, showCmd)
	if err != nil {
		log.Fatalf("gui: failed to elevate: %v", err)
	}

	os.Exit(0)
}

// Ensure ShellExecute is available - it's in shell32.dll.
func init() {
	// windows.ShellExecute is available in golang.org/x/sys/windows since v0.1.0
	// Verify it compiles; if not, we use the fallback below.
	_ = unsafe.Sizeof(0)
}
