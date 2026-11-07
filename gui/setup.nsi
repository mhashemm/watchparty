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
!include Win\COM.nsh
!include Win\Propkey.nsh
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
	System::Call 'OLE32::CoInitialize(p 0)'
	!insertmacro ComHlpr_CreateInProcInstance ${CLSID_ShellLink} ${IID_IShellLink} r0 ""
	${If} $0 P<> 0
		${IUnknown::QueryInterface} $0 '("${IID_IPersistFile}",.r2)'
		${If} $2 P<> 0
			${IPersistFile::Load} $2 '("$1",${STGM_READWRITE}).r3'
			${If} $3 = 0
				${IUnknown::QueryInterface} $0 '("${IID_IPropertyStore}",.r4)'
				${If} $4 P<> 0
					System::Call '*${SYSSTRUCT_PROPERTYKEY}(${PKEY_AppUserModel_ID})p.r6'
					System::Call '*(&w64 "${APPID}")p.r5'
					System::Call '*${SYSSTRUCT_PROPVARIANT}(${VT_LPWSTR},,$5)p.r7'
					${IPropertyStore::SetValue} $4 '($6,$7)'
					${IPropertyStore::Commit} $4 ""
					${IUnknown::Release} $4 ""
					System::Free $5
					System::Free $6
					System::Free $7
					${IPersistFile::Save} $2 '("$1",1)'
				${EndIf}
			${EndIf}
			${IUnknown::Release} $2 ""
		${EndIf}
		${IUnknown::Release} $0 ""
	${EndIf}
	System::Call 'OLE32::CoUninitialize()'
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
