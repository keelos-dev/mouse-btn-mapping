package main

// Minimal pure-Go (no cgo) Windows HID access: enumerate HID interfaces,
// open by path, overlapped read/write of raw reports.

import (
	"errors"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	hidDLL                = windows.NewLazySystemDLL("hid.dll")
	procHidDGetHidGuid    = hidDLL.NewProc("HidD_GetHidGuid")
	procHidDGetAttributes = hidDLL.NewProc("HidD_GetAttributes")
	procHidDGetPreparsed  = hidDLL.NewProc("HidD_GetPreparsedData")
	procHidDFreePreparsed = hidDLL.NewProc("HidD_FreePreparsedData")
	procHidPGetCaps       = hidDLL.NewProc("HidP_GetCaps")
	procHidDGetProductStr = hidDLL.NewProc("HidD_GetProductString")
	errTimeout            = errors.New("HID read timeout")
)

type hidInfo struct {
	Path      string
	VID, PID  uint16
	UsagePage uint16
	Usage     uint16
	InLen     int
	OutLen    int
	Product   string
}

type hidAttributes struct {
	Size          uint32
	VendorID      uint16
	ProductID     uint16
	VersionNumber uint16
}

func hidGUID() windows.GUID {
	var g windows.GUID
	procHidDGetHidGuid.Call(uintptr(unsafe.Pointer(&g)))
	return g
}

func openRaw(path string, access uint32, overlapped bool) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var flags uint32
	if overlapped {
		flags = windows.FILE_FLAG_OVERLAPPED
	}
	return windows.CreateFile(p, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, flags, 0)
}

func hidEnumerate(vid uint16) ([]hidInfo, error) {
	g := hidGUID()
	paths, err := windows.CM_Get_Device_Interface_List("", &g, windows.CM_GET_DEVICE_INTERFACE_LIST_PRESENT)
	if err != nil {
		return nil, err
	}
	var out []hidInfo
	for _, path := range paths {
		if !strings.Contains(strings.ToLower(path), "vid_") {
			continue
		}
		h, err := openRaw(path, 0, false) // no access needed for attributes/caps
		if err != nil {
			continue
		}
		info := hidInfo{Path: path}
		attr := hidAttributes{Size: uint32(unsafe.Sizeof(hidAttributes{}))}
		if r, _, _ := procHidDGetAttributes.Call(uintptr(h), uintptr(unsafe.Pointer(&attr))); r != 0 {
			info.VID, info.PID = attr.VendorID, attr.ProductID
		}
		if info.VID != vid {
			windows.CloseHandle(h)
			continue
		}
		var pp uintptr
		if r, _, _ := procHidDGetPreparsed.Call(uintptr(h), uintptr(unsafe.Pointer(&pp))); r != 0 {
			var caps [64]uint16 // HIDP_CAPS
			procHidPGetCaps.Call(pp, uintptr(unsafe.Pointer(&caps[0])))
			info.Usage, info.UsagePage = caps[0], caps[1]
			info.InLen, info.OutLen = int(caps[2]), int(caps[3])
			procHidDFreePreparsed.Call(pp)
		}
		var name [128]uint16
		if r, _, _ := procHidDGetProductStr.Call(uintptr(h), uintptr(unsafe.Pointer(&name[0])), unsafe.Sizeof(name)); r != 0 {
			info.Product = windows.UTF16ToString(name[:])
		}
		windows.CloseHandle(h)
		out = append(out, info)
	}
	return out, nil
}

type hidDev struct {
	h     windows.Handle
	inLen int
	ev    windows.Handle
}

func hidOpen(info hidInfo) (*hidDev, error) {
	h, err := openRaw(info.Path, windows.GENERIC_READ|windows.GENERIC_WRITE, true)
	if err != nil {
		return nil, err
	}
	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	return &hidDev{h: h, inLen: info.InLen, ev: ev}, nil
}

func (d *hidDev) Close() {
	windows.CloseHandle(d.ev)
	windows.CloseHandle(d.h)
}

func (d *hidDev) io(write bool, buf []byte, timeoutMs uint32) (int, error) {
	var ov windows.Overlapped
	ov.HEvent = d.ev
	windows.ResetEvent(d.ev)
	var n uint32
	var err error
	if write {
		err = windows.WriteFile(d.h, buf, &n, &ov)
	} else {
		err = windows.ReadFile(d.h, buf, &n, &ov)
	}
	if err != nil && err != windows.ERROR_IO_PENDING {
		return 0, err
	}
	ev, _ := windows.WaitForSingleObject(d.ev, timeoutMs)
	if ev == uint32(windows.WAIT_TIMEOUT) {
		windows.CancelIoEx(d.h, &ov)
		windows.GetOverlappedResult(d.h, &ov, &n, true)
		return 0, errTimeout
	}
	if err := windows.GetOverlappedResult(d.h, &ov, &n, true); err != nil {
		return 0, err
	}
	return int(n), nil
}

func (d *hidDev) Write(b []byte) error {
	_, err := d.io(true, b, 1000)
	return err
}

func (d *hidDev) Read(timeoutMs uint32) ([]byte, error) {
	buf := make([]byte, d.inLen)
	n, err := d.io(false, buf, timeoutMs)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}
