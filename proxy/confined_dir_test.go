package main

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

	dir := openConfinedDir(workspace)
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
	if text, ok := dir.readCapped("link.txt", 100); ok {
		t.Errorf("readCapped of a link that leaves the workspace returned %q", text)
	}
	if data, err := readConfined(workspace, "link.txt"); err == nil {
		t.Errorf("readConfined of a link that leaves the workspace returned %q, want an error", data)
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
		dir := openConfinedDir(folder)
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

func TestPlanContextStaysInsideTheWorkspace(t *testing.T) {
	t.Run("a file read by its fixed name", func(t *testing.T) {
		workspace := t.TempDir()
		linkOutOfWorkspace(t, workspace, "README.md", outsideMarker)
		writeInWorkspace(t, workspace, "main.py", "print('inside')\n")
		got := samplePlanContext(workspace, 6, 2000)
		assertNoOutsideContent(t, "samplePlanContext", got)
		if !strings.Contains(got["main.py"], "inside") {
			t.Errorf("the regular file was not read: %v", got)
		}
	})
	t.Run("a file read by its fixed name one folder down", func(t *testing.T) {
		workspace := t.TempDir()
		linkOutOfWorkspace(t, workspace, "app/README.md", outsideMarker)
		writeInWorkspace(t, workspace, "app/main.py", "print('inside')\n")
		got := samplePlanContext(workspace, 6, 2000)
		assertNoOutsideContent(t, "samplePlanContext", got)
		if !strings.Contains(got[filepath.Join("app", "main.py")], "inside") {
			t.Errorf("the regular file was not read: %v", got)
		}
	})
	t.Run("a source file found in the folder listing", func(t *testing.T) {
		workspace := t.TempDir()
		linkOutOfWorkspace(t, workspace, "notes.py", outsideMarker)
		writeInWorkspace(t, workspace, "tool.py", "print('inside')\n")
		got := samplePlanContext(workspace, 6, 2000)
		assertNoOutsideContent(t, "samplePlanContext", got)
		if !strings.Contains(got["tool.py"], "inside") {
			t.Errorf("the regular file was not read: %v", got)
		}
	})
}

func TestProjectScanStaysInsideTheWorkspace(t *testing.T) {
	workspace := t.TempDir()
	linkOutOfWorkspace(t, workspace, "linked.py", outsideMarker)
	linkOutOfWorkspace(t, workspace, "pkg/linked.py", outsideMarker)
	writeInWorkspace(t, workspace, "pkg/real.py", "x = 'inside'\n")
	got := walkPythonFiles(workspace)
	assertNoOutsideContent(t, "walkPythonFiles", got)
	if !strings.Contains(got[filepath.Join("pkg", "real.py")], "inside") {
		t.Errorf("the regular file was not read: %v", got)
	}
}

func TestNodeDetectionStaysInsideTheWorkspace(t *testing.T) {
	const manifest = `{"scripts": {"build": "placeholder-build"}}`
	t.Run("a manifest outside the workspace", func(t *testing.T) {
		workspace := t.TempDir()
		linkOutOfWorkspace(t, workspace, "package.json", manifest)
		info := detectProjectInfo(workspace)
		if info == nil || info.Language != "nodejs" {
			t.Fatalf("detectProjectInfo = %+v, want a nodejs project (the name is in the workspace)", info)
		}
		if info.BuildCommand != "" {
			t.Errorf("build command %q was taken from a file outside the workspace", info.BuildCommand)
		}
	})
	t.Run("a regular manifest", func(t *testing.T) {
		workspace := t.TempDir()
		writeInWorkspace(t, workspace, "package.json", manifest)
		if info := detectProjectInfo(workspace); info == nil || info.BuildCommand == "" {
			t.Errorf("detectProjectInfo = %+v, want the build command of the regular file", info)
		}
	})
}

func TestPythonDetectionStaysInsideTheWorkspace(t *testing.T) {
	t.Run("a requirements file outside the workspace", func(t *testing.T) {
		workspace := t.TempDir()
		linkOutOfWorkspace(t, workspace, "requirements.txt", "flask\n")
		info := detectProjectInfo(workspace)
		if info == nil || info.Language != "python" {
			t.Fatalf("detectProjectInfo = %+v, want a python project (the name is in the workspace)", info)
		}
		if info.Framework != "" {
			t.Errorf("framework %q was taken from a file outside the workspace", info.Framework)
		}
	})
	t.Run("a regular requirements file", func(t *testing.T) {
		workspace := t.TempDir()
		writeInWorkspace(t, workspace, "requirements.txt", "flask\n")
		if info := detectProjectInfo(workspace); info == nil || info.Framework != "flask" {
			t.Errorf("detectProjectInfo = %+v, want the framework of the regular file", info)
		}
	})
}

func TestAssetLintStaysInsideTheWorkspace(t *testing.T) {
	workspace := t.TempDir()
	linkOutOfWorkspace(t, workspace, "templates/linked.html", outsideMarker)
	writeInWorkspace(t, workspace, "templates/real.html", "<p>inside</p>\n")
	files, ok := assetLintFiles(workspace)
	if !ok {
		t.Fatal("assetLintFiles reported a project too large to judge")
	}
	var names []string
	for _, file := range files {
		names = append(names, file.rel)
		if strings.Contains(file.content, outsideMarker) {
			t.Errorf("assetLintFiles returned the content of a file outside the workspace under %q", file.rel)
		}
	}
	if got, want := strings.Join(names, " "), "templates/real.html"; got != want {
		t.Errorf("assetLintFiles read %q, want %q", got, want)
	}
}

func TestExecutionSettingStaysInsideTheWorkspace(t *testing.T) {
	const setting = "[execution]\ntarget = \"host\"\n"
	t.Setenv("ATLAS_VERIFY_IN", "")

	linked := t.TempDir()
	linkOutOfWorkspace(t, linked, filepath.Join(".atlas", "config.toml"), setting)
	if got := resolveVerifyTarget(linked); got != "sandbox" {
		t.Errorf("resolveVerifyTarget = %q with a settings file outside the workspace, want sandbox", got)
	}

	regular := t.TempDir()
	writeInWorkspace(t, regular, filepath.Join(".atlas", "config.toml"), setting)
	if got := resolveVerifyTarget(regular); got != "host" {
		t.Errorf("resolveVerifyTarget = %q with a regular settings file, want host", got)
	}
}
