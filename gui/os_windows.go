package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const exeSuffix = ".exe"

func browsers() []browser {
	var found []browser
	for _, path := range []string{
		filepath.Join(os.Getenv("ProgramFiles"), `Google\Chrome\Application\chrome.exe`),
		filepath.Join(os.Getenv("LocalAppData"), `Google\Chrome\Application\chrome.exe`),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `Microsoft\Edge\Application\msedge.exe`),
	} {
		found = append(found, browser{path, []string{path}, os.TempDir()})
	}
	return found
}

func openURL(url string) *exec.Cmd {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
}

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

func hide(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}

const (
	appID        = "mhashemm.watchparty"
	uninstallKey = `Software\Microsoft\Windows\CurrentVersion\Uninstall\watchparty`
)

var (
	findWindowEx         = windows.NewLazySystemDLL("user32.dll").NewProc("FindWindowExW")
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

func groupWindow(c context.Context, exe, title string, every time.Duration) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED)
	defer windows.CoUninitialize()
	class, _ := windows.UTF16PtrFromString("Chrome_WidgetWin_1")
	name, _ := windows.UTF16PtrFromString(title)
	tick := time.NewTicker(every)
	defer tick.Stop()
	timeout := time.After(30 * time.Second)
	done := map[uintptr]bool{}
	for {
		select {
		case <-c.Done():
			return
		case <-timeout:
			return
		case <-tick.C:
		}
		for hwnd := uintptr(0); ; {
			hwnd, _, _ = findWindowEx.Call(0, hwnd, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(name)))
			if hwnd == 0 {
				break
			}
			if !done[hwnd] && setAppID(hwnd, exe) == nil {
				done[hwnd] = true
			}
		}
	}
}

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
