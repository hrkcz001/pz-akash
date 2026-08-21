//go:build windows

package httpapi

import (
	"syscall"
	"unsafe"
)

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	getDiskFreeSpaceEx = kernel32.NewProc("GetDiskFreeSpaceExW")
)

// diskFree is the space available to the calling user on the volume holding dir.
//
// The controller only ever runs on Linux, so this exists for one reason: the
// development and test machine is Windows, and a package that will not build
// there cannot be worked on there. GetDiskFreeSpaceExW's first out-parameter is
// the caller's quota-adjusted free space, which is the Windows analogue of Bavail.
func diskFree(dir string) (int64, bool) {
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0, false
	}
	var freeToCaller, total, totalFree uint64
	r, _, _ := getDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&freeToCaller)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if r == 0 {
		return 0, false
	}
	return int64(freeToCaller), true
}
