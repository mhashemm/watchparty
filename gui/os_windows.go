package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
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

func pickFolder() (string, error) {
	return dialog("powershell", "-NoProfile", "-STA", "-Command", pickOwner+`$d = New-Object System.Windows.Forms.FolderBrowserDialog
if ($d.ShowDialog($owner) -eq 'OK') { $d.SelectedPath }`)
}

func hide(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}
