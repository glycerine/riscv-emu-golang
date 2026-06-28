//go:build windows

package keysmith

import (
	"encoding/binary"

	"golang.org/x/sys/windows"
)

func windowsReparseLinkTarget(path string) (string, bool) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", false
	}
	h, err := windows.CreateFile(
		p,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return "", false
	}
	defer windows.CloseHandle(h)

	buf := make([]byte, windows.MAXIMUM_REPARSE_DATA_BUFFER_SIZE)
	var bytesReturned uint32
	err = windows.DeviceIoControl(
		h,
		windows.FSCTL_GET_REPARSE_POINT,
		nil,
		0,
		&buf[0],
		uint32(len(buf)),
		&bytesReturned,
		nil,
	)
	if err != nil || bytesReturned < 8 {
		return "", false
	}

	dataLen := int(binary.LittleEndian.Uint16(buf[4:6]))
	maxDataLen := int(bytesReturned) - 8
	if dataLen > maxDataLen {
		dataLen = maxDataLen
	}
	if dataLen <= 0 {
		return "", false
	}
	return reparseDataLinkTarget(buf[8 : 8+dataLen])
}
