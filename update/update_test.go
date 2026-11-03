package update

import "testing"

func TestNewer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"v0.0.2", "v0.0.1", true},
		{"v0.0.1", "v0.0.1", false},
		{"v0.0.1", "v0.0.2", false},
		{"v1.0.0", "v0.9.9", true},
		{"v0.10.0", "v0.9.0", true},
		{"v0.1.0", "v0.0.99", true},
		{"v1.0", "v1.0.0", false},
		{"v1.0.1", "v1.0", true},
		{"0.0.2", "0.0.1", true},
		{"v0.0.2", "dev", true},
	}
	for _, tc := range cases {
		if got := Newer(tc.latest, tc.current); got != tc.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", tc.latest, tc.current, got, tc.want)
		}
	}
}

func TestAssetName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		base, goos, goarch, want string
	}{
		{"watchparty", "linux", "amd64", "watchparty_amd64"},
		{"watchparty", "linux", "arm64", "watchparty_arm64"},
		{"watchparty", "windows", "amd64", "watchparty.exe"},
		{"watchparty", "darwin", "arm64", "watchparty_mac_arm64"},
		{"watchparty-gui", "linux", "amd64", "watchparty-gui_amd64"},
		{"watchparty-gui", "windows", "amd64", "watchparty-gui.exe"},
		{"watchparty-gui", "darwin", "amd64", "watchparty-gui_mac_amd64"},
		{"watchparty", "windows", "arm64", ""},
		{"watchparty", "plan9", "amd64", ""},
	}
	for _, tc := range cases {
		if got := assetName(tc.base, tc.goos, tc.goarch); got != tc.want {
			t.Errorf("assetName(%q, %q, %q) = %q, want %q", tc.base, tc.goos, tc.goarch, got, tc.want)
		}
	}
}
