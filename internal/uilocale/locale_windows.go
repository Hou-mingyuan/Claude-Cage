package uilocale

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var preferredUILanguages = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetUserPreferredUILanguages")

func hostLanguages() []string {
	const languageName = 0x8
	var count, size uint32
	preferredUILanguages.Call(languageName, uintptr(unsafe.Pointer(&count)), 0, uintptr(unsafe.Pointer(&size)))
	if size < 2 {
		return nil
	}
	buffer := make([]uint16, size)
	ok, _, _ := preferredUILanguages.Call(languageName, uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&buffer[0])), uintptr(unsafe.Pointer(&size)))
	if ok == 0 {
		return nil
	}
	var tags []string
	for i, start := 0, 0; i < int(size); i++ {
		if buffer[i] != 0 {
			continue
		}
		if start == i {
			break
		}
		tags = append(tags, windows.UTF16ToString(buffer[start:i]))
		start = i + 1
	}
	return tags
}
