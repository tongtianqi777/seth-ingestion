package grouplist

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "groups.txt")
	content := "# comment\n\nAnnouncements\n  Family   Connection  \nfamily connection\nCleanup Duties/Campus Security\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Announcements", "Family   Connection", "Cleanup Duties/Campus Security"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadFile = %q, want %q", got, want)
	}

	if err := os.WriteFile(path, []byte("# nothing yet\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(path); err == nil {
		t.Error("ReadFile accepted a file that lists no groups")
	}
}

func TestFolderName(t *testing.T) {
	for name, want := range map[string]string{
		"Preschool":                      "preschool",
		"Cleanup Duties/Campus Security": "cleanup-duties-campus-security",
		"Preschool P.E. Teachers":        "preschool-pe-teachers",
		"Facilities & Outdoor":           "facilities-outdoor",
		"Anything to do with High Tech":  "anything-to-do-with-high-tech",
		"Parents' Corner":                "parents-corner",
		"  --Spring 2026!--  ":           "spring-2026",
		"🌱 Sprouts":                      "sprouts",
		"Ñandú Club":                     "ñandú-club",
		"种子 班":                           "种子-班",
		"🌱 ✨":                            "120363012345678901",
	} {
		if got := FolderName(name, "120363012345678901"); got != want {
			t.Errorf("FolderName(%q) = %q, want %q", name, got, want)
		}
	}
}
