package paths

import "testing"

func TestExpandHome(t *testing.T) {
	t.Setenv("HOME", "/Users/someone")
	for in, want := range map[string]string{
		"~":                           "/Users/someone",
		"~/seth-ingestion-data/media": "/Users/someone/seth-ingestion-data/media",
		"data":                        "data",
		"/var/media":                  "/var/media",
		"~other/media":                "~other/media",
		"a/~/b":                       "a/~/b",
		"":                            "",
	} {
		if got := ExpandHome(in); got != want {
			t.Errorf("ExpandHome(%q) = %q, want %q", in, got, want)
		}
	}
}
