package managedproxy

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func tryLockPreflight(f *os.File) (bool, error) {
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}

func ownedByCurrentUser(path string, _ os.FileInfo) bool {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false
	}
	owner, _, err := sd.Owner()
	return err == nil && owner != nil && windows.EqualSid(owner, user.User.Sid)
}

func preflightProcessStart(pid int) (uint64, string, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, "", err
	}
	defer windows.CloseHandle(handle)
	_, created, err := identifyCore(handle)
	return uint64(created), "", err
}

// The handle pins the process while its image and creation time are checked,
// as in stopWindowsCore.
func stopPreflightProcess(record preflightCoreRecord, core string) error {
	expected := windowsCoreRecord{PID: uint32(record.PID), Executable: core, Created: int64(record.Started)}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, expected.PID)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return nil
	}
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) && !recordedCoreRunning(expected) {
		return nil
	}
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	if exited, err := windows.WaitForSingleObject(handle, 0); err == nil && exited == windows.WAIT_OBJECT_0 {
		return nil
	}
	image, created, err := identifyCore(handle)
	if err != nil {
		return err
	}
	if !sameCore(expected, image, created) {
		return nil
	}
	if err := windows.TerminateProcess(handle, 0); err != nil {
		return err
	}
	if state, err := windows.WaitForSingleObject(handle, 5000); err != nil {
		return err
	} else if state != windows.WAIT_OBJECT_0 {
		return errors.New("Cage 代理进程未能及时停止")
	}
	return nil
}
