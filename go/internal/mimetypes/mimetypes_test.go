package mimetypes

import "testing"

// TestGuessMimeTypeMirrorsPythonTable pins the mimetypes.guess_type port:
// case-insensitive lookup, suffix_map rewriting, encoding peels, and the
// octet-stream fallback for unknown or extension-less names.
func TestGuessMimeTypeMirrorsPythonTable(t *testing.T) {
	cases := map[string]string{
		"bench-one.txt": "text/plain",
		"PHOTO.JPG":     "image/jpeg",
		"notes.md":      "text/markdown",
		"icon.ico":      "image/vnd.microsoft.icon",
		"clip.wav":      "audio/x-wav",
		"movie.mp4":     "video/mp4",
		"page.html":     "text/html",
		"data.json":     "application/json",
		"doc.docx":      "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"x.tar.gz":      "application/x-tar",
		"archive.tgz":   "application/x-tar",
		"backup.tbz2":   "application/x-tar",
		"blob.svgz":     "image/svg+xml",
		"bundle.txz":    "application/x-tar",
		"x.gz":          "application/octet-stream",
		"x.br":          "application/octet-stream",
		"noext":         "application/octet-stream",
		".hidden":       "application/octet-stream",
		"x.":            "application/octet-stream",
		"weird.xyzzy":   "application/octet-stream",
	}
	for name, want := range cases {
		if got := GuessMimeType(name); got != want {
			t.Errorf("GuessMimeType(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestPythonSplitExt pins the splitext port on the leading-dot rule.
func TestPythonSplitExt(t *testing.T) {
	cases := []struct{ in, base, ext string }{
		{"bench-one.txt", "bench-one", ".txt"},
		{"x.tar.gz", "x.tar", ".gz"},
		{".hidden", ".hidden", ""},
		{"..dots", "..dots", ""},
		{"...a.txt", "...a", ".txt"},
		{".a.b", ".a", ".b"},
		{"x.", "x", "."},
		{"noext", "noext", ""},
		{"/a/b/c.txt", "/a/b/c", ".txt"},
	}
	for _, tc := range cases {
		base, ext := pythonSplitExt(tc.in)
		if base != tc.base || ext != tc.ext {
			t.Errorf("pythonSplitExt(%q) = (%q, %q), want (%q, %q)", tc.in, base, ext, tc.base, tc.ext)
		}
	}
}
