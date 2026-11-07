package main

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var user32 = windows.NewLazySystemDLL("user32.dll")

func newWindow(t *testing.T, class, title string) uintptr {
	c, _ := windows.UTF16PtrFromString(class)
	n, _ := windows.UTF16PtrFromString(title)
	hwnd, _, err := user32.NewProc("CreateWindowExW").Call(0, uintptr(unsafe.Pointer(c)), uintptr(unsafe.Pointer(n)), 0, 0, 0, 0, 0, 0, 0, 0, 0)
	if hwnd == 0 {
		t.Fatal(err)
	}
	t.Cleanup(func() { user32.NewProc("DestroyWindow").Call(hwnd) })
	return hwnd
}

func property(hwnd uintptr, pid uint32) string {
	var store *propertyStore
	hr, _, _ := propertyStoreForHwnd.Call(hwnd, uintptr(unsafe.Pointer(&iidPropertyStore)), uintptr(unsafe.Pointer(&store)))
	if hr != 0 {
		return ""
	}
	defer syscall.SyscallN(store.vtbl[2], uintptr(unsafe.Pointer(store)))
	var pv propVariant
	key := propertyKey{appUserModel, pid}
	hr, _, _ = syscall.SyscallN(store.vtbl[5], uintptr(unsafe.Pointer(store)), uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&pv)))
	if hr != 0 || pv.vt != 31 {
		return ""
	}
	return windows.UTF16PtrToString(pv.val)
}

func TestSetAppIDIsWhatTheTaskbarReads(t *testing.T) {
	t.Parallel()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED)
	defer windows.CoUninitialize()
	hwnd := newWindow(t, "STATIC", "")
	dir := t.TempDir()
	exe := filepath.Join(dir, "watchparty-gui.exe")
	os.WriteFile(filepath.Join(dir, "icon.ico"), nil, 0o600)
	if err := setAppID(hwnd, exe); err != nil {
		t.Fatal(err)
	}
	for pid, want := range map[uint32]string{2: `"` + exe + `"`, 3: filepath.Join(dir, "icon.ico") + ",0", 4: "Watchparty", 5: appID} {
		if got := property(hwnd, pid); got != want {
			t.Errorf("property %d is %q, want %q", pid, got, want)
		}
	}
}

func TestGroupWindowKeepsLookingAfterAnotherGUIsWindow(t *testing.T) {
	t.Parallel()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED)
	defer windows.CoUninitialize()
	class, _ := windows.UTF16PtrFromString("Chrome_WidgetWin_1")
	wc := struct {
		size, style                        uint32
		proc                               uintptr
		clsExtra, wndExtra                 int32
		instance, icon, cursor, background uintptr
		menu, class                        *uint16
		iconSm                             uintptr
	}{proc: user32.NewProc("DefWindowProcW").Addr(), class: class}
	wc.size = uint32(unsafe.Sizeof(wc))
	user32.NewProc("RegisterClassExW").Call(uintptr(unsafe.Pointer(&wc)))
	title := "watchparty-test-" + rand.Text()
	wait := func(hwnd uintptr) bool {
		for range 5000 {
			if property(hwnd, 5) == appID {
				return true
			}
			time.Sleep(time.Millisecond)
		}
		return false
	}
	c, cancel := context.WithCancel(t.Context())
	defer cancel()
	other := newWindow(t, "Chrome_WidgetWin_1", title)
	go groupWindow(c, filepath.Join(t.TempDir(), "watchparty-gui.exe"), title, time.Millisecond)
	if !wait(other) {
		t.Fatal("the window that was already open never got the id")
	}
	if !wait(newWindow(t, "Chrome_WidgetWin_1", title)) {
		t.Error("a window opened after another GUI's was matched never got the id")
	}
}

func TestRecordVersionOnlyForTheInstalledCopy(t *testing.T) {
	t.Parallel()
	key := `Software\watchparty-test-` + rand.Text()
	k, _, err := registry.CreateKey(registry.CURRENT_USER, key, registry.ALL_ACCESS)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.DeleteKey(registry.CURRENT_USER, key)
	defer k.Close()
	dir := t.TempDir()
	k.SetStringValue("InstallLocation", dir)
	k.SetStringValue("DisplayVersion", "v0.0.1")
	recordVersionAt(key, filepath.Join(t.TempDir(), "watchparty-gui.exe"), "v0.0.9")
	if v, _, _ := k.GetStringValue("DisplayVersion"); v != "v0.0.1" {
		t.Errorf("a copy elsewhere changed the installed version to %s", v)
	}
	recordVersionAt(key, filepath.Join(dir, "watchparty-gui.exe"), "v0.0.2")
	if v, _, _ := k.GetStringValue("DisplayVersion"); v != "v0.0.2" {
		t.Errorf("DisplayVersion is %s, want v0.0.2", v)
	}
}
