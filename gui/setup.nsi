; makensis -DEXE=<watchparty-gui.exe> -DOUT=<setup.exe> -DVERSION=<vX.Y.Z> gui/setup.nsi
; Per user, so the app can still replace its own .exe when it updates itself.
Unicode true
Name "Watchparty"
OutFile "${OUT}"
InstallDir "$LOCALAPPDATA\Programs\watchparty"
RequestExecutionLevel user
Icon "icon.ico"
UninstallIcon "icon.ico"
SetCompressor /SOLID lzma
AutoCloseWindow true
Page instfiles

!include LogicLib.nsh
!include WordFunc.nsh
!ifndef VERSION
	!define VERSION dev
!endif
!define UNINST "Software\Microsoft\Windows\CurrentVersion\Uninstall\watchparty"

Function .onInit
	ReadRegStr $0 HKCU "${UNINST}" "DisplayVersion"
	StrCpy $1 $0 1
	StrCpy $2 "${VERSION}" 1
	${If} $1 == "v"
	${AndIf} $2 == "v"
		StrCpy $1 $0 "" 1
		StrCpy $2 "${VERSION}" "" 1
		${VersionCompare} $1 $2 $3
		${If} $3 = 1
			MessageBox MB_YESNO|MB_ICONQUESTION "Watchparty $0 is installed, newer than this ${VERSION}. Replace it with ${VERSION}?" /SD IDYES IDYES replace
			Abort
			replace:
		${EndIf}
	${EndIf}
FunctionEnd

Section
	SetOutPath "$INSTDIR"
	File "/oname=watchparty-gui.exe" "${EXE}"
	File "icon.ico"
	WriteUninstaller "$INSTDIR\uninstall.exe"
	CreateShortcut "$SMPROGRAMS\Watchparty.lnk" "$INSTDIR\watchparty-gui.exe" "" "$INSTDIR\icon.ico"
	CreateShortcut "$DESKTOP\Watchparty.lnk" "$INSTDIR\watchparty-gui.exe" "" "$INSTDIR\icon.ico"
	WriteRegStr HKCU "${UNINST}" "DisplayName" "Watchparty"
	WriteRegStr HKCU "${UNINST}" "DisplayIcon" "$INSTDIR\icon.ico"
	WriteRegStr HKCU "${UNINST}" "DisplayVersion" "${VERSION}"
	WriteRegStr HKCU "${UNINST}" "InstallLocation" "$INSTDIR"
	WriteRegStr HKCU "${UNINST}" "UninstallString" '"$INSTDIR\uninstall.exe"'
	WriteRegStr HKCU "${UNINST}" "QuietUninstallString" '"$INSTDIR\uninstall.exe" /S'
	WriteRegDWORD HKCU "${UNINST}" "NoModify" 1
	WriteRegDWORD HKCU "${UNINST}" "NoRepair" 1
	IfSilent +2
	Exec '"$INSTDIR\watchparty-gui.exe"'
SectionEnd

Section "Uninstall"
	Delete "$INSTDIR\watchparty-gui.exe"
	Delete "$INSTDIR\watchparty-gui.exe.old"
	Delete "$INSTDIR\.watchparty-*"
	Delete "$INSTDIR\icon.ico"
	Delete "$INSTDIR\uninstall.exe"
	RMDir "$INSTDIR"
	Delete "$SMPROGRAMS\Watchparty.lnk"
	Delete "$DESKTOP\Watchparty.lnk"
	DeleteRegKey HKCU "${UNINST}"
SectionEnd
