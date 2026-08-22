package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const clipAnalysisVersion = "waveform-v1"

type clipCandidateRow struct {
	ID              uuid.UUID       `json:"id"`
	RecordingID     uuid.UUID       `json:"recordingId"`
	StartMS         int             `json:"startMs"`
	EndMS           int             `json:"endMs"`
	Score           float64         `json:"score"`
	Reasons         json.RawMessage `json:"reasons"`
	ScoreBreakdown  json.RawMessage `json:"scoreBreakdown"`
	Source          string          `json:"source"`
	ReviewStatus    string          `json:"reviewStatus"`
	Title           string          `json:"title,omitempty"`
	Notes           string          `json:"notes,omitempty"`
	AnalysisVersion string          `json:"analysisVersion"`
}

func (app *application) clipStudioDay(w http.ResponseWriter, r *http.Request) {
	date := r.PathValue("date")
	if _, err := time.Parse("2006-01-02", date); err != nil {
		writeError(w, http.StatusBadRequest, "invalid practice date")
		return
	}
	userID, err := app.userID(r.Context())
	if err != nil {
		app.serverError(w, err)
		return
	}
	recordings, err := app.clipStudioRecordings(r, userID, date)
	if err != nil {
		app.serverError(w, err)
		return
	}
	candidates, err := app.clipCandidates(r, userID, date)
	if err != nil {
		app.serverError(w, err)
		return
	}
	var analysisVersion string
	var analyzedAt time.Time
	var analyzedRecordingCount int
	analysisErr := app.db.QueryRow(r.Context(), `SELECT analysis_version,analyzed_at,recording_count FROM clip_day_analyses WHERE user_id=$1 AND practice_date=$2::date`, userID, date).Scan(&analysisVersion, &analyzedAt, &analyzedRecordingCount)
	if analysisErr != nil && !errors.Is(analysisErr, pgx.ErrNoRows) {
		app.serverError(w, analysisErr)
		return
	}
	needsScan := errors.Is(analysisErr, pgx.ErrNoRows) || analysisVersion != clipAnalysisVersion || analyzedRecordingCount != len(recordings)
	analysis := map[string]any{"version": clipAnalysisVersion, "needsScan": needsScan, "recordingCount": len(recordings)}
	if !analyzedAt.IsZero() {
		analysis["analyzedAt"] = analyzedAt
	}
	writeJSON(w, http.StatusOK, map[string]any{"date": date, "recordings": recordings, "candidates": candidates, "analysis": analysis})
}

func (app *application) clipStudioRecordings(r *http.Request, userID uuid.UUID, date string) ([]recordingRow, error) {
	rows, err := app.db.Query(r.Context(), `
		SELECT r.id,r.content_type,COALESCE(r.codec,''),COALESCE(r.size_bytes,r.expected_size_bytes),COALESCE(r.duration_ms,0),
		COALESCE(r.sample_rate,0),COALESCE(r.channels,0),r.recorded_at,r.status,
		COALESCE(r.tune_id,''),COALESCE(r.mission_id,''),r.skill_ids,COALESCE(r.take_number,0),COALESCE(r.notes,''),
		COALESCE(r.practice_session_id,''),COALESCE(ps.title,''),COALESCE(r.practice_block_id::text,''),
		COALESCE(pb.practice_date::text,''),COALESCE(pb.block_key,''),COALESCE(pb.title,''),COALESCE(pb.category,''),COALESCE(pb.track,''),r.object_name,
		COALESCE(r.media_kind,'audio'),COALESCE(r.video_content_type,''),COALESCE(r.video_codec,''),COALESCE(r.video_size_bytes,r.video_expected_size_bytes,0),
		COALESCE(r.video_width,0),COALESCE(r.video_height,0),COALESCE(r.video_frame_rate,0),COALESCE(r.video_object_name,''),r.waveform_peaks
		FROM recordings r
		LEFT JOIN practice_sessions ps ON ps.id::text=r.practice_session_id AND ps.user_id=r.user_id
		LEFT JOIN practice_blocks pb ON pb.id=r.practice_block_id AND pb.user_id=r.user_id
		WHERE r.user_id=$1 AND r.status='ready'
		AND COALESCE(pb.practice_date,(r.recorded_at AT TIME ZONE 'America/Los_Angeles')::date)=$2::date
		ORDER BY r.recorded_at`, userID, date)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]recordingRow, 0)
	for rows.Next() {
		var item recordingRow
		var skills, waveform []byte
		if err := rows.Scan(&item.ID, &item.ContentType, &item.Codec, &item.SizeBytes, &item.DurationMS, &item.SampleRate, &item.Channels, &item.RecordedAt, &item.Status,
			&item.TuneID, &item.MissionID, &skills, &item.TakeNumber, &item.Notes, &item.SessionID, &item.SessionTitle, &item.BlockID, &item.BlockDate, &item.BlockKey, &item.BlockTitle, &item.BlockCategory, &item.BlockTrack, &item.ObjectName,
			&item.MediaKind, &item.VideoContentType, &item.VideoCodec, &item.VideoSizeBytes, &item.VideoWidth, &item.VideoHeight, &item.VideoFrameRate, &item.VideoObjectName, &waveform); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(skills, &item.SkillIDs)
		_ = json.Unmarshal(waveform, &item.WaveformPeaks)
		result = append(result, item)
	}
	return result, rows.Err()
}

func (app *application) clipCandidates(r *http.Request, userID uuid.UUID, date string) ([]clipCandidateRow, error) {
	rows, err := app.db.Query(r.Context(), `
		SELECT c.id,c.recording_id,c.start_ms,c.end_ms,c.score,c.reasons,c.score_breakdown,c.source,c.review_status,
		COALESCE(c.title,''),COALESCE(c.notes,''),c.analysis_version
		FROM clip_candidates c JOIN recordings r ON r.id=c.recording_id
		LEFT JOIN practice_blocks pb ON pb.id=r.practice_block_id AND pb.user_id=r.user_id
		WHERE c.user_id=$1 AND r.status='ready' AND COALESCE(pb.practice_date,(r.recorded_at AT TIME ZONE 'America/Los_Angeles')::date)=$2::date
		ORDER BY CASE c.review_status WHEN 'kept' THEN 0 WHEN 'suggested' THEN 1 ELSE 2 END,c.score DESC`, userID, date)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]clipCandidateRow, 0)
	for rows.Next() {
		var item clipCandidateRow
		if err := rows.Scan(&item.ID, &item.RecordingID, &item.StartMS, &item.EndMS, &item.Score, &item.Reasons, &item.ScoreBreakdown, &item.Source, &item.ReviewStatus, &item.Title, &item.Notes, &item.AnalysisVersion); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (app *application) scanClipStudioDay(w http.ResponseWriter, r *http.Request) {
	date := r.PathValue("date")
	if _, err := time.Parse("2006-01-02", date); err != nil {
		writeError(w, http.StatusBadRequest, "invalid practice date")
		return
	}
	userID, err := app.userID(r.Context())
	if err != nil {
		app.serverError(w, err)
		return
	}
	recordings, err := app.clipStudioRecordings(r, userID, date)
	if err != nil {
		app.serverError(w, err)
		return
	}
	type daySuggestion struct {
		recordingID uuid.UUID
		clipSuggestion
	}
	daySuggestions := make([]daySuggestion, 0)
	for _, recording := range recordings {
		for _, suggestion := range scanWaveformForClips(recording.WaveformPeaks, recording.DurationMS) {
			daySuggestions = append(daySuggestions, daySuggestion{recording.ID, suggestion})
		}
	}
	sort.Slice(daySuggestions, func(i, j int) bool { return daySuggestions[i].Score > daySuggestions[j].Score })
	if len(daySuggestions) > 24 {
		daySuggestions = daySuggestions[:24]
	}
	tx, err := app.db.Begin(r.Context())
	if err != nil {
		app.serverError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	_, err = tx.Exec(r.Context(), `
		DELETE FROM clip_candidates c USING recordings rec
		WHERE c.recording_id=rec.id AND c.user_id=$1 AND c.review_status='suggested' AND c.source='activity-scan'
		AND COALESCE((SELECT pb.practice_date FROM practice_blocks pb WHERE pb.id=rec.practice_block_id AND pb.user_id=rec.user_id),(rec.recorded_at AT TIME ZONE 'America/Los_Angeles')::date)=$2::date`, userID, date)
	if err != nil {
		app.serverError(w, err)
		return
	}
	created := 0
	for _, suggestion := range daySuggestions {
		reasons, _ := json.Marshal(suggestion.Reasons)
		breakdown, _ := json.Marshal(map[string]float64{"activityCoverage": suggestion.Coverage, "phraseContinuity": suggestion.Continuity})
		result, execErr := tx.Exec(r.Context(), `
				INSERT INTO clip_candidates (id,user_id,recording_id,start_ms,end_ms,score,reasons,score_breakdown)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
				ON CONFLICT (recording_id,start_ms,end_ms,analysis_version) DO UPDATE SET score=EXCLUDED.score,reasons=EXCLUDED.reasons,score_breakdown=EXCLUDED.score_breakdown,updated_at=now()`,
			uuid.New(), userID, suggestion.recordingID, suggestion.StartMS, suggestion.EndMS, suggestion.Score, reasons, breakdown)
		if execErr != nil {
			app.serverError(w, execErr)
			return
		}
		created += int(result.RowsAffected())
	}
	_, err = tx.Exec(r.Context(), `
		INSERT INTO clip_day_analyses (user_id,practice_date,analysis_version,recording_count,analyzed_at)
		VALUES ($1,$2::date,$3,$4,now())
		ON CONFLICT (user_id,practice_date) DO UPDATE SET analysis_version=EXCLUDED.analysis_version,recording_count=EXCLUDED.recording_count,analyzed_at=now()`,
		userID, date, clipAnalysisVersion, len(recordings))
	if err != nil {
		app.serverError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		app.serverError(w, err)
		return
	}
	candidates, err := app.clipCandidates(r, userID, date)
	if err != nil {
		app.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"date": date, "scannedRecordings": len(recordings), "updatedCandidates": created, "candidates": candidates})
}

type clipCandidateUpdate struct {
	StartMS      *int    `json:"startMs"`
	EndMS        *int    `json:"endMs"`
	ReviewStatus *string `json:"reviewStatus"`
	Title        *string `json:"title"`
	Notes        *string `json:"notes"`
}

func (app *application) updateClipCandidate(w http.ResponseWriter, r *http.Request) {
	candidateID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid candidate id")
		return
	}
	var input clipCandidateUpdate
	if err := readJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	userID, err := app.userID(r.Context())
	if err != nil {
		app.serverError(w, err)
		return
	}
	if input.ReviewStatus != nil && *input.ReviewStatus != "suggested" && *input.ReviewStatus != "kept" && *input.ReviewStatus != "rejected" {
		writeError(w, http.StatusUnprocessableEntity, "clip candidate update is invalid")
		return
	}
	var startArg, endArg, statusArg any
	if input.StartMS != nil {
		startArg = *input.StartMS
	}
	if input.EndMS != nil {
		endArg = *input.EndMS
	}
	if input.ReviewStatus != nil {
		statusArg = *input.ReviewStatus
	}
	title, notes := "", ""
	if input.Title != nil {
		title = clean(*input.Title, 120)
	}
	if input.Notes != nil {
		notes = clean(*input.Notes, 2000)
	}
	var startMS, endMS int
	var status, savedTitle, savedNotes string
	var updatedAt time.Time
	err = app.db.QueryRow(r.Context(), `
		UPDATE clip_candidates c SET
		start_ms=COALESCE($1,c.start_ms),end_ms=COALESCE($2,c.end_ms),review_status=COALESCE($3,c.review_status),
		title=CASE WHEN $4 THEN NULLIF($5,'') ELSE c.title END,
		notes=CASE WHEN $6 THEN NULLIF($7,'') ELSE c.notes END,updated_at=now()
		FROM recordings rec
		WHERE c.id=$8 AND c.user_id=$9 AND rec.id=c.recording_id AND rec.status='ready'
		AND COALESCE($1,c.start_ms)>=0
		AND COALESCE($2,c.end_ms)>COALESCE($1,c.start_ms)
		AND COALESCE($2,c.end_ms)<=rec.duration_ms
		RETURNING c.start_ms,c.end_ms,c.review_status,COALESCE(c.title,''),COALESCE(c.notes,''),c.updated_at`,
		startArg, endArg, statusArg, input.Title != nil, title, input.Notes != nil, notes, candidateID, userID).
		Scan(&startMS, &endMS, &status, &savedTitle, &savedNotes, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		checkErr := app.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM clip_candidates c JOIN recordings rec ON rec.id=c.recording_id WHERE c.id=$1 AND c.user_id=$2 AND rec.status='ready')`, candidateID, userID).Scan(&exists)
		if checkErr != nil {
			app.serverError(w, checkErr)
			return
		}
		if exists {
			writeError(w, http.StatusUnprocessableEntity, "clip candidate update is invalid")
		} else {
			writeError(w, http.StatusNotFound, "clip candidate not found")
		}
		return
	}
	if err != nil {
		app.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": candidateID, "startMs": startMS, "endMs": endMS, "reviewStatus": status, "title": savedTitle, "notes": savedNotes, "updatedAt": updatedAt})
}
