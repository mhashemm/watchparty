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
; the GUI gives its browser window the same id (appID in os_windows.go)
!define APPID "mhashemm.watchparty"

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

Function SetAppID
	Exch $1
	Push $0
	Push $2
	Push $3
	Push $4
	Push $5
	Push $6
	Push $7
	System::Call 'ole32::CoInitialize(p 0)'
	System::Call 'ole32::CoCreateInstance(g "{00021401-0000-0000-C000-000000000046}", p 0, i 1, g "{000214F9-0000-0000-C000-000000000046}", *p .r2) i .r0'
	${If} $0 = 0
		System::Call '$2->0(g "{0000010b-0000-0000-C000-000000000046}", *p .r3) i .r0'
		${If} $0 = 0
			System::Call '$3->5(w r1, i 2) i .r0'
			System::Call '$2->0(g "{886D8EEB-8CF2-4446-8D02-CDBA1DBDCF99}", *p .r4) i .r0'
			${If} $0 = 0
				System::Call '*(&g16 "{9F4C2855-9F79-4B39-A8D0-E1D42DE1D5F3}", i 5) p .r5'
				System::Call '*(&w64 "${APPID}") p .r7'
				System::Call '*(&i2 31, &i2 0, &i2 0, &i2 0, p r7, p 0) p .r6'
				System::Call '$4->6(p r5, p r6) i .r0'
				System::Call '$4->7() i .r0'
				System::Call '$4->2()'
				System::Free $5
				System::Free $6
				System::Free $7
				System::Call '$3->6(p 0, i 1) i .r0'
			${EndIf}
			System::Call '$3->2()'
		${EndIf}
		System::Call '$2->2()'
	${EndIf}
	System::Call 'ole32::CoUninitialize()'
	Pop $7
	Pop $6
	Pop $5
	Pop $4
	Pop $3
	Pop $2
	Pop $0
	Pop $1
FunctionEnd

Section
	SetOutPath "$INSTDIR"
	File "/oname=watchparty-gui.exe" "${EXE}"
	File "icon.ico"
	WriteUninstaller "$INSTDIR\uninstall.exe"
	CreateShortcut "$SMPROGRAMS\Watchparty.lnk" "$INSTDIR\watchparty-gui.exe" "" "$INSTDIR\icon.ico"
	CreateShortcut "$DESKTOP\Watchparty.lnk" "$INSTDIR\watchparty-gui.exe" "" "$INSTDIR\icon.ico"
	Push "$SMPROGRAMS\Watchparty.lnk"
	Call SetAppID
	Push "$DESKTOP\Watchparty.lnk"
	Call SetAppID
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
