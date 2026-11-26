package assertion

import (
	"errors"
	"os"
	"runtime"
	"syscall"
	"testing"
)

// symlink.go creates filesystem links for tests that exercise link resolution.
// On Windows, the account may lack the privilege needed to create a link.
// Other setup errors fail the test at the caller's location.

// windowsPrivilegeNotHeld identifies the Windows error for a missing privilege
// when a test process asks the filesystem to create a symbolic link.
const windowsPrivilegeNotHeld syscall.Errno = 1314

// Symlink creates the requested link or skips the current test when Windows
// denies the symbolic-link privilege. Call it in a subtest when neighboring
// assertions can run without a link.
func Symlink(t testing.TB, target, link string) {
	t.Helper()
	err := os.Symlink(target, link)
	if runtime.GOOS == "windows" && errors.Is(err, windowsPrivilegeNotHeld) {
		t.Skipf("symbolic link privilege unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("create fixture symlink: %v", err)
	}
}
