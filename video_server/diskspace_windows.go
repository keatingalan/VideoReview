//go:build windows

package main

import "golang.org/x/sys/windows"

func diskFreeBytes(path string) (int64, error) {
	directory, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var available, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(directory, &available, &total, &totalFree); err != nil {
		return 0, err
	}
	return int64(available), nil
}
