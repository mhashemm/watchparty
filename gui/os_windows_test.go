package main

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
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
