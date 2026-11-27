package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"gioui.org/app"
	gioevent "gioui.org/io/event"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const exeSuffix = ".exe"

const pickOwner = `[Console]::OutputEncoding = [Text.Encoding]::UTF8
Add-Type -AssemblyName System.Windows.Forms
$owner = New-Object System.Windows.Forms.Form -Property @{TopMost = $true; ShowInTaskbar = $false; FormBorderStyle = 'None'; Opacity = 0; StartPosition = 'CenterScreen'; Width = 1; Height = 1}
$owner.Show()
$owner.Activate()
$owner.TopMost = $false
`

func pickFile() (string, error) {
	patterns := "*" + strings.Join(pickExts, ";*")
	return dialog("powershell", "-NoProfile", "-STA", "-Command", pickOwner+`$d = New-Object System.Windows.Forms.OpenFileDialog -Property @{Filter = 'Videos|`+patterns+`'}
if ($d.ShowDialog($owner) -eq 'OK') { $d.FileName }`)
}

func pickMpv() (string, error) {
	return dialog("powershell", "-NoProfile", "-STA", "-Command", pickOwner+`$d = New-Object System.Windows.Forms.OpenFileDialog
if ($d.ShowDialog($owner) -eq 'OK') { $d.FileName }`)
}

func pickFolder() (string, error) {
	return dialog("powershell", "-NoProfile", "-STA", "-Command", pickOwner+`$d = New-Object System.Windows.Forms.FolderBrowserDialog
if ($d.ShowDialog($owner) -eq 'OK') { $d.SelectedPath }`)
}

// pickSave asks where to save a file, name suggested.
func pickSave(name string) (string, error) {
	return dialog("powershell", "-NoProfile", "-STA", "-Command", pickOwner+`$d = New-Object System.Windows.Forms.SaveFileDialog -Property @{FileName = '`+strings.ReplaceAll(name, "'", "''")+`'}
if ($d.ShowDialog($owner) -eq 'OK') { $d.FileName }`)
}

func hide(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}

const (
	appID        = "mhashemm.watchparty"
	uninstallKey = `Software\Microsoft\Windows\CurrentVersion\Uninstall\watchparty`
)

var (
	propertyStoreForHwnd = windows.NewLazySystemDLL("shell32.dll").NewProc("SHGetPropertyStoreForWindow")
	iidPropertyStore     = windows.GUID{Data1: 0x886D8EEB, Data2: 0x8CF2, Data3: 0x4446, Data4: [8]byte{0x8D, 0x02, 0xCD, 0xBA, 0x1D, 0xBD, 0xCF, 0x99}}
	appUserModel         = windows.GUID{Data1: 0x9F4C2855, Data2: 0x9F79, Data3: 0x4B39, Data4: [8]byte{0xA8, 0xD0, 0xE1, 0xD4, 0x2D, 0xE1, 0xD5, 0xF3}}
)

type propertyKey struct {
	fmtid windows.GUID
	pid   uint32
}

type propVariant struct {
	vt  uint16
	_   [3]uint16
	val *uint16
	_   uintptr
}

type propertyStore struct{ vtbl *[8]uintptr }

func setAppID(hwnd uintptr, exe string) error {
	var store *propertyStore
	hr, _, _ := propertyStoreForHwnd.Call(hwnd, uintptr(unsafe.Pointer(&iidPropertyStore)), uintptr(unsafe.Pointer(&store)))
	if hr != 0 {
		return fmt.Errorf("SHGetPropertyStoreForWindow: %#x", hr)
	}
	defer syscall.SyscallN(store.vtbl[2], uintptr(unsafe.Pointer(store)))
	props := make([]string, 6)
	if exe != "" {
		props[2], props[4] = `"`+exe+`"`, "Watchparty"
		icon := filepath.Join(filepath.Dir(exe), "icon.ico")
		if _, err := os.Stat(icon); err == nil {
			props[3] = icon + ",0"
		}
	}
	props[5] = appID
	for pid, value := range props {
		if value == "" {
			continue
		}
		v, _ := windows.UTF16PtrFromString(value)
		key, pv := propertyKey{appUserModel, uint32(pid)}, propVariant{vt: 31, val: v}
		hr, _, _ = syscall.SyscallN(store.vtbl[6], uintptr(unsafe.Pointer(store)), uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&pv)))
		runtime.KeepAlive(v)
		if hr != 0 {
			return fmt.Errorf("SetValue %d: %#x", pid, hr)
		}
	}
	return nil
}

// viewEvent gives the window, once it has a handle, the id setup.nsi puts on the shortcuts,
// so a pinned shortcut and the window share one taskbar button.
func viewEvent(e gioevent.Event, exe string) {
	v, ok := e.(app.Win32ViewEvent)
	if !ok || v.HWND == 0 {
		return
	}
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED)
		defer windows.CoUninitialize()
		setAppID(v.HWND, exe)
	}()
}

// viewHandle is the window's HWND once Gio has one, 0 before and after.
func viewHandle(e gioevent.Event) uintptr {
	if v, ok := e.(app.Win32ViewEvent); ok {
		return v.HWND
	}
	return 0
}

var (
	dwmSetWindowAttribute = windows.NewLazySystemDLL("dwmapi.dll").NewProc("DwmSetWindowAttribute")
	setWindowPos          = windows.NewLazySystemDLL("user32.dll").NewProc("SetWindowPos")
)

// DWMWA_USE_IMMERSIVE_DARK_MODE: 20 since Windows 10 20H1, 19 on the builds before it
const dwmDarkMode, dwmDarkModeBefore20H1 = 20, 19

// darkFrame turns the native title bar and frame dark or light: Windows leaves an app's in
// light unless the app asks. The frame repaints at once (SWP_FRAMECHANGED), posted to the
// window's thread rather than waited for (SWP_ASYNCWINDOWPOS).
func darkFrame(hwnd uintptr, dark bool) {
	var on int32 // a BOOL
	if dark {
		on = 1
	}
	if r, _, _ := dwmSetWindowAttribute.Call(hwnd, dwmDarkMode, uintptr(unsafe.Pointer(&on)), 4); r != 0 {
		dwmSetWindowAttribute.Call(hwnd, dwmDarkModeBefore20H1, uintptr(unsafe.Pointer(&on)), 4)
	}
	const flags = 0x0002 | 0x0001 | 0x0004 | 0x0010 | 0x0020 | 0x4000 // NOMOVE NOSIZE NOZORDER NOACTIVATE FRAMECHANGED ASYNCWINDOWPOS
	setWindowPos.Call(hwnd, 0, 0, 0, 0, 0, flags)
}

func recordVersion(exe, tag string) {
	recordVersionAt(uninstallKey, exe, tag)
}

func recordVersionAt(key, exe, tag string) {
	k, err := registry.OpenKey(registry.CURRENT_USER, key, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	dir, _, _ := k.GetStringValue("InstallLocation")
	if dir != "" && strings.EqualFold(filepath.Clean(dir), filepath.Dir(exe)) {
		k.SetStringValue("DisplayVersion", tag)
	}
}
