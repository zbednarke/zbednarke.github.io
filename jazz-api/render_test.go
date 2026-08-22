package main

import (
	"strings"
	"testing"
)

func TestBuildFFmpegRenderArgsUsesLosslessAudioAndStandardVideo(t *testing.T) {
	args := buildFFmpegRenderArgs([]renderSource{
		{StartMS: 1250, EndMS: 6250, VideoURL: "https://video-one", AudioURL: "https://audio-one"},
		{StartMS: 0, EndMS: 3000, VideoURL: "https://video-two", AudioURL: "https://audio-two"},
	})
	joined := strings.Join(args, " ")
	for _, expected := range []string{"-ss 1.250 -t 5.000 -i https://video-one", "-ss 1.250 -t 5.000 -i https://audio-one", "concat=n=2:v=1:a=1", "scale=1920:1080", "-c:v libx264", "-crf 18", "-c:a alac", "-ar 48000", "pipe:1"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected ffmpeg args to contain %q; got %s", expected, joined)
		}
	}
}

func TestRenderDownloadFilenameIsSafeAndStable(t *testing.T) {
	if got := renderDownloadFilename("  My Best Take!  "); got != "my-best-take.mp4" {
		t.Fatalf("unexpected filename %q", got)
	}
	if got := renderDownloadFilename("***"); got != "jazz-practice.mp4" {
		t.Fatalf("unexpected fallback filename %q", got)
	}
}
