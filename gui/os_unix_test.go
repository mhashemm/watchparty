//go:build !windows

package main

import "testing"

func TestFileURIPath(t *testing.T) {
	t.Parallel()
	for uri, want := range map[string]string{
		"file:///home/me/movie.mkv":                                "/home/me/movie.mkv",
		"file:///home/me/My%20Movies/%D9%81%D9%8A%D9%84%D9%85.mkv": "/home/me/My Movies/فيلم.mkv",
	} {
		got, err := fileURIPath(uri)
		if err != nil || got != want {
			t.Errorf("fileURIPath(%q) = %q, %v; want %q", uri, got, err, want)
		}
	}
	if _, err := fileURIPath("https://example.com/movie.mkv"); err == nil {
		t.Error("a non-file uri must be an error")
	}
}
