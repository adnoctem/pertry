package doctor

import "testing"

// TestUnescapeMount matters because /proc/mounts separates fields by whitespace and therefore
// escapes any whitespace inside a mount point. The archive this was written against lives at
// "/win/e/Bilder/2026.03.29 - Google Fotos Sicherung", so a mount point with a space in it is the
// normal case here, not an exotic one.
func TestUnescapeMount(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"/win/e":           "/win/e",
		`/mnt/My\040Drive`: "/mnt/My Drive",
		`/a\040b\040c`:     "/a b c",
		`/tab\011here`:     "/tab\there",
		`/back\134slash`:   `/back\slash`,
		// A trailing backslash with no complete escape is left alone rather than dropped.
		`/trailing\`: `/trailing\`,
		// Not an octal escape, so it stays as written.
		`/not\09zescape`: `/not\09zescape`,
	}

	for in, want := range tests {
		if got := unescapeMount(in); got != want {
			t.Errorf("unescapeMount(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestUnder compares whole path elements, so that a directory whose name merely starts with a
// mount point's name is not attributed to it.
func TestUnder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path, mount string
		want        bool
	}{
		{"/win/e", "/win/e", true},
		{"/win/e/Bilder", "/win/e", true},
		{"/win/e/Bilder/x.jpg", "/win/e", true},
		// The case a naive prefix test gets wrong.
		{"/winter", "/win", false},
		{"/win/exterior", "/win/e", false},
		{"/home/fmj", "/win/e", false},
		// Everything is under the root mount.
		{"/anything", "/", true},
		{"/", "/", true},
		// A mount point recorded with a trailing slash still compares correctly.
		{"/win/e/Bilder", "/win/e/", true},
	}

	for _, tt := range tests {
		if got := under(tt.path, tt.mount); got != tt.want {
			t.Errorf("under(%q, %q) = %v, want %v", tt.path, tt.mount, got, tt.want)
		}
	}
}
