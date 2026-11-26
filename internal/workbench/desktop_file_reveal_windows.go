package workbench

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	fileShell                = windows.NewLazySystemDLL("shell32.dll")
	parseDisplayName         = fileShell.NewProc("SHParseDisplayName")
	openFolderAndSelectItems = fileShell.NewProc("SHOpenFolderAndSelectItems")
)

func revealHostFile(ctx context.Context, path string) error {
	// Shell extensions can block a COM call. The one-shot desktop helper must
	// still complete its request and exit when the file action times out.
	done := make(chan error, 1)
	go func() { done <- selectShellItem(ctx, path) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return errors.New("本机文件管理器响应超时，请稍后重试")
	}
}

func selectShellItem(ctx context.Context, path string) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// COM initialization and cleanup belong to the same thread. S_FALSE
	// also succeeds and requires a matching CoUninitialize.
	if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED); err != nil && err != windows.Errno(windows.S_FALSE) {
		return fmt.Errorf("无法初始化本机文件管理器：%w", err)
	}
	defer windows.CoUninitialize()
	var item unsafe.Pointer
	hr, _, _ := parseDisplayName.Call(uintptr(unsafe.Pointer(name)), 0, uintptr(unsafe.Pointer(&item)), 0, 0)
	if int32(hr) < 0 {
		return fmt.Errorf("无法定位本机文件（HRESULT 0x%08X）", uint32(hr))
	}
	defer windows.CoTaskMemFree(item)
	if err := ctx.Err(); err != nil {
		return err
	}
	// With zero children, the Shell opens the parent and selects this item.
	hr, _, _ = openFolderAndSelectItems.Call(uintptr(item), 0, 0, 0)
	if int32(hr) < 0 {
		return fmt.Errorf("无法在本机文件管理器中显示此项目（HRESULT 0x%08X）", uint32(hr))
	}
	return nil
}
