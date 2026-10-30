package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

const exeSuffix = ".exe"

var browsers = []string{
	filepath.Join(os.Getenv("ProgramFiles(x86)"), `Microsoft\Edge\Application\msedge.exe`),
	filepath.Join(os.Getenv("ProgramFiles"), `Microsoft\Edge\Application\msedge.exe`),
	filepath.Join(os.Getenv("ProgramFiles"), `Google\Chrome\Application\chrome.exe`),
	filepath.Join(os.Getenv("LocalAppData"), `Google\Chrome\Application\chrome.exe`),
}

func openURL(url string) *exec.Cmd {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
}

const pickScript = `[Console]::OutputEncoding = [Text.Encoding]::UTF8
Add-Type -AssemblyName System.Windows.Forms
$owner = New-Object System.Windows.Forms.Form -Property @{TopMost = $true}
$d = New-Object System.Windows.Forms.OpenFileDialog
if ($d.ShowDialog($owner) -eq 'OK') { $d.FileName }`

func pickFile() (string, error) {
	return dialog("powershell", "-NoProfile", "-STA", "-Command", pickScript)
}

func hide(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}

func replaceExe(src, dst string) error {
	old := dst + ".old"
	os.Remove(old)
	err := os.Rename(dst, old)
	if err != nil {
		return err
	}
	err = os.Rename(src, dst)
	if err != nil {
		os.Rename(old, dst)
	}
	return err
}
