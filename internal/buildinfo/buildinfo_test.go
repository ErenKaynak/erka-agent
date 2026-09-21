package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestFromSettingsUsesVCSStampAndMarksDirtyTrees(t *testing.T) {
	t.Parallel()
	got := fromSettings(Info{Version: "dev", Commit: "unknown", Date: "unknown"}, []debug.BuildSetting{
		{Key: "vcs.revision", Value: "24f8645aabbccddeeff00112233"},
		{Key: "vcs.time", Value: "2026-09-21T10:00:00Z"},
		{Key: "vcs.modified", Value: "true"},
	})
	if got.Commit != "24f8645aabbc-dirty" || got.Date != "2026-09-21T10:00:00Z" {
		t.Fatalf("fromSettings() = %#v", got)
	}
	release := fromSettings(Info{Version: "1.0.0", Commit: "abc", Date: "today"}, []debug.BuildSetting{{Key: "vcs.revision", Value: "other"}})
	if release.Commit != "abc" || release.Date != "today" {
		t.Fatalf("release stamp overwritten: %#v", release)
	}
}
