package main

import (
	"strings"
	"testing"
)

func TestBuildFFmpegRenderArgsUsesCompatibleAndLosslessAudioWithExactDurations(t *testing.T) {
	args := buildFFmpegRenderArgs([]renderSource{
		{StartMS: 1250, EndMS: 6250, VideoURL: "https://video-one", AudioURL: "https://audio-one"},
		{StartMS: 0, EndMS: 3000, VideoURL: "https://video-two", AudioURL: "https://audio-two"},
	})
	joined := strings.Join(args, " ")
	for _, expected := range []string{"-ss 1.250 -i https://video-one", "-ss 1.250 -t 5.000 -i https://audio-one", "tpad=stop_mode=clone:stop_duration=5.000", "trim=duration=5.000", "apad=pad_dur=5.000", "atrim=duration=5.000", "concat=n=2:v=1:a=1", "asplit=2[outaac][outlossless]", "scale=1920:1080", "-c:v libx264", "-preset veryfast", "-crf 18", "-c:a:0 aac", "-b:a:0 320k", "-c:a:1 alac", "title=Lossless ALAC Master", "pipe:1"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected ffmpeg args to contain %q; got %s", expected, joined)
		}
	}
	if strings.Contains(joined, "-ss 1.250 -t 5.000 -i https://video-one") {
		t.Fatal("video input duration limit would consume sparse-keyframe preroll and freeze the rendered clip")
	}
	if !strings.Contains(joined, "format=yuv420p,setpts=PTS-STARTPTS,tpad=stop_mode=clone:stop_duration=5.000,trim=duration=5.000") {
		t.Fatal("video timestamps must be normalized before duration-sensitive padding and trimming")
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
