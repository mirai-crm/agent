//go:build !windows

package updater

import "testing"

func TestBuildHelperCommandDetachesOnUnix(t *testing.T) {
	cmd := buildHelperCommand("/tmp/helper", "/tmp/request.json")
	if cmd.Path != "/tmp/helper" {
		t.Fatalf("Path = %q, want /tmp/helper", cmd.Path)
	}
	if len(cmd.Args) != 2 || cmd.Args[0] != "apply-update" || cmd.Args[1] != "/tmp/request.json" {
		t.Fatalf("Args = %v, want apply-update request", cmd.Args)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatal("SysProcAttr.Setsid = false, want true")
	}
}
