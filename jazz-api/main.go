package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	iamcredentials "cloud.google.com/go/iam/credentials/apiv1"
	credentialspb "cloud.google.com/go/iam/credentials/apiv1/credentialspb"
	"cloud.google.com/go/storage"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	maxRequestBytes  = 2 << 20
	maxAudioBytes    = int64(4) << 30
	maxVideoBytes    = int64(32) << 30
	maxDurationMS    = 4 * 60 * 60 * 1000
	maxTakesPerBlock = 20
	maxTakeNoteBytes = 2000
)

var datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
var downloadFilenamePartPattern = regexp.MustCompile(`[^a-z0-9]+`)
var publicShareTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{32}$`)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type config struct {
	Port                string
	DatabaseURL         string
	GatewayKey          string
	Bucket              string
	ServiceAccountEmail string
	PublicShareBaseURL  string
	AllowInsecureLocal  bool
}

type application struct {
	cfg         config
	db          *pgxpool.Pool
	storage     *storage.Client
	tokenSource oauth2.TokenSource
	iamSigner   *iamcredentials.IamCredentialsClient
	httpClient  *http.Client
	logger      *slog.Logger
}

type contextKey string

const userSubjectKey contextKey = "user-subject"

type practiceEntry struct {
	ID      string  `json:"id"`
	Date    string  `json:"date"`
	Minutes float64 `json:"minutes"`
	Track   string  `json:"track"`
	Note    string  `json:"note"`
	Preset  bool    `json:"preset,omitempty"`
}

type campaignState struct {
	Version       int             `json:"version"`
	SkillLevels   map[string]int  `json:"skillLevels"`
	Objectives    map[string]bool `json:"objectives"`
	Repertoire    map[string]int  `json:"repertoire"`
	Bosses        map[string]bool `json:"bosses"`
	Scene         map[string]bool `json:"scene"`
	Practice      []practiceEntry `json:"practice"`
	PeopleCanCall int             `json:"peopleCanCall"`
}

type syncRequest struct {
	ClientMutationID string          `json:"clientMutationId"`
	EventType        string          `json:"eventType"`
	BaseRevision     int64           `json:"baseRevision"`
	State            json.RawMessage `json:"state"`
}

type stateResponse struct {
	HasState bool            `json:"hasState"`
	Revision int64           `json:"revision"`
	State    json.RawMessage `json:"state,omitempty"`
}

type recordingInitRequest struct {
	MediaKind         string    `json:"mediaKind"`
	ContentType       string    `json:"contentType"`
	Codec             string    `json:"codec"`
	SizeBytes         int64     `json:"sizeBytes"`
	DurationMS        int       `json:"durationMs"`
	SampleRate        int       `json:"sampleRate"`
	Channels          int       `json:"channels"`
	RecordedAt        string    `json:"recordedAt"`
	PracticeSessionID string    `json:"practiceSessionId"`
	PracticeBlockID   string    `json:"practiceBlockId"`
	TuneID            string    `json:"tuneId"`
	MissionID         string    `json:"missionId"`
	SkillIDs          []string  `json:"skillIds"`
	TakeNumber        int       `json:"takeNumber"`
	Notes             string    `json:"notes"`
	VideoContentType  string    `json:"videoContentType"`
	VideoCodec        string    `json:"videoCodec"`
	VideoSizeBytes    int64     `json:"videoSizeBytes"`
	VideoWidth        int       `json:"videoWidth"`
	VideoHeight       int       `json:"videoHeight"`
	VideoFrameRate    float64   `json:"videoFrameRate"`
	WaveformPeaks     []float64 `json:"waveformPeaks"`
}

type recordingCompleteRequest struct {
	Asset string `json:"asset"`
}

type recordingUpdateRequest struct {
	Notes *string `json:"notes"`
}

type recordingRow struct {
	ID                     uuid.UUID `json:"id"`
	ContentType            string    `json:"contentType"`
	Codec                  string    `json:"codec,omitempty"`
	SizeBytes              int64     `json:"sizeBytes,omitempty"`
	DurationMS             int       `json:"durationMs,omitempty"`
	SampleRate             int       `json:"sampleRate,omitempty"`
	Channels               int       `json:"channels,omitempty"`
	RecordedAt             time.Time `json:"recordedAt"`
	Status                 string    `json:"status"`
	TuneID                 string    `json:"tuneId,omitempty"`
	MissionID              string    `json:"missionId,omitempty"`
	SkillIDs               []string  `json:"skillIds"`
	TakeNumber             int       `json:"takeNumber,omitempty"`
	Notes                  string    `json:"notes,omitempty"`
	SessionID              string    `json:"practiceSessionId,omitempty"`
	SessionTitle           string    `json:"practiceSessionTitle,omitempty"`
	BlockID                string    `json:"practiceBlockId,omitempty"`
	BlockDate              string    `json:"practiceDate,omitempty"`
	BlockKey               string    `json:"practiceBlockKey,omitempty"`
	BlockTitle             string    `json:"practiceBlockTitle,omitempty"`
	BlockCategory          string    `json:"practiceBlockCategory,omitempty"`
	BlockTrack             string    `json:"practiceBlockTrack,omitempty"`
	ObjectName             string    `json:"-"`
	MediaKind              string    `json:"mediaKind"`
	VideoContentType       string    `json:"videoContentType,omitempty"`
	VideoCodec             string    `json:"videoCodec,omitempty"`
	VideoSizeBytes         int64     `json:"videoSizeBytes,omitempty"`
	VideoWidth             int       `json:"videoWidth,omitempty"`
	VideoHeight            int       `json:"videoHeight,omitempty"`
	VideoFrameRate         float64   `json:"videoFrameRate,omitempty"`
	VideoObjectName        string    `json:"-"`
	VideoPlaybackOptimized bool      `json:"videoPlaybackOptimized,omitempty"`
	WaveformPeaks          []float64 `json:"waveformPeaks,omitempty"`
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		slog.Error("configuration error", "error", err)
		os.Exit(1)
	}

	ctx := context.Background()
	db, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("database configuration failed", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	if err := db.Ping(ctx); err != nil {
		slog.Error("database connection failed", "error", err)
		os.Exit(1)
	}
	if err := migrate(ctx, db); err != nil {
		slog.Error("database migration failed", "error", err)
		os.Exit(1)
	}

	storageClient, err := storage.NewClient(ctx)
	if err != nil {
		slog.Error("storage client failed", "error", err)
		os.Exit(1)
	}
	defer storageClient.Close()
	tokenSource, err := google.DefaultTokenSource(ctx, storage.ScopeReadWrite)
	if err != nil {
		slog.Error("google credentials failed", "error", err)
		os.Exit(1)
	}
	iamSigner, err := iamcredentials.NewIamCredentialsClient(ctx)
	if err != nil {
		slog.Error("signing client failed", "error", err)
		os.Exit(1)
	}
	defer iamSigner.Close()

	app := &application{
		cfg: cfg, db: db, storage: storageClient, tokenSource: tokenSource,
		iamSigner: iamSigner, httpClient: &http.Client{Timeout: 15 * time.Second}, logger: slog.Default(),
	}
	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Clip Studio renders stream a bounded ten-minute movie through FFmpeg.
		// Cloud Run owns the request deadline; a server-level write deadline would
		// terminate valid renders before their signed download response is sent.
		WriteTimeout: 0,
		IdleTimeout:  90 * time.Second,
	}
	slog.Info("jazz API listening", "port", cfg.Port)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func loadConfig() (config, error) {
	cfg := config{
		Port:                envOr("PORT", "8080"),
		DatabaseURL:         strings.TrimSpace(os.Getenv("DATABASE_URL")),
		GatewayKey:          strings.TrimSpace(os.Getenv("GATEWAY_KEY")),
		Bucket:              strings.TrimSpace(os.Getenv("GCS_BUCKET")),
		ServiceAccountEmail: strings.TrimSpace(os.Getenv("GCP_SERVICE_ACCOUNT")),
		PublicShareBaseURL:  strings.TrimRight(envOr("PUBLIC_SHARE_BASE_URL", "https://zachbednarke.com/jazz/share"), "/"),
		AllowInsecureLocal:  os.Getenv("JAZZ_ALLOW_INSECURE_LOCAL") == "1",
	}
	if cfg.DatabaseURL == "" || cfg.Bucket == "" || cfg.ServiceAccountEmail == "" {
		return cfg, errors.New("DATABASE_URL, GCS_BUCKET, and GCP_SERVICE_ACCOUNT are required")
	}
	if cfg.GatewayKey == "" && !cfg.AllowInsecureLocal {
		return cfg, errors.New("GATEWAY_KEY is required outside local development")
	}
	return cfg, nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func migrate(ctx context.Context, db *pgxpool.Pool) error {
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		contents, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return err
		}
		if _, err := db.Exec(ctx, string(contents)); err != nil {
			return fmt.Errorf("%s: %w", entry.Name(), err)
		}
	}
	return nil
}

func (app *application) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", app.health)
	mux.HandleFunc("GET /v1/public/recordings/{token}", app.publicRecordingShare)
	mux.Handle("GET /v1/state", app.authenticate(http.HandlerFunc(app.getState)))
	mux.Handle("POST /v1/sync", app.authenticate(http.HandlerFunc(app.syncState)))
	mux.Handle("GET /v1/practice-sessions", app.authenticate(http.HandlerFunc(app.listPracticeSessions)))
	mux.Handle("POST /v1/practice-sessions", app.authenticate(http.HandlerFunc(app.createPracticeSession)))
	mux.Handle("GET /v1/practice-sessions/{id}", app.authenticate(http.HandlerFunc(app.getPracticeSession)))
	mux.Handle("PATCH /v1/practice-sessions/{id}", app.authenticate(http.HandlerFunc(app.updatePracticeSession)))
	mux.Handle("POST /v1/practice-sessions/{id}/activities", app.authenticate(http.HandlerFunc(app.createPracticeActivity)))
	mux.Handle("GET /v1/practice-sessions/{id}/blocks", app.authenticate(http.HandlerFunc(app.listPracticeBlocks)))
	mux.Handle("POST /v1/practice-sessions/{id}/blocks", app.authenticate(http.HandlerFunc(app.bootstrapPracticeBlocks)))
	mux.Handle("PATCH /v1/practice-blocks/{id}", app.authenticate(http.HandlerFunc(app.updatePracticeBlock)))
	mux.Handle("GET /v1/archive/calendar", app.authenticate(http.HandlerFunc(app.archiveCalendar)))
	mux.Handle("GET /v1/archive/days/{date}", app.authenticate(http.HandlerFunc(app.archiveDay)))
	mux.Handle("GET /v1/studio/days/{date}", app.authenticate(http.HandlerFunc(app.clipStudioDay)))
	mux.Handle("POST /v1/studio/days/{date}/scan", app.authenticate(http.HandlerFunc(app.scanClipStudioDay)))
	mux.Handle("PATCH /v1/studio/candidates/{id}", app.authenticate(http.HandlerFunc(app.updateClipCandidate)))
	mux.Handle("POST /v1/studio/renders", app.authenticate(http.HandlerFunc(app.renderClipStudioMovie)))
	mux.Handle("POST /v1/guide-tone-drills", app.authenticate(http.HandlerFunc(app.createGuideToneDrill)))
	mux.Handle("PATCH /v1/guide-tone-drills/{id}", app.authenticate(http.HandlerFunc(app.updateGuideToneDrill)))
	mux.Handle("POST /v1/guide-tone-drills/{id}/attempts", app.authenticate(http.HandlerFunc(app.createGuideToneAttempt)))
	mux.Handle("GET /v1/guide-tone-drills/summary", app.authenticate(http.HandlerFunc(app.guideToneDrillSummary)))
	mux.Handle("GET /v1/recordings", app.authenticate(http.HandlerFunc(app.listRecordings)))
	mux.Handle("POST /v1/recordings/init", app.authenticate(http.HandlerFunc(app.initRecording)))
	mux.Handle("POST /v1/recordings/{id}/complete", app.authenticate(http.HandlerFunc(app.completeRecording)))
	mux.Handle("POST /v1/recordings/{id}/playback-url", app.authenticate(http.HandlerFunc(app.recordingPlaybackURL)))
	mux.Handle("POST /v1/recordings/{id}/share-url", app.authenticate(http.HandlerFunc(app.recordingShareURL)))
	mux.Handle("PATCH /v1/recordings/{id}", app.authenticate(http.HandlerFunc(app.updateRecording)))
	mux.Handle("DELETE /v1/recordings/{id}", app.authenticate(http.HandlerFunc(app.deleteRecording)))
	return app.recoverPanic(app.logRequests(mux))
}

func (app *application) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providedKey := r.Header.Get("X-Jazz-Gateway-Key")
		if !app.cfg.AllowInsecureLocal && subtle.ConstantTimeCompare([]byte(providedKey), []byte(app.cfg.GatewayKey)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		subject := strings.TrimSpace(r.Header.Get("X-Jazz-User"))
		if subject == "" && app.cfg.AllowInsecureLocal {
			subject = "local"
		}
		if subject == "" || len(subject) > 128 {
			writeError(w, http.StatusUnauthorized, "missing authenticated user")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userSubjectKey, subject)))
	})
}

func (app *application) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := app.db.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (app *application) userID(ctx context.Context) (uuid.UUID, error) {
	subject, _ := ctx.Value(userSubjectKey).(string)
	newID := uuid.New()
	var id uuid.UUID
	err := app.db.QueryRow(ctx, `
		INSERT INTO app_users (id, auth_subject) VALUES ($1, $2)
		ON CONFLICT (auth_subject) DO UPDATE SET auth_subject = EXCLUDED.auth_subject
		RETURNING id`, newID, subject).Scan(&id)
	return id, err
}

func (app *application) getState(w http.ResponseWriter, r *http.Request) {
	userID, err := app.userID(r.Context())
	if err != nil {
		app.serverError(w, err)
		return
	}
	var response stateResponse
	err = app.db.QueryRow(r.Context(), `SELECT revision, data FROM campaign_state WHERE user_id = $1`, userID).
		Scan(&response.Revision, &response.State)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, stateResponse{HasState: false, Revision: 0})
		return
	}
	if err != nil {
		app.serverError(w, err)
		return
	}
	response.HasState = true
	writeJSON(w, http.StatusOK, response)
}

func (app *application) syncState(w http.ResponseWriter, r *http.Request) {
	var input syncRequest
	if err := readJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	mutationID, err := uuid.Parse(input.ClientMutationID)
	if err != nil || len(input.EventType) == 0 || len(input.EventType) > 80 {
		writeError(w, http.StatusBadRequest, "invalid mutation")
		return
	}
	if err := validateCampaignState(input.State); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	userID, err := app.userID(r.Context())
	if err != nil {
		app.serverError(w, err)
		return
	}

	tx, err := app.db.Begin(r.Context())
	if err != nil {
		app.serverError(w, err)
		return
	}
	defer tx.Rollback(r.Context())

	var duplicate bool
	err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM progress_events WHERE user_id=$1 AND client_mutation_id=$2)`, userID, mutationID).Scan(&duplicate)
	if err != nil {
		app.serverError(w, err)
		return
	}

	var revision int64
	var current json.RawMessage
	err = tx.QueryRow(r.Context(), `SELECT revision, data FROM campaign_state WHERE user_id=$1 FOR UPDATE`, userID).Scan(&revision, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		revision = 0
		current = nil
	} else if err != nil {
		app.serverError(w, err)
		return
	}
	if duplicate {
		if err := tx.Commit(r.Context()); err != nil {
			app.serverError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, stateResponse{HasState: current != nil, Revision: revision, State: current})
		return
	}
	if input.BaseRevision != revision {
		writeJSON(w, http.StatusConflict, stateResponse{HasState: current != nil, Revision: revision, State: current})
		return
	}

	nextRevision := revision + 1
	_, err = tx.Exec(r.Context(), `
		INSERT INTO campaign_state (user_id, data, revision) VALUES ($1, $2, $3)
		ON CONFLICT (user_id) DO UPDATE SET data=EXCLUDED.data, revision=EXCLUDED.revision, updated_at=now()`,
		userID, input.State, nextRevision)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO progress_events (user_id, client_mutation_id, event_type, payload) VALUES ($1,$2,$3,$4)`,
			userID, mutationID, input.EventType, input.State)
	}
	if err != nil {
		app.serverError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		app.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, stateResponse{HasState: true, Revision: nextRevision, State: input.State})
}

func validateCampaignState(raw json.RawMessage) error {
	if len(raw) == 0 || len(raw) > maxRequestBytes {
		return errors.New("campaign state is missing or too large")
	}
	var state campaignState
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return errors.New("campaign state is invalid")
	}
	if state.Version < 1 || state.Version > 100 || state.PeopleCanCall < 0 || state.PeopleCanCall > 999 {
		return errors.New("campaign state values are out of range")
	}
	if len(state.SkillLevels) > 200 || len(state.Objectives) > 500 || len(state.Repertoire) > 500 || len(state.Bosses) > 500 || len(state.Scene) > 500 || len(state.Practice) > 50000 {
		return errors.New("campaign state contains too many entries")
	}
	for _, level := range state.SkillLevels {
		if level < 0 || level > 4 {
			return errors.New("skill level is out of range")
		}
	}
	for _, stage := range state.Repertoire {
		if stage < 0 || stage > 6 {
			return errors.New("repertoire stage is out of range")
		}
	}
	for _, entry := range state.Practice {
		if len(entry.ID) == 0 || len(entry.ID) > 160 || !datePattern.MatchString(entry.Date) || math.IsNaN(entry.Minutes) || math.IsInf(entry.Minutes, 0) || entry.Minutes <= 0 || entry.Minutes > 360 || len(entry.Track) > 30 || len(entry.Note) > 100 {
			return errors.New("practice entry is invalid")
		}
	}
	return nil
}

func validateRecordingMedia(input recordingInitRequest) (string, string, string, error) {
	mediaKind := strings.ToLower(strings.TrimSpace(input.MediaKind))
	if mediaKind == "" {
		mediaKind = "audio"
	}
	audioType := strings.ToLower(strings.TrimSpace(strings.Split(input.ContentType, ";")[0]))
	if !allowedAudioType(audioType) || input.SizeBytes < 1 || input.SizeBytes > maxAudioBytes || input.DurationMS < 0 || input.DurationMS > maxDurationMS {
		return "", "", "", errors.New("recording type, size, or duration is not allowed")
	}
	if mediaKind == "audio" {
		if input.VideoSizeBytes != 0 || strings.TrimSpace(input.VideoContentType) != "" {
			return "", "", "", errors.New("audio recordings cannot include video metadata")
		}
		return mediaKind, audioType, "", nil
	}
	if mediaKind != "video" {
		return "", "", "", errors.New("recording media kind is not allowed")
	}
	videoType := strings.ToLower(strings.TrimSpace(strings.Split(input.VideoContentType, ";")[0]))
	if !allowedVideoType(videoType) || input.VideoSizeBytes < 1 || input.VideoSizeBytes > maxVideoBytes ||
		input.VideoWidth < 1 || input.VideoWidth > 7680 || input.VideoHeight < 1 || input.VideoHeight > 4320 ||
		input.VideoFrameRate < 1 || input.VideoFrameRate > 120 {
		return "", "", "", errors.New("video type, size, or dimensions are not allowed")
	}
	return mediaKind, audioType, videoType, nil
}

func normalizeWaveformPeaks(peaks []float64) ([]float64, error) {
	if len(peaks) > 1200 {
		return nil, errors.New("recording waveform is too large")
	}
	result := make([]float64, len(peaks))
	for index, peak := range peaks {
		if math.IsNaN(peak) || math.IsInf(peak, 0) || peak < 0 || peak > 1 {
			return nil, errors.New("recording waveform is invalid")
		}
		result[index] = math.Round(peak*10000) / 10000
	}
	return result, nil
}

func (app *application) initRecording(w http.ResponseWriter, r *http.Request) {
	var input recordingInitRequest
	if err := readJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	mediaKind, baseType, videoType, err := validateRecordingMedia(input)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	waveformPeaks, err := normalizeWaveformPeaks(input.WaveformPeaks)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	recordedAt, err := time.Parse(time.RFC3339, input.RecordedAt)
	if err != nil || len(input.Notes) > 500 || len(input.SkillIDs) > 20 || input.TakeNumber < 0 || input.TakeNumber > 99 {
		writeError(w, http.StatusUnprocessableEntity, "recording metadata is invalid")
		return
	}
	userID, err := app.userID(r.Context())
	if err != nil {
		app.serverError(w, err)
		return
	}
	recordingID := uuid.New()
	if input.PracticeSessionID != "" {
		sessionID, parseErr := uuid.Parse(input.PracticeSessionID)
		if parseErr != nil || !app.practiceSessionBelongs(r.Context(), userID, sessionID) {
			writeError(w, http.StatusUnprocessableEntity, "practice session is invalid")
			return
		}
	}
	var practiceBlockID *uuid.UUID
	if input.PracticeBlockID != "" {
		blockID, parseErr := uuid.Parse(input.PracticeBlockID)
		if parseErr != nil {
			writeError(w, http.StatusUnprocessableEntity, "practice block is invalid")
			return
		}
		var blockSessionID uuid.UUID
		var recordingCount int
		queryErr := app.db.QueryRow(r.Context(), `
			SELECT pb.session_id,(SELECT COUNT(*)::int FROM recordings r WHERE r.practice_block_id=pb.id AND r.status IN ('uploading','ready'))
			FROM practice_blocks pb WHERE pb.id=$1 AND pb.user_id=$2`, blockID, userID).Scan(&blockSessionID, &recordingCount)
		if errors.Is(queryErr, pgx.ErrNoRows) || (input.PracticeSessionID != "" && blockSessionID.String() != input.PracticeSessionID) {
			writeError(w, http.StatusUnprocessableEntity, "practice block is invalid")
			return
		}
		if queryErr != nil {
			app.serverError(w, queryErr)
			return
		}
		if recordingCount >= maxTakesPerBlock {
			writeError(w, http.StatusConflict, "this practice block already has twenty recordings")
			return
		}
		practiceBlockID = &blockID
	}
	objectName := fmt.Sprintf("users/%s/%s/%s/audio-master.%s", userID, recordedAt.UTC().Format("2006/01/02"), recordingID, extensionFor(baseType))
	videoObjectName := ""
	if mediaKind == "video" {
		videoObjectName = fmt.Sprintf("users/%s/%s/%s/video.%s", userID, recordedAt.UTC().Format("2006/01/02"), recordingID, extensionFor(videoType))
	}
	skillJSON, _ := json.Marshal(input.SkillIDs)
	waveformJSON, _ := json.Marshal(waveformPeaks)
	_, err = app.db.Exec(r.Context(), `
		INSERT INTO recordings
		(id,user_id,practice_session_id,practice_block_id,bucket,object_name,content_type,codec,expected_size_bytes,duration_ms,sample_rate,channels,recorded_at,status,tune_id,mission_id,skill_ids,take_number,notes,
		 media_kind,video_bucket,video_object_name,video_content_type,video_codec,video_expected_size_bytes,video_width,video_height,video_frame_rate,waveform_peaks)
		VALUES ($1,$2,NULLIF($3,''),$4,$5,$6,$7,NULLIF($8,''),$9,NULLIF($10,0),NULLIF($11,0),NULLIF($12,0),$13,'uploading',NULLIF($14,''),NULLIF($15,''),$16,NULLIF($17,0),NULLIF($18,''),
		 $19,CASE WHEN $19='video' THEN $5 ELSE NULL END,NULLIF($20,''),NULLIF($21,''),NULLIF($22,''),NULLIF($23,0),NULLIF($24,0),NULLIF($25,0),NULLIF($26,0),$27)`,
		recordingID, userID, clean(input.PracticeSessionID, 160), practiceBlockID, app.cfg.Bucket, objectName, baseType, clean(input.Codec, 80), input.SizeBytes,
		input.DurationMS, input.SampleRate, input.Channels, recordedAt, clean(input.TuneID, 100), clean(input.MissionID, 100), skillJSON,
		input.TakeNumber, clean(input.Notes, 500), mediaKind, videoObjectName, videoType, clean(input.VideoCodec, 120), input.VideoSizeBytes,
		input.VideoWidth, input.VideoHeight, input.VideoFrameRate, waveformJSON)
	if err != nil {
		app.serverError(w, err)
		return
	}
	uploadURL, err := app.createResumableUpload(r.Context(), recordingID, userID, objectName, baseType, input.SizeBytes, "audio", allowedUploadOrigin(r.Header.Get("Origin")))
	if err != nil {
		_, _ = app.db.Exec(r.Context(), `UPDATE recordings SET status='failed', updated_at=now() WHERE id=$1`, recordingID)
		app.serverError(w, err)
		return
	}
	response := map[string]any{"id": recordingID, "uploadUrl": uploadURL, "objectName": objectName}
	if mediaKind == "video" {
		videoUploadURL, videoErr := app.createResumableUpload(r.Context(), recordingID, userID, videoObjectName, videoType, input.VideoSizeBytes, "video", allowedUploadOrigin(r.Header.Get("Origin")))
		if videoErr != nil {
			_, _ = app.db.Exec(r.Context(), `UPDATE recordings SET status='failed', updated_at=now() WHERE id=$1`, recordingID)
			app.serverError(w, videoErr)
			return
		}
		response["videoUploadUrl"] = videoUploadURL
		response["videoObjectName"] = videoObjectName
	}
	writeJSON(w, http.StatusCreated, response)
}

func (app *application) createResumableUpload(ctx context.Context, recordingID, userID uuid.UUID, objectName, contentType string, size int64, assetKind, origin string) (string, error) {
	token, err := app.tokenSource.Token()
	if err != nil {
		return "", err
	}
	metadata := map[string]any{
		"name": objectName, "contentType": contentType,
		"metadata": map[string]string{"recordingId": recordingID.String(), "userId": userID.String(), "assetKind": assetKind},
	}
	body, _ := json.Marshal(metadata)
	endpoint := "https://storage.googleapis.com/upload/storage/v1/b/" + url.PathEscape(app.cfg.Bucket) + "/o?uploadType=resumable&name=" + url.QueryEscape(objectName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("X-Upload-Content-Type", contentType)
	req.Header.Set("X-Upload-Content-Length", strconv.FormatInt(size, 10))
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	response, err := app.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return "", fmt.Errorf("storage upload initialization failed: %s: %s", response.Status, strings.TrimSpace(string(message)))
	}
	location := response.Header.Get("Location")
	if location == "" {
		return "", errors.New("storage did not return an upload session")
	}
	return location, nil
}

func allowedUploadOrigin(origin string) string {
	origin = strings.TrimSpace(origin)
	switch origin {
	case "https://zachbednarke.com", "http://localhost:4173", "http://127.0.0.1:4173":
		return origin
	default:
		return ""
	}
}

func (app *application) completeRecording(w http.ResponseWriter, r *http.Request) {
	var input recordingCompleteRequest
	if err := readJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	asset := strings.ToLower(strings.TrimSpace(input.Asset))
	if asset == "" {
		asset = "audio"
	}
	if asset != "audio" && asset != "video" {
		writeError(w, http.StatusUnprocessableEntity, "recording asset is invalid")
		return
	}
	recordingID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid recording id")
		return
	}
	userID, err := app.userID(r.Context())
	if err != nil {
		app.serverError(w, err)
		return
	}
	var mediaKind, audioObjectName, videoObjectName, status string
	var audioExpectedSize, videoExpectedSize int64
	var audioUploaded, videoUploaded bool
	err = app.db.QueryRow(r.Context(), `
		SELECT media_kind,object_name,expected_size_bytes,COALESCE(video_object_name,''),COALESCE(video_expected_size_bytes,0),
		       uploaded_at IS NOT NULL,video_uploaded_at IS NOT NULL,status
		FROM recordings WHERE id=$1 AND user_id=$2 AND status IN ('uploading','ready')`, recordingID, userID).
		Scan(&mediaKind, &audioObjectName, &audioExpectedSize, &videoObjectName, &videoExpectedSize, &audioUploaded, &videoUploaded, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "recording not found")
		return
	}
	if err != nil {
		app.serverError(w, err)
		return
	}
	objectName, expectedSize := audioObjectName, audioExpectedSize
	if asset == "video" {
		if mediaKind != "video" || videoObjectName == "" || videoExpectedSize < 1 {
			writeError(w, http.StatusUnprocessableEntity, "recording has no video asset")
			return
		}
		objectName, expectedSize = videoObjectName, videoExpectedSize
	}
	if (asset == "audio" && audioUploaded) || (asset == "video" && videoUploaded) {
		writeJSON(w, http.StatusOK, map[string]any{"id": recordingID, "asset": asset, "status": status})
		return
	}
	attrs, err := app.storage.Bucket(app.cfg.Bucket).Object(objectName).Attrs(r.Context())
	if err != nil {
		app.serverError(w, err)
		return
	}
	if attrs.Size != expectedSize || attrs.Metadata["recordingId"] != recordingID.String() || attrs.Metadata["userId"] != userID.String() || attrs.Metadata["assetKind"] != asset {
		writeError(w, http.StatusConflict, "uploaded object did not pass verification")
		return
	}
	checksum := fmt.Sprintf("crc32c:%08x", attrs.CRC32C)
	if asset == "video" {
		_, err = app.db.Exec(r.Context(), `
			UPDATE recordings SET video_size_bytes=$1,video_object_generation=$2,video_checksum=$3,video_uploaded_at=now(),
			status=CASE WHEN uploaded_at IS NOT NULL THEN 'ready' ELSE 'uploading' END,updated_at=now()
			WHERE id=$4 AND user_id=$5`, attrs.Size, attrs.Generation, checksum, recordingID, userID)
	} else {
		_, err = app.db.Exec(r.Context(), `
			UPDATE recordings SET size_bytes=$1,object_generation=$2,checksum=$3,uploaded_at=now(),
			status=CASE WHEN media_kind='video' AND video_uploaded_at IS NULL THEN 'uploading' ELSE 'ready' END,updated_at=now()
			WHERE id=$4 AND user_id=$5`, attrs.Size, attrs.Generation, checksum, recordingID, userID)
	}
	if err != nil {
		app.serverError(w, err)
		return
	}
	if err := app.db.QueryRow(r.Context(), `SELECT status FROM recordings WHERE id=$1 AND user_id=$2`, recordingID, userID).Scan(&status); err != nil {
		app.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": recordingID, "asset": asset, "status": status})
}

func (app *application) listRecordings(w http.ResponseWriter, r *http.Request) {
	userID, err := app.userID(r.Context())
	if err != nil {
		app.serverError(w, err)
		return
	}
	rows, err := app.db.Query(r.Context(), `
		SELECT r.id,r.content_type,COALESCE(r.codec,''),COALESCE(r.size_bytes,r.expected_size_bytes),COALESCE(r.duration_ms,0),
		COALESCE(r.sample_rate,0),COALESCE(r.channels,0),r.recorded_at,r.status,
		COALESCE(r.tune_id,''),COALESCE(r.mission_id,''),r.skill_ids,COALESCE(r.take_number,0),COALESCE(r.notes,''),
		COALESCE(r.practice_session_id,''),COALESCE(ps.title,''),COALESCE(r.practice_block_id::text,''),
		COALESCE(pb.practice_date::text,''),COALESCE(pb.block_key,''),COALESCE(pb.title,''),COALESCE(pb.category,''),COALESCE(pb.track,''),r.object_name,
		COALESCE(r.media_kind,'audio'),COALESCE(r.video_content_type,''),COALESCE(r.video_codec,''),COALESCE(r.video_size_bytes,r.video_expected_size_bytes,0),
		COALESCE(r.video_width,0),COALESCE(r.video_height,0),COALESCE(r.video_frame_rate,0),COALESCE(r.video_object_name,''),r.waveform_peaks
		FROM recordings r
		LEFT JOIN practice_sessions ps ON ps.id::text = r.practice_session_id AND ps.user_id = r.user_id
		LEFT JOIN practice_blocks pb ON pb.id = r.practice_block_id AND pb.user_id = r.user_id
		WHERE r.user_id=$1 AND r.status <> 'deleted' ORDER BY r.recorded_at DESC LIMIT 100`, userID)
	if err != nil {
		app.serverError(w, err)
		return
	}
	defer rows.Close()
	result := make([]recordingRow, 0)
	for rows.Next() {
		var item recordingRow
		var skills []byte
		var waveform []byte
		if err := rows.Scan(&item.ID, &item.ContentType, &item.Codec, &item.SizeBytes, &item.DurationMS, &item.SampleRate, &item.Channels, &item.RecordedAt, &item.Status,
			&item.TuneID, &item.MissionID, &skills, &item.TakeNumber, &item.Notes, &item.SessionID, &item.SessionTitle, &item.BlockID,
			&item.BlockDate, &item.BlockKey, &item.BlockTitle, &item.BlockCategory, &item.BlockTrack, &item.ObjectName,
			&item.MediaKind, &item.VideoContentType, &item.VideoCodec, &item.VideoSizeBytes, &item.VideoWidth, &item.VideoHeight, &item.VideoFrameRate, &item.VideoObjectName, &waveform); err != nil {
			app.serverError(w, err)
			return
		}
		_ = json.Unmarshal(skills, &item.SkillIDs)
		_ = json.Unmarshal(waveform, &item.WaveformPeaks)
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		app.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"recordings": result})
}

func (app *application) recordingPlaybackURL(w http.ResponseWriter, r *http.Request) {
	recordingID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid recording id")
		return
	}
	userID, err := app.userID(r.Context())
	if err != nil {
		app.serverError(w, err)
		return
	}
	var mediaKind, audioObjectName, audioContentType, videoObjectName, videoContentType, blockTitle string
	var durationMS int
	var takeNumber int
	var recordedAt time.Time
	err = app.db.QueryRow(r.Context(), `
		SELECT r.media_kind,r.object_name,r.content_type,COALESCE(r.video_object_name,''),COALESCE(r.video_content_type,''),
		       COALESCE(r.duration_ms,0),COALESCE(pb.title,''),COALESCE(r.take_number,0),r.recorded_at
		FROM recordings r
		LEFT JOIN practice_blocks pb ON pb.id=r.practice_block_id AND pb.user_id=r.user_id
		WHERE r.id=$1 AND r.user_id=$2 AND r.status='ready'`, recordingID, userID).
		Scan(&mediaKind, &audioObjectName, &audioContentType, &videoObjectName, &videoContentType, &durationMS, &blockTitle, &takeNumber, &recordedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "recording not found")
		return
	}
	if err != nil {
		app.serverError(w, err)
		return
	}
	asset := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("asset")))
	if asset == "" {
		asset = mediaKind
	}
	objectName, contentType := audioObjectName, audioContentType
	if asset == "video" && mediaKind == "video" && videoObjectName != "" {
		objectName, contentType = videoObjectName, videoContentType
		asset = "video"
	} else if asset == "audio" {
		asset = "audio"
	} else {
		writeError(w, http.StatusUnprocessableEntity, "recording asset is invalid")
		return
	}
	download := r.URL.Query().Get("download") == "1"
	expires := time.Now().Add(10 * time.Minute)
	filename := ""
	var query url.Values
	if download {
		expires = time.Now().Add(time.Hour)
		filename = recordingDownloadFilename(recordedAt, blockTitle, takeNumber, asset, contentType)
		query = url.Values{
			"response-content-disposition": {`attachment; filename="` + filename + `"`},
		}
	}
	signedURL, err := app.signedRecordingObjectURL(r.Context(), objectName, expires, query)
	if err != nil {
		app.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"url": signedURL, "expiresAt": expires, "asset": asset, "contentType": contentType, "durationMs": durationMS, "filename": filename})
}

func (app *application) signedRecordingObjectURL(ctx context.Context, objectName string, expires time.Time, query url.Values) (string, error) {
	return storage.SignedURL(app.cfg.Bucket, objectName, &storage.SignedURLOptions{
		GoogleAccessID:  app.cfg.ServiceAccountEmail,
		Method:          http.MethodGet,
		Expires:         expires,
		Scheme:          storage.SigningSchemeV4,
		QueryParameters: query,
		SignBytes: func(payload []byte) ([]byte, error) {
			response, err := app.iamSigner.SignBlob(ctx, &credentialspb.SignBlobRequest{
				Name: "projects/-/serviceAccounts/" + app.cfg.ServiceAccountEmail, Payload: payload,
			})
			if err != nil {
				return nil, err
			}
			return response.SignedBlob, nil
		},
	})
}

func normalizeShareAsset(requested, mediaKind string) (string, error) {
	asset := strings.ToLower(strings.TrimSpace(requested))
	if asset == "" {
		asset = mediaKind
	}
	if asset == "audio" || (asset == "video" && mediaKind == "video") {
		return asset, nil
	}
	return "", errors.New("recording asset is invalid")
}

func newPublicShareToken() (string, error) {
	contents := make([]byte, 24)
	if _, err := rand.Read(contents); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(contents), nil
}

func (app *application) recordingShareURL(w http.ResponseWriter, r *http.Request) {
	recordingID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid recording id")
		return
	}
	userID, err := app.userID(r.Context())
	if err != nil {
		app.serverError(w, err)
		return
	}
	var mediaKind string
	err = app.db.QueryRow(r.Context(), `SELECT media_kind FROM recordings WHERE id=$1 AND user_id=$2 AND status='ready'`, recordingID, userID).Scan(&mediaKind)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "recording not found")
		return
	}
	if err != nil {
		app.serverError(w, err)
		return
	}
	asset, err := normalizeShareAsset(r.URL.Query().Get("asset"), mediaKind)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	token, err := newPublicShareToken()
	if err != nil {
		app.serverError(w, err)
		return
	}
	err = app.db.QueryRow(r.Context(), `
		INSERT INTO recording_shares (id,user_id,recording_id,asset,token)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (recording_id,asset) WHERE revoked_at IS NULL
		DO UPDATE SET recording_id=EXCLUDED.recording_id
		RETURNING token`, uuid.New(), userID, recordingID, asset, token).Scan(&token)
	if err != nil {
		app.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"url":   app.cfg.PublicShareBaseURL + "/" + token,
		"asset": asset,
	})
}

func (app *application) publicRecordingShare(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.PathValue("token"))
	if !publicShareTokenPattern.MatchString(token) {
		http.NotFound(w, r)
		return
	}
	var asset, mediaKind, audioObjectName, videoObjectName string
	err := app.db.QueryRow(r.Context(), `
		SELECT s.asset,r.media_kind,r.object_name,COALESCE(r.video_object_name,'')
		FROM recording_shares s
		JOIN recordings r ON r.id=s.recording_id AND r.user_id=s.user_id
		WHERE s.token=$1 AND s.revoked_at IS NULL AND r.status='ready'`, token).
		Scan(&asset, &mediaKind, &audioObjectName, &videoObjectName)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		app.serverError(w, err)
		return
	}
	objectName := audioObjectName
	if asset == "video" && mediaKind == "video" && videoObjectName != "" {
		objectName = videoObjectName
	} else if asset != "audio" {
		http.NotFound(w, r)
		return
	}
	signedURL, err := app.signedRecordingObjectURL(r.Context(), objectName, time.Now().Add(15*time.Minute), nil)
	if err != nil {
		app.serverError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow, noarchive")
	http.Redirect(w, r, signedURL, http.StatusTemporaryRedirect)
}

func recordingDownloadFilename(recordedAt time.Time, blockTitle string, takeNumber int, asset, contentType string) string {
	part := strings.Trim(downloadFilenamePartPattern.ReplaceAllString(strings.ToLower(strings.TrimSpace(blockTitle)), "-"), "-")
	if part == "" {
		part = "practice"
	}
	parts := []string{recordedAt.Format("2006-01-02"), part}
	if takeNumber > 0 {
		parts = append(parts, "take-"+strconv.Itoa(takeNumber))
	}
	if asset == "video" {
		parts = append(parts, "video")
	} else {
		parts = append(parts, "audio")
	}
	return strings.Join(parts, "-") + "." + extensionFor(contentType)
}

func (app *application) updateRecording(w http.ResponseWriter, r *http.Request) {
	recordingID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid recording id")
		return
	}
	var input recordingUpdateRequest
	if err := readJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	notes, err := normalizeRecordingNote(input.Notes)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	userID, err := app.userID(r.Context())
	if err != nil {
		app.serverError(w, err)
		return
	}
	var updatedAt time.Time
	err = app.db.QueryRow(r.Context(), `
		UPDATE recordings SET notes=NULLIF($1,''),updated_at=now()
		WHERE id=$2 AND user_id=$3 AND status <> 'deleted'
		RETURNING updated_at`, notes, recordingID, userID).Scan(&updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "recording not found")
		return
	}
	if err != nil {
		app.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": recordingID, "notes": notes, "updatedAt": updatedAt})
}

func normalizeRecordingNote(input *string) (string, error) {
	if input == nil {
		return "", errors.New("recording notes are required")
	}
	notes := strings.TrimSpace(*input)
	if len(notes) > maxTakeNoteBytes {
		return "", errors.New("recording notes are too long")
	}
	return notes, nil
}

func (app *application) deleteRecording(w http.ResponseWriter, r *http.Request) {
	recordingID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid recording id")
		return
	}
	userID, err := app.userID(r.Context())
	if err != nil {
		app.serverError(w, err)
		return
	}
	var objectName, videoObjectName string
	err = app.db.QueryRow(r.Context(), `SELECT object_name,COALESCE(video_object_name,'') FROM recordings WHERE id=$1 AND user_id=$2 AND status <> 'deleted'`, recordingID, userID).
		Scan(&objectName, &videoObjectName)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "recording not found")
		return
	}
	if err != nil {
		app.serverError(w, err)
		return
	}
	for _, assetObjectName := range []string{objectName, videoObjectName} {
		if assetObjectName == "" {
			continue
		}
		if err := app.storage.Bucket(app.cfg.Bucket).Object(assetObjectName).Delete(r.Context()); err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
			app.serverError(w, err)
			return
		}
	}
	_, err = app.db.Exec(r.Context(), `UPDATE recordings SET status='deleted', updated_at=now() WHERE id=$1 AND user_id=$2`, recordingID, userID)
	if err != nil {
		app.serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func allowedAudioType(contentType string) bool {
	switch contentType {
	case "audio/webm", "audio/mp4", "audio/ogg", "audio/wav", "audio/x-wav", "audio/mpeg":
		return true
	default:
		return false
	}
}

func allowedVideoType(contentType string) bool {
	switch contentType {
	case "video/webm", "video/mp4":
		return true
	default:
		return false
	}
}

func extensionFor(contentType string) string {
	switch contentType {
	case "audio/mp4":
		return "m4a"
	case "audio/ogg":
		return "ogg"
	case "audio/wav", "audio/x-wav":
		return "wav"
	case "audio/mpeg":
		return "mp3"
	case "video/mp4":
		return "mp4"
	default:
		return "webm"
	}
}

func clean(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) > limit {
		return value[:limit]
	}
	return value
}

func readJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return errors.New("invalid JSON request")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request must contain one JSON object")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func (app *application) serverError(w http.ResponseWriter, err error) {
	app.logger.Error("request failed", "error", err)
	writeError(w, http.StatusInternalServerError, "internal server error")
}

func (app *application) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				app.logger.Error("panic", "value", recovered)
				writeError(w, http.StatusInternalServerError, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (app *application) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		app.logger.Info("request", "method", r.Method, "path", r.URL.Path, "duration_ms", time.Since(started).Milliseconds())
	})
}
