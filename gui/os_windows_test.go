package main

import (
	"crypto/rand"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows/registry"
)

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
