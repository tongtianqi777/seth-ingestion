// Package grouplist holds the rules for the groups file and group folder
// names, shared by seth-ingestion and the seth-sim simulator.
package grouplist

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"unicode"
)

// ReadFile returns the group names listed in path: one per line, with
// blank lines and lines starting with # ignored.
func ReadFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var names []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || seen[Key(line)] {
			continue
		}
		seen[Key(line)] = true
		names = append(names, line)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("%s lists no groups", path)
	}
	return names, nil
}

// Key normalizes a group name for matching: case and runs of whitespace don't matter.
func Key(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}

// FolderName turns a group name into a lowercase folder name of letters,
// digits and dashes, such as "cleanup-duties-campus-security". Dots and
// apostrophes are dropped ("P.E." becomes "pe"), and every other run of
// characters that aren't letters or digits becomes one dash. A name with no
// letters or digits gets fallback.
func FolderName(name, fallback string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		case r == '.' || r == '\'' || r == '’':
		default:
			dash = true
		}
	}
	if b.Len() == 0 {
		return fallback
	}
	return b.String()
}
