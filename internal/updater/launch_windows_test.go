//go:build windows

package updater

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestBuildHelperCommandDetachesOnWindows(t *testing.T) {
	cmd := buildHelperCommand(`C:\helper.exe`, `C:\request.json`)
	if cmd.Path != `C:\helper.exe` {
		t.Fatalf(`Path = %q, want C:\helper.exe`, cmd.Path)
	}
	if len(cmd.Args) != 2 || cmd.Args[0] != "apply-update" || cmd.Args[1] != `C:\request.json` {
		t.Fatalf("Args = %v, want apply-update request", cmd.Args)
	}
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr = nil, want detached process flags")
	}
	want := uint32(windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP)
	if cmd.SysProcAttr.CreationFlags != want {
		t.Fatalf("CreationFlags = %#x, want %#x", cmd.SysProcAttr.CreationFlags, want)
	}
}

func TestPrepareHelperSidecarDLLLeavesAdjacentStagedDLLIntact(t *testing.T) {
	dir := t.TempDir()
	dllPath := filepath.Join(dir, "libusb-1.0.dll")
	want := []byte("staged-windows-dll")
	if err := os.WriteFile(dllPath, want, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if err := prepareHelperSidecarDLL(ApplyRequest{StagedLibUSBPath: dllPath}, dir); err != nil {
		t.Fatalf("prepareHelperSidecarDLL() error = %v", err)
	}

	got, err := os.ReadFile(dllPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("DLL contents = %q, want %q", got, want)
	}
}
