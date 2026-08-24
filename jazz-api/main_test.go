package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidateCampaignState(t *testing.T) {
	valid := json.RawMessage(`{"version":1,"skillLevels":{"sound":2},"objectives":{"0":true},"repertoire":{"blue-bossa":3},"bosses":{},"scene":{},"practice":[{"id":"p1","date":"2026-08-02","minutes":30,"track":"trumpet","note":"long tones"}],"peopleCanCall":2}`)
	if err := validateCampaignState(valid); err != nil {
		t.Fatalf("valid state rejected: %v", err)
	}

	invalid := json.RawMessage(`{"version":1,"skillLevels":{"sound":9},"objectives":{},"repertoire":{},"bosses":{},"scene":{},"practice":[],"peopleCanCall":0}`)
	if err := validateCampaignState(invalid); err == nil {
		t.Fatal("invalid skill level was accepted")
	}
}

func TestValidateCampaignStateAcceptsFractionalPracticeMinutes(t *testing.T) {
	shortPractice := json.RawMessage(`{"version":1,"skillLevels":{},"objectives":{},"repertoire":{},"bosses":{},"scene":{},"practice":[{"id":"short-take","date":"2026-08-23","minutes":0.125,"track":"trumpet","note":""}],"peopleCanCall":0}`)
	if err := validateCampaignState(shortPractice); err != nil {
		t.Fatalf("fractional practice minutes rejected: %v", err)
	}
}

func TestAuthenticationRejectsMissingGatewayKey(t *testing.T) {
	app := &application{cfg: config{GatewayKey: "secret"}}
	handler := app.authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	request := httptest.NewRequest(http.MethodGet, "/v1/state", nil)
	request.Header.Set("X-Jazz-User", "zach")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("got status %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestAuthenticationAcceptsGatewayAndUser(t *testing.T) {
	app := &application{cfg: config{GatewayKey: "secret"}}
	handler := app.authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	request := httptest.NewRequest(http.MethodGet, "/v1/state", nil)
	request.Header.Set("X-Jazz-Gateway-Key", "secret")
	request.Header.Set("X-Jazz-User", "zach")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("got status %d, want %d", response.Code, http.StatusNoContent)
	}
}

func TestAllowedUploadOrigin(t *testing.T) {
	tests := []struct {
		name   string
		origin string
		want   string
	}{
		{name: "production", origin: "https://zachbednarke.com", want: "https://zachbednarke.com"},
		{name: "local host", origin: "http://localhost:4173", want: "http://localhost:4173"},
		{name: "local IP", origin: "http://127.0.0.1:4173", want: "http://127.0.0.1:4173"},
		{name: "trims whitespace", origin: "  http://localhost:4173  ", want: "http://localhost:4173"},
		{name: "rejects arbitrary origin", origin: "https://example.com", want: ""},
		{name: "rejects lookalike origin", origin: "https://zachbednarke.com.example.com", want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := allowedUploadOrigin(test.origin); got != test.want {
				t.Fatalf("allowedUploadOrigin(%q) = %q, want %q", test.origin, got, test.want)
			}
		})
	}
}

func TestValidateRecordingMedia(t *testing.T) {
	audio := recordingInitRequest{ContentType: "audio/wav", SizeBytes: 1024, DurationMS: maxDurationMS}
	kind, audioType, videoType, err := validateRecordingMedia(audio)
	if err != nil || kind != "audio" || audioType != "audio/wav" || videoType != "" {
		t.Fatalf("valid audio rejected: kind=%q audio=%q video=%q err=%v", kind, audioType, videoType, err)
	}

	video := recordingInitRequest{
		MediaKind: "video", ContentType: "audio/wav", SizeBytes: 2048, DurationMS: 60000,
		VideoContentType: "video/webm;codecs=vp9,opus", VideoSizeBytes: 4096,
		VideoWidth: 1920, VideoHeight: 1080, VideoFrameRate: 30,
	}
	kind, audioType, videoType, err = validateRecordingMedia(video)
	if err != nil || kind != "video" || audioType != "audio/wav" || videoType != "video/webm" {
		t.Fatalf("valid video rejected: kind=%q audio=%q video=%q err=%v", kind, audioType, videoType, err)
	}

	video.VideoSizeBytes = maxVideoBytes + 1
	if _, _, _, err = validateRecordingMedia(video); err == nil {
		t.Fatal("oversized video was accepted")
	}
}

func TestVideoExtensions(t *testing.T) {
	if got := extensionFor("video/webm"); got != "webm" {
		t.Fatalf("video/webm extension = %q", got)
	}
	if got := extensionFor("video/mp4"); got != "mp4" {
		t.Fatalf("video/mp4 extension = %q", got)
	}
}

func TestRecordingDownloadFilename(t *testing.T) {
	recordedAt := time.Date(2026, 8, 12, 20, 34, 19, 0, time.UTC)
	tests := []struct {
		name, title, asset, contentType, want string
		take                                  int
	}{
		{name: "lossless section take", title: "Warm up", take: 2, asset: "audio", contentType: "audio/wav", want: "2026-08-12-warm-up-take-2-audio.wav"},
		{name: "video section take", title: "Articulation & Flexibility!", take: 3, asset: "video", contentType: "video/webm", want: "2026-08-12-articulation-flexibility-take-3-video.webm"},
		{name: "uncategorized fallback", title: "", asset: "audio", contentType: "audio/webm", want: "2026-08-12-practice-audio.webm"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := recordingDownloadFilename(recordedAt, test.title, test.take, test.asset, test.contentType); got != test.want {
				t.Fatalf("recordingDownloadFilename() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestNormalizeShareAsset(t *testing.T) {
	tests := []struct {
		requested, mediaKind, want string
		wantError                  bool
	}{
		{requested: "", mediaKind: "audio", want: "audio"},
		{requested: "", mediaKind: "video", want: "video"},
		{requested: "audio", mediaKind: "video", want: "audio"},
		{requested: "video", mediaKind: "video", want: "video"},
		{requested: "video", mediaKind: "audio", wantError: true},
		{requested: "other", mediaKind: "video", wantError: true},
	}
	for _, test := range tests {
		got, err := normalizeShareAsset(test.requested, test.mediaKind)
		if test.wantError && err == nil {
			t.Fatalf("normalizeShareAsset(%q, %q) accepted an invalid asset", test.requested, test.mediaKind)
		}
		if !test.wantError && (err != nil || got != test.want) {
			t.Fatalf("normalizeShareAsset(%q, %q) = %q, %v; want %q", test.requested, test.mediaKind, got, err, test.want)
		}
	}
}

func TestNewPublicShareToken(t *testing.T) {
	first, err := newPublicShareToken()
	if err != nil || !publicShareTokenPattern.MatchString(first) {
		t.Fatalf("invalid public share token %q: %v", first, err)
	}
	second, err := newPublicShareToken()
	if err != nil || first == second {
		t.Fatalf("share tokens were not independently generated: first=%q second=%q err=%v", first, second, err)
	}
}

func TestNormalizeGuideToneDrill(t *testing.T) {
	valid := createGuideToneDrillRequest{
		TuneID: "blue-bossa", Instrument: "bb-trumpet", Mode: "learn", Tempo: 72,
		StartedAt: "2026-08-18T18:00:00Z",
	}
	got, startedAt, err := normalizeGuideToneDrill(valid)
	if err != nil || got.TuneID != "blue-bossa" || startedAt.IsZero() {
		t.Fatalf("valid guide-tone drill rejected: input=%+v start=%v err=%v", got, startedAt, err)
	}
	invalid := []createGuideToneDrillRequest{
		{TuneID: "other", Instrument: "bb-trumpet", Mode: "learn", Tempo: 72, StartedAt: valid.StartedAt},
		{TuneID: "blue-bossa", Instrument: "alto", Mode: "learn", Tempo: 72, StartedAt: valid.StartedAt},
		{TuneID: "blue-bossa", Instrument: "concert", Mode: "random", Tempo: 72, StartedAt: valid.StartedAt},
		{TuneID: "blue-bossa", Instrument: "concert", Mode: "tempo", Tempo: 400, StartedAt: valid.StartedAt},
	}
	for _, input := range invalid {
		if _, _, err := normalizeGuideToneDrill(input); err == nil {
			t.Fatalf("invalid guide-tone drill accepted: %+v", input)
		}
	}
}

func TestValidateGuideToneAttempt(t *testing.T) {
	playedMIDI, playedPitchClass, cents := 63, 3, -4.2
	valid := createGuideToneAttemptRequest{
		MeasureNumber: 1, ChordIndex: 0, ChordSymbol: "Cm7", TargetDegree: 3, ExpectedPitchClass: 3,
		PlayedMIDI: &playedMIDI, PlayedPitchClass: &playedPitchClass, Cents: &cents, Correct: true,
		ResponseMS: 840, OccurredAt: "2026-08-18T18:00:01Z",
	}
	if occurredAt, err := validateGuideToneAttempt(valid); err != nil || occurredAt.IsZero() {
		t.Fatalf("valid guide-tone attempt rejected: occurred=%v err=%v", occurredAt, err)
	}
	missed := valid
	missed.PlayedMIDI, missed.PlayedPitchClass, missed.Cents, missed.Correct = nil, nil, nil, false
	if _, err := validateGuideToneAttempt(missed); err != nil {
		t.Fatalf("missed guide-tone attempt rejected: %v", err)
	}
	incomplete := valid
	incomplete.PlayedPitchClass = nil
	if _, err := validateGuideToneAttempt(incomplete); err == nil {
		t.Fatal("incomplete played pitch was accepted")
	}
	inconsistent := valid
	inconsistent.PlayedPitchClass = &playedPitchClass
	inconsistent.ExpectedPitchClass = 10
	if _, err := validateGuideToneAttempt(inconsistent); err == nil {
		t.Fatal("inconsistent correct result was accepted")
	}
	invalidDegree := valid
	invalidDegree.TargetDegree = 5
	if _, err := validateGuideToneAttempt(invalidDegree); err == nil {
		t.Fatal("invalid target degree was accepted")
	}
}

func TestRecordingSectionLimits(t *testing.T) {
	if maxTakesPerBlock != 20 {
		t.Fatalf("max takes per block = %d, want 20", maxTakesPerBlock)
	}
	note := "  clean attacks today  "
	got, err := normalizeRecordingNote(&note)
	if err != nil || got != "clean attacks today" {
		t.Fatalf("valid note normalized to %q with error %v", got, err)
	}
	tooLong := strings.Repeat("x", maxTakeNoteBytes+1)
	if _, err := normalizeRecordingNote(&tooLong); err == nil {
		t.Fatal("oversized take note was accepted")
	}
	if _, err := normalizeRecordingNote(nil); err == nil {
		t.Fatal("missing take note was accepted")
	}
}

func TestRecordingPracticeMSIncludesDurableTakesOnly(t *testing.T) {
	recordings := []blockRecordingSummary{
		{Status: "ready", DurationMS: 171000},
		{Status: "uploading", DurationMS: 779000},
		{Status: "failed", DurationMS: 60000},
		{Status: "deleted", DurationMS: 60000},
	}
	if got := recordingPracticeMS(recordings); got != 950000 {
		t.Fatalf("recordingPracticeMS() = %d, want 950000", got)
	}
}

func TestBlockPracticeLimitMatchesRecordingPolicy(t *testing.T) {
	if maxBlockElapsedMS != maxTakesPerBlock*maxDurationMS {
		t.Fatalf("block practice limit = %d, want %d", maxBlockElapsedMS, maxTakesPerBlock*maxDurationMS)
	}
}

func TestReconcileBlockPracticeTimeUsesRecordingFloorAndCompletesGoal(t *testing.T) {
	updatedAt := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	block := practiceBlock{
		TargetMinutes: 10,
		ElapsedMS:     207000,
		Status:        "paused",
		UpdatedAt:     updatedAt,
		Recordings: []blockRecordingSummary{
			{Status: "ready", DurationMS: 171000},
			{Status: "ready", DurationMS: 779000},
		},
	}
	reconcileBlockPracticeTime(&block)
	if block.RecordedMS != 950000 || block.ElapsedMS != 950000 {
		t.Fatalf("reconciled durations = recorded %d elapsed %d, want 950000", block.RecordedMS, block.ElapsedMS)
	}
	if block.Status != "completed" || block.CompletedAt == nil || !block.CompletedAt.Equal(updatedAt) {
		t.Fatalf("goal was not completed: status=%q completedAt=%v", block.Status, block.CompletedAt)
	}
}

func TestReconcileBlockPracticeTimePreservesCancelledPracticeTime(t *testing.T) {
	block := practiceBlock{
		TargetMinutes: 20,
		ElapsedMS:     720000,
		Status:        "paused",
		Recordings:    []blockRecordingSummary{{Status: "ready", DurationMS: 600000}},
	}
	reconcileBlockPracticeTime(&block)
	if block.RecordedMS != 600000 || block.ElapsedMS != 720000 {
		t.Fatalf("cancelled practice was lost: recorded=%d elapsed=%d", block.RecordedMS, block.ElapsedMS)
	}
}

func TestParseArchiveDateRejectsNormalizedAndImpossibleDates(t *testing.T) {
	valid, err := parseArchiveDate("2026-08-09")
	if err != nil || valid.Format("2006-01-02") != "2026-08-09" {
		t.Fatalf("valid archive date rejected: date=%v err=%v", valid, err)
	}
	for _, value := range []string{"2026-8-9", "2026-02-30", "", "Aug 9, 2026"} {
		if _, err := parseArchiveDate(value); err == nil {
			t.Fatalf("invalid archive date %q was accepted", value)
		}
	}
}

func TestArchiveRangeIsBounded(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/v1/archive/calendar?from=2026-08-01&to=2026-08-31", nil)
	from, to, err := archiveRange(request)
	if err != nil || from.Format("2006-01-02") != "2026-08-01" || to.Format("2006-01-02") != "2026-08-31" {
		t.Fatalf("valid archive range rejected: from=%v to=%v err=%v", from, to, err)
	}
	for _, rawQuery := range []string{
		"from=2026-08-31&to=2026-08-01",
		"from=2025-01-01&to=2026-08-01",
		"from=bad&to=2026-08-01",
	} {
		request := httptest.NewRequest(http.MethodGet, "/v1/archive/calendar?"+rawQuery, nil)
		if _, _, err := archiveRange(request); err == nil {
			t.Fatalf("invalid archive range %q was accepted", rawQuery)
		}
	}
}

func TestNormalizeWaveformPeaks(t *testing.T) {
	peaks, err := normalizeWaveformPeaks([]float64{0, 0.123456, 1})
	if err != nil || len(peaks) != 3 || peaks[1] != 0.1235 {
		t.Fatalf("valid waveform rejected or not normalized: peaks=%v err=%v", peaks, err)
	}
	if _, err := normalizeWaveformPeaks([]float64{-0.1}); err == nil {
		t.Fatal("negative waveform peak was accepted")
	}
	if _, err := normalizeWaveformPeaks([]float64{1.1}); err == nil {
		t.Fatal("oversized waveform peak was accepted")
	}
	if _, err := normalizeWaveformPeaks(make([]float64, 1201)); err == nil {
		t.Fatal("oversized waveform payload was accepted")
	}
}
