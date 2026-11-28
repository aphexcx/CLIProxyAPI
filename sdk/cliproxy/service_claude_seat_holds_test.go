package cliproxy

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeSeatHoldFilePathBesideConfig(t *testing.T) {
	if got := claudeSeatHoldFilePath(" "); got != "" {
		t.Fatalf("without a config path the holds stay in memory, got file %q", got)
	}
	dir := t.TempDir()
	first := claudeSeatHoldFilePath(filepath.Join(dir, "config.yaml"))
	second := claudeSeatHoldFilePath(filepath.Join(dir, "config-second.yaml"))
	if filepath.Dir(first) != dir || first == second {
		t.Fatalf("want one file per config beside it, got %q and %q", first, second)
	}
	if strings.HasSuffix(strings.ToLower(first), ".json") {
		t.Fatalf("hold file %q would read as a credential in an auth directory", first)
	}
}
