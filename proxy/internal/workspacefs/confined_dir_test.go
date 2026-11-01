package workspacefs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// outsideMarker is the content of the file outside the workspace. A read
// path that returns it has left the workspace.
const outsideMarker = "placeholder content of a file outside the workspace"

// linkOutOfWorkspace puts a file with content outside the workspace and
// makes name, inside the workspace, point at it.
func linkOutOfWorkspace(t *testing.T, workspace, name, content string) {
	t.Helper()
	outside := filepath.Join(t.TempDir(), "outside-file")
	if err := os.WriteFile(outside, []byte(content), 0o600); err != nil {
		t.Fatalf("write the outside file: %v", err)
	}
	link := filepath.Join(workspace, name)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("make the folder of the link: %v", err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("make the link: %v", err)
	}
}

func writeInWorkspace(t *testing.T, workspace, name, content string) {
	t.Helper()
	path := filepath.Join(workspace, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("make the folder of %s: %v", name, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func assertNoOutsideContent(t *testing.T, where string, texts map[string]string) {
	t.Helper()
	for name, text := range texts {
		if strings.Contains(text, outsideMarker) {
			t.Errorf("%s returned the content of a file outside the workspace under %q", where, name)
		}
	}
}

func TestConfinedDirReadsInsideAndRefusesOutside(t *testing.T) {
	workspace := t.TempDir()
	writeInWorkspace(t, workspace, "inside.txt", "inside")
	writeInWorkspace(t, workspace, "sub/deep.txt", "deep")
	linkOutOfWorkspace(t, workspace, "link.txt", outsideMarker)
	if err := os.Symlink("inside.txt", filepath.Join(workspace, "alias.txt")); err != nil {
		t.Fatalf("make the link inside the workspace: %v", err)
	}
	if err := os.Symlink(filepath.Join(workspace, "inside.txt"), filepath.Join(workspace, "absolute.txt")); err != nil {
		t.Fatalf("make the link with an absolute target: %v", err)
	}

	dir := OpenConfinedDir(workspace)
	defer dir.Close()
	if data, err := dir.ReadFile("inside.txt"); err != nil || string(data) != "inside" {
		t.Errorf("ReadFile(inside.txt) = %q, %v; want the content", data, err)
	}
	if data, err := dir.ReadFile(filepath.Join("sub", "deep.txt")); err != nil || string(data) != "deep" {
		t.Errorf("ReadFile(sub/deep.txt) = %q, %v; want the content", data, err)
	}
	if data, err := dir.ReadFile("alias.txt"); err != nil || string(data) != "inside" {
		t.Errorf("ReadFile of a link that stays inside = %q, %v; want the content", data, err)
	}
	if data, err := dir.ReadFile("link.txt"); err == nil {
		t.Errorf("ReadFile of a link that leaves the workspace returned %q, want an error", data)
	}
	// os.Root does not follow a link with an absolute target, wherever it points.
	if data, err := dir.ReadFile("absolute.txt"); err == nil {
		t.Errorf("ReadFile of a link with an absolute target returned %q, want an error", data)
	}
	if _, err := dir.Stat("link.txt"); err == nil {
		t.Error("Stat of a link that leaves the workspace succeeded, want an error")
	}
	if text, ok := dir.ReadCapped("link.txt", 100); ok {
		t.Errorf("ReadCapped of a link that leaves the workspace returned %q", text)
	}
	if data, err := ReadConfined(workspace, "link.txt"); err == nil {
		t.Errorf("ReadConfined of a link that leaves the workspace returned %q, want an error", data)
	}

	var walked []string
	if err := dir.Walk(func(rel string, _ os.DirEntry, _ error) error {
		walked = append(walked, filepath.ToSlash(rel))
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if got, want := strings.Join(walked, " "), "absolute.txt alias.txt inside.txt link.txt sub sub/deep.txt"; got != want {
		t.Errorf("Walk reported %q, want %q", got, want)
	}
}

func TestConfinedDirOfAMissingFolderReadsNothing(t *testing.T) {
	for _, folder := range []string{"", filepath.Join(t.TempDir(), "absent")} {
		dir := OpenConfinedDir(folder)
		if _, err := dir.ReadFile("a.txt"); err == nil {
			t.Errorf("ReadFile in %q succeeded, want an error", folder)
		}
		if _, err := dir.ReadDir(); err == nil {
			t.Errorf("ReadDir of %q succeeded, want an error", folder)
		}
		if err := dir.Walk(func(string, os.DirEntry, error) error { return nil }); err == nil {
			t.Errorf("Walk of %q succeeded, want an error", folder)
		}
		dir.Close()
	}
}
