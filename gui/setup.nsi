; makensis -DEXE=<watchparty-gui.exe> -DOUT=<setup.exe> -DVERSION=<vX.Y.Z> gui/setup.nsi
; Per user, so the app can still replace its own .exe when it updates itself.
Unicode true
Name "Watchparty"
OutFile "${OUT}"
!define UNINST "Software\Microsoft\Windows\CurrentVersion\Uninstall\watchparty"
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
!include WinMessages.nsh
!ifndef VERSION
	!define VERSION dev
!endif
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

; $0 is curl's exit code; curl.exe and tar.exe ship with Windows 10 1803 and later
!macro Fetch URL FILE
	nsExec::ExecToLog '"$SYSDIR\curl.exe" -fsSL --retry 3 -o "${FILE}" "${URL}"'
	Pop $0
!macroend

; $1 is the tag /releases/latest redirects to, so the newest release at install time
!macro Latest REPO
	nsExec::ExecToStack '"$SYSDIR\curl.exe" -fsS --retry 3 -o NUL -w "%{redirect_url}" "https://github.com/${REPO}/releases/latest"'
	Pop $0
	Pop $1
	${WordFind} "$1" "/" "-1" $1
	${If} $0 != 0
		StrCpy $1 ""
	${EndIf}
!macroend

; Downloads NAME into $INSTDIR\NAME unless PATH has it or it is already there. $1 in URL is
; REPO's latest tag; TAR is tar's options for a zip, or - for a bare exe. If it fails, mpv,
; which Watchparty needs and which comes first, asks Retry or Cancel the install; the others
; ask Retry or go on without it.
!macro Get NAME EXE REPO URL TAR LABEL SIZE NEEDED
	; on PATH only: SearchPath would also look in the installer's folder and the current one
	System::Call 'kernel32::SearchPathW(p R9, w "${NAME}.exe", p 0, i ${NSIS_MAX_STRLEN}, w .r0, p 0) i .r2'
	${If} $2 = 0
	${AndIfNot} ${FileExists} "$INSTDIR\${NAME}\${EXE}"
		${LABEL}:
		DetailPrint "Downloading ${NAME}${SIZE}"
		CreateDirectory "$INSTDIR\${NAME}"
		!if "${TAR}" == "-"
			!insertmacro Fetch "${URL}" "$INSTDIR\${NAME}\${EXE}"
		!else
			!insertmacro Latest "${REPO}"
			!insertmacro Fetch "${URL}" "$PLUGINSDIR\${NAME}.zip"
			${If} $0 == 0
				nsExec::ExecToLog '"$SYSDIR\tar.exe" -xf "$PLUGINSDIR\${NAME}.zip" ${TAR} -C "$INSTDIR\${NAME}"'
				Pop $0
			${EndIf}
		!endif
		; curl leaves a truncated file and tar a half-unpacked folder when they fail partway
		${If} $0 != 0
		${OrIfNot} ${FileExists} "$INSTDIR\${NAME}\${EXE}"
			RMDir /r "$INSTDIR\${NAME}"
			!if "${NEEDED}" == "needed"
				MessageBox MB_RETRYCANCEL|MB_ICONEXCLAMATION "Couldn't download ${NAME}, which Watchparty needs. Check your connection and retry, or cancel the install." /SD IDCANCEL IDRETRY ${LABEL}
				RMDir "$INSTDIR"
				Abort "Couldn't download ${NAME}"
			!else
				MessageBox MB_YESNO|MB_ICONEXCLAMATION "Couldn't download ${NAME}. Try again? No goes on without it, and Watchparty's Settings page says how to get it later." /SD IDNO IDYES ${LABEL}
			!endif
		${EndIf}
	${EndIf}
!macroend

; adds BIN to $1 when NAME is in the install folder
!macro OnPath NAME EXE BIN
	${If} ${FileExists} "$INSTDIR\${NAME}\${EXE}"
		StrCpy $1 "$1$INSTDIR\${BIN};"
	${EndIf}
!macroend

Function Players
	InitPluginsDir
	; PATH can be longer than NSIS's strings, so it stays in this buffer
	System::Alloc 65536
	Pop $R9
	System::Call 'kernel32::GetEnvironmentVariableW(w "PATH", p R9, i 32767)'
	; mingw, not msvc: the msvc build needs the Visual C++ runtime, which a fresh Windows lacks
	!insertmacro Get mpv "mpv.exe" "mpv-player/mpv" "https://github.com/mpv-player/mpv/releases/download/$1/mpv-$1-x86_64-w64-mingw32-full.zip" "" getmpv ", about 65 MB" needed
	!insertmacro Get ffmpeg "bin\ffmpeg.exe" "GyanD/codexffmpeg" "https://github.com/GyanD/codexffmpeg/releases/download/$1/ffmpeg-$1-essentials_build.zip" "--strip-components 1" getffmpeg ", about 115 MB" optional
	!insertmacro Get yt-dlp "yt-dlp.exe" "" "https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp.exe" "-" getytdlp "" optional
	StrCpy $1 ""
	!insertmacro OnPath mpv "mpv.exe" "mpv"
	!insertmacro OnPath ffmpeg "bin\ffmpeg.exe" "ffmpeg\bin"
	!insertmacro OnPath yt-dlp "yt-dlp.exe" "yt-dlp"
	${If} $1 != ""
		File "/oname=$PLUGINSDIR\path.ps1" "path.ps1"
		nsExec::ExecToLog '"$SYSDIR\WindowsPowerShell\v1.0\powershell.exe" -NoProfile -ExecutionPolicy Bypass -File "$PLUGINSDIR\path.ps1" add "$1"'
		Pop $0
		${If} $0 != 0
			MessageBox MB_ICONEXCLAMATION "Couldn't add these folders to your Path: $1$\r$\nAdd them by hand: Start, type environment variables, open Edit environment variables for your account, then Path." /SD IDOK
		${EndIf}
		SendMessage ${HWND_BROADCAST} ${WM_SETTINGCHANGE} 0 "STR:Environment" /TIMEOUT=5000
		; The GUI started at the end inherits this process's PATH, which predates the change; every
		; later launch gets the new one from Windows. A result over 32767 characters, the most PATH
		; can hold, would leave the buffer empty.
		System::Call 'kernel32::ExpandEnvironmentStringsW(w "$1%PATH%", p R9, i 32767) i .r2'
		${If} $2 > 0
		${AndIf} $2 <= 32767
			System::Call 'kernel32::SetEnvironmentVariableW(w "PATH", p R9)'
		${EndIf}
	${EndIf}
	System::Free $R9
FunctionEnd

Section
	Call Players
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
	; mpv, ffmpeg and yt-dlp go only from the install folder; copies elsewhere are left alone
	RMDir /r "$INSTDIR\mpv"
	RMDir /r "$INSTDIR\ffmpeg"
	RMDir /r "$INSTDIR\yt-dlp"
	InitPluginsDir
	File "/oname=$PLUGINSDIR\path.ps1" "path.ps1"
	nsExec::ExecToLog '"$SYSDIR\WindowsPowerShell\v1.0\powershell.exe" -NoProfile -ExecutionPolicy Bypass -File "$PLUGINSDIR\path.ps1" remove "$INSTDIR\mpv;$INSTDIR\ffmpeg\bin;$INSTDIR\yt-dlp"'
	Pop $0
	SendMessage ${HWND_BROADCAST} ${WM_SETTINGCHANGE} 0 "STR:Environment" /TIMEOUT=5000
	RMDir "$INSTDIR"
	Delete "$SMPROGRAMS\Watchparty.lnk"
	Delete "$DESKTOP\Watchparty.lnk"
	DeleteRegKey HKCU "${UNINST}"
SectionEnd
