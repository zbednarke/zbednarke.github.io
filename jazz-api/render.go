package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	maxRenderClips      = 24
	maxRenderDurationMS = 10 * 60 * 1000
	minRenderClipMS     = 500
	renderWidth         = 1920
	renderHeight        = 1080
	renderFrameRate     = 30
)

type renderClipRequest struct {
	RecordingID string `json:"recordingId"`
	StartMS     int    `json:"startMs"`
	EndMS       int    `json:"endMs"`
}

type renderMovieRequest struct {
	Title string              `json:"title"`
	Clips []renderClipRequest `json:"clips"`
}

type renderSource struct {
	RecordingID    uuid.UUID
	StartMS        int
	EndMS          int
	AudioObject    string
	VideoObject    string
	AudioURL       string
	VideoURL       string
	RecordingTitle string
}

func (app *application) renderClipStudioMovie(w http.ResponseWriter, r *http.Request) {
	var input renderMovieRequest
	if err := readJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	title := clean(input.Title, 120)
	if title == "" || len(input.Clips) == 0 || len(input.Clips) > maxRenderClips {
		writeError(w, http.StatusUnprocessableEntity, "render title or clip list is invalid")
		return
	}
	userID, err := app.userID(r.Context())
	if err != nil {
		app.serverError(w, err)
		return
	}
	sources := make([]renderSource, 0, len(input.Clips))
	totalDuration := 0
	for _, clip := range input.Clips {
		recordingID, parseErr := uuid.Parse(clip.RecordingID)
		if parseErr != nil || clip.StartMS < 0 || clip.EndMS-clip.StartMS < minRenderClipMS {
			writeError(w, http.StatusUnprocessableEntity, "render clip boundaries are invalid")
			return
		}
		var source renderSource
		var durationMS int
		var mediaKind string
		err = app.db.QueryRow(r.Context(), `
			SELECT r.object_name,COALESCE(r.video_object_name,''),COALESCE(r.duration_ms,0),COALESCE(r.media_kind,'audio'),COALESCE(pb.title,'')
			FROM recordings r LEFT JOIN practice_blocks pb ON pb.id=r.practice_block_id AND pb.user_id=r.user_id
			WHERE r.id=$1 AND r.user_id=$2 AND r.status='ready'`, recordingID, userID).
			Scan(&source.AudioObject, &source.VideoObject, &durationMS, &mediaKind, &source.RecordingTitle)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "render recording not found")
			return
		}
		if err != nil {
			app.serverError(w, err)
			return
		}
		if mediaKind != "video" || source.VideoObject == "" || clip.EndMS > durationMS {
			writeError(w, http.StatusUnprocessableEntity, "every rendered clip must have synchronized video and lossless audio")
			return
		}
		totalDuration += clip.EndMS - clip.StartMS
		if totalDuration > maxRenderDurationMS {
			writeError(w, http.StatusUnprocessableEntity, "render output cannot exceed ten minutes")
			return
		}
		source.RecordingID = recordingID
		source.StartMS = clip.StartMS
		source.EndMS = clip.EndMS
		sources = append(sources, source)
	}
	expires := time.Now().Add(2 * time.Hour)
	for index := range sources {
		sources[index].VideoURL, err = app.signedRecordingObjectURL(r.Context(), sources[index].VideoObject, expires, nil)
		if err != nil {
			app.serverError(w, err)
			return
		}
		sources[index].AudioURL, err = app.signedRecordingObjectURL(r.Context(), sources[index].AudioObject, expires, nil)
		if err != nil {
			app.serverError(w, err)
			return
		}
	}
	renderID := uuid.New()
	filename := renderDownloadFilename(title)
	objectName := fmt.Sprintf("renders/%s/%s/%s", userID, renderID, filename)
	if err := app.streamRenderedMovie(r.Context(), objectName, renderID, sources); err != nil {
		app.logger.Error("clip render failed", "render_id", renderID, "user_id", userID, "error", err)
		writeError(w, http.StatusInternalServerError, "render failed; the source recordings were not changed")
		return
	}
	downloadExpires := time.Now().Add(time.Hour)
	downloadURL, err := app.signedRecordingObjectURL(r.Context(), objectName, downloadExpires, url.Values{
		"response-content-disposition": {`attachment; filename="` + filename + `"`},
		"response-content-type":        {"video/mp4"},
	})
	if err != nil {
		app.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": renderID, "filename": filename, "url": downloadURL, "expiresAt": downloadExpires,
		"retainedUntil": time.Now().Add(24 * time.Hour), "durationMs": totalDuration,
		"quality": map[string]any{"video": "1080p H.264, CRF 18", "audio": "Lossless ALAC, 48 kHz, original levels", "audioLossless": true},
	})
}

func (app *application) streamRenderedMovie(ctx context.Context, objectName string, renderID uuid.UUID, sources []renderSource) error {
	args := buildFFmpegRenderArgs(sources)
	command := exec.CommandContext(ctx, envOr("FFMPEG_PATH", "ffmpeg"), args...)
	writer := app.storage.Bucket(app.cfg.Bucket).Object(objectName).NewWriter(ctx)
	writer.ChunkSize = 16 << 20
	writer.ContentType = "video/mp4"
	writer.CacheControl = "private, no-store"
	writer.CustomTime = time.Now().UTC()
	writer.Metadata = map[string]string{
		"renderId": renderID.String(), "temporary": "true", "deleteAfter": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
		"audioQuality": "lossless-alac-48khz", "videoQuality": "h264-1080p-crf18",
	}
	command.Stdout = writer
	var stderr cappedBuffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		_ = writer.CloseWithError(err)
		_ = app.storage.Bucket(app.cfg.Bucket).Object(objectName).Delete(context.Background())
		return fmt.Errorf("ffmpeg: %w: %s", err, stderr.String())
	}
	if err := writer.Close(); err != nil {
		_ = app.storage.Bucket(app.cfg.Bucket).Object(objectName).Delete(context.Background())
		return fmt.Errorf("upload rendered movie: %w", err)
	}
	return nil
}

func buildFFmpegRenderArgs(sources []renderSource) []string {
	args := []string{"-hide_banner", "-loglevel", "warning"}
	for _, source := range sources {
		start := formatFFmpegSeconds(source.StartMS)
		duration := formatFFmpegSeconds(source.EndMS - source.StartMS)
		args = append(args, "-ss", start, "-t", duration, "-i", source.VideoURL)
		args = append(args, "-ss", start, "-t", duration, "-i", source.AudioURL)
	}
	filters := make([]string, 0, len(sources)*2+1)
	concatInputs := strings.Builder{}
	for index := range sources {
		videoInput, audioInput := index*2, index*2+1
		filters = append(filters,
			fmt.Sprintf("[%d:v:0]scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=black,fps=%d,format=yuv420p,setpts=PTS-STARTPTS[v%d]", videoInput, renderWidth, renderHeight, renderWidth, renderHeight, renderFrameRate, index),
			fmt.Sprintf("[%d:a:0]aresample=48000,aformat=sample_fmts=s32:channel_layouts=mono,asetpts=PTS-STARTPTS[a%d]", audioInput, index))
		concatInputs.WriteString(fmt.Sprintf("[v%d][a%d]", index, index))
	}
	filters = append(filters, fmt.Sprintf("%sconcat=n=%d:v=1:a=1[outv][outa]", concatInputs.String(), len(sources)))
	args = append(args, "-filter_complex", strings.Join(filters, ";"), "-map", "[outv]", "-map", "[outa]",
		"-c:v", "libx264", "-preset", "medium", "-crf", "18", "-profile:v", "high", "-pix_fmt", "yuv420p", "-threads", "2",
		"-c:a", "alac", "-ar", "48000", "-movflags", "+frag_keyframe+empty_moov+default_base_moof", "-f", "mp4", "pipe:1")
	return args
}

func formatFFmpegSeconds(milliseconds int) string {
	return strconv.FormatFloat(float64(milliseconds)/1000, 'f', 3, 64)
}

func renderDownloadFilename(title string) string {
	name := strings.Trim(downloadFilenamePartPattern.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if name == "" {
		name = "jazz-practice"
	}
	if len(name) > 72 {
		name = strings.Trim(name[:72], "-")
	}
	return name + ".mp4"
}

type cappedBuffer struct{ bytes.Buffer }

func (buffer *cappedBuffer) Write(contents []byte) (int, error) {
	const max = 16 << 10
	if buffer.Len() < max {
		remaining := max - buffer.Len()
		_, _ = buffer.Buffer.Write(contents[:min(len(contents), remaining)])
	}
	return len(contents), nil
}
