//go:build windows

package updater

import (
	"os"
	"path/filepath"
	"testing"
)

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
