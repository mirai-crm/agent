package updater

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLaunchHelperCopiesExecutableAndWritesRequest(t *testing.T) {
	fixture := newApplyFixture(t, applyFixtureOptions{withExistingDLL: false})
	selfPath := filepath.Join(t.TempDir(), "mirai-agent-self")
	if err := os.WriteFile(selfPath, []byte("self-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var startedPath string
	var startedArgs []string
	result, err := launchHelperWith(fixture.request, launchDeps{
		selfPath: selfPath,
		startDetached: func(cmd launchedCommand) error {
			startedPath = cmd.Path
			startedArgs = append([]string(nil), cmd.Args...)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("launchHelperWith() error = %v", err)
	}

	if got := mustReadString(t, result.HelperPath); got != "self-binary" {
		t.Fatalf("helper contents = %q, want copied self binary", got)
	}
	loaded, err := LoadApplyRequest(result.RequestPath)
	if err != nil {
		t.Fatalf("LoadApplyRequest() error = %v", err)
	}
	if loaded.TargetPath != fixture.request.TargetPath {
		t.Fatalf("loaded target path = %q, want %q", loaded.TargetPath, fixture.request.TargetPath)
	}
	if startedPath != result.HelperPath {
		t.Fatalf("started path = %q, want %q", startedPath, result.HelperPath)
	}
	if len(startedArgs) != 2 || startedArgs[0] != "apply-update" || startedArgs[1] != result.RequestPath {
		t.Fatalf("started args = %v, want [apply-update request]", startedArgs)
	}
}
