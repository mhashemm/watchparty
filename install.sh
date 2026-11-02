#!/bin/sh
# curl -fsSL https://raw.githubusercontent.com/mhashemm/watchparty/master/install.sh | sh
set -eu

case $(uname -m) in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) echo "no watchparty build for $(uname -m)" >&2 && exit 1 ;;
esac

case $(uname -s) in
Linux)
	asset=watchparty-gui_$arch
	bin=${XDG_BIN_HOME:-$HOME/.local/bin}
	data=${XDG_DATA_HOME:-$HOME/.local/share}
	;;
Darwin)
	asset=watchparty-gui_mac_$arch
	app=$HOME/Applications/Watchparty.app
	bin=$app/Contents/MacOS
	;;
*) echo "use watchparty-gui-setup.exe from https://github.com/mhashemm/watchparty/releases on Windows" >&2 && exit 1 ;;
esac

mkdir -p "$bin"
tmp=$bin/.watchparty-gui-install
curl -fL --progress-bar -o "$tmp" "https://github.com/mhashemm/watchparty/releases/latest/download/$asset"
chmod 755 "$tmp"
mv -f "$tmp" "$bin/watchparty-gui"

if [ "$(uname -s)" = Linux ]; then
	mkdir -p "$data/applications" "$data/icons/hicolor/scalable/apps"
	cat >"$data/icons/hicolor/scalable/apps/watchparty.svg" <<'EOF'
<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'><path d='M12 18h8l1 11h-10z' fill='#f1ead6' stroke='#6e4b30' stroke-width='1.5'/><path d='M3 18C5 5 27 5 29 18z' fill='#a8642a' stroke='#4a3220' stroke-width='1.5'/><circle cx='11' cy='12' r='2' fill='#f6efdc'/><circle cx='19' cy='9.5' r='1.6' fill='#f6efdc'/></svg>
EOF
	cat >"$data/applications/watchparty.desktop" <<EOF
[Desktop Entry]
Type=Application
Name=Watchparty
Comment=Watch the same video with friends in mpv
Exec="$bin/watchparty-gui"
Icon=watchparty
Terminal=false
Categories=AudioVideo;Video;
EOF
	files="\"$bin/watchparty-gui\" \"$data/applications/watchparty.desktop\" \"$data/icons/hicolor/scalable/apps/watchparty.svg\""
else
	cat >"$app/Contents/Info.plist" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleExecutable</key><string>watchparty-gui</string>
	<key>CFBundleIdentifier</key><string>io.github.mhashemm.watchparty</string>
	<key>CFBundleName</key><string>Watchparty</string>
	<key>CFBundlePackageType</key><string>APPL</string>
	<key>LSUIElement</key><true/>
	<key>LSEnvironment</key>
	<dict>
		<key>PATH</key><string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
	</dict>
</dict>
</plist>
EOF
	files="\"$app\""
fi

echo "installed $bin/watchparty-gui, it updates itself from Settings"
command -v mpv >/dev/null || [ -x /opt/homebrew/bin/mpv ] || echo "watchparty needs mpv: https://mpv.io/installation/"
echo "to uninstall: rm -r $files"
