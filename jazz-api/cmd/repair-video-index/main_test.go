package main

import (
	"strings"
	"testing"
)

func TestBackupObjectName(t *testing.T) {
	got := backupObjectName("users/user/date/take/video.webm")
	want := "users/user/date/take/video.original-unindexed.webm"
	if got != want {
		t.Fatalf("backupObjectName() = %q, want %q", got, want)
	}
}

func TestMP4RemuxBuildsFastStartIndexWithoutReencoding(t *testing.T) {
	args := strings.Join(remuxArgs("video/mp4", "input.mp4", "output.mp4"), " ")
	for _, expected := range []string{"-c copy", "-movflags +faststart", "output.mp4"} {
		if !strings.Contains(args, expected) {
			t.Fatalf("expected MP4 remux args to contain %q; got %s", expected, args)
		}
	}
	if strings.Contains(args, "cues_to_front") {
		t.Fatalf("MP4 remux unexpectedly used WebM cue flags: %s", args)
	}
}

func TestWebMRemuxKeepsCueRepair(t *testing.T) {
	args := strings.Join(remuxArgs("video/webm", "input.webm", "output.webm"), " ")
	for _, expected := range []string{"-c copy", "-reserve_index_space 1048576", "-cues_to_front 1"} {
		if !strings.Contains(args, expected) {
			t.Fatalf("expected WebM remux args to contain %q; got %s", expected, args)
		}
	}
}

func TestRepairedSizeIsSafe(t *testing.T) {
	const source = int64(700 << 20)
	if !repairedSizeIsSafe(source, source+(5<<20)) {
		t.Fatal("expected a sub-one-percent container metadata increase to be accepted")
	}
	if repairedSizeIsSafe(source, source-(100<<20)) {
		t.Fatal("expected a large loss of packet data to be rejected")
	}
}

func TestSmallRepairAllowsCueMetadata(t *testing.T) {
	const source = int64(35 << 20)
	if !repairedSizeIsSafe(source, source+(1<<20)) {
		t.Fatal("expected cue metadata overhead on a small recording to be accepted")
	}
}
