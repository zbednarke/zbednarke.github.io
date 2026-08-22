package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/cloudsqlconn"
	"cloud.google.com/go/storage"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
)

type recording struct {
	ID          uuid.UUID
	Bucket      string
	ObjectName  string
	ContentType string
	DurationMS  int
	StoredSize  int64
	StoredGen   int64
	RecordedAt  time.Time
}

func main() {
	var recordingID string
	var apply bool
	var all bool
	var ffmpegPath string
	var userSubject string
	flag.StringVar(&recordingID, "id", "", "repair one recording UUID")
	flag.BoolVar(&all, "all", false, "repair every unoptimized ready video")
	flag.BoolVar(&apply, "apply", os.Getenv("APPLY") == "1", "write backups and repaired playback objects")
	flag.StringVar(&ffmpegPath, "ffmpeg", "ffmpeg", "path to ffmpeg")
	flag.StringVar(&userSubject, "user", "zach", "app user auth subject")
	flag.Parse()

	if all == (recordingID != "") {
		log.Fatal("choose exactly one of -all or -id")
	}
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	if _, err := exec.LookPath(ffmpegPath); err != nil {
		log.Fatalf("ffmpeg not found: %v", err)
	}

	ctx := context.Background()
	var tokenSource oauth2.TokenSource
	if _, err := exec.LookPath("gcloud"); err == nil {
		tokenSource = oauth2.ReuseTokenSource(nil, gcloudTokenSource{})
	}
	db, closeDatabase, err := openDatabase(ctx, databaseURL, tokenSource)
	if err != nil {
		log.Fatal(err)
	}
	defer closeDatabase()
	if err := db.Ping(ctx); err != nil {
		log.Fatalf("database unavailable: %v", err)
	}
	var client *storage.Client
	if tokenSource != nil {
		client, err = storage.NewClient(ctx, option.WithTokenSource(tokenSource))
	} else {
		client, err = storage.NewClient(ctx)
	}
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	recordings, err := loadRecordings(ctx, db, userSubject, recordingID)
	if err != nil {
		log.Fatal(err)
	}
	var total int64
	for _, item := range recordings {
		total += item.StoredSize
	}
	log.Printf("found %d unoptimized ready video(s), %.2f GiB total", len(recordings), float64(total)/(1<<30))
	if !apply {
		log.Print("dry run only; pass -apply to create permanent backups and repair playback objects")
		return
	}

	for index, item := range recordings {
		log.Printf("[%d/%d] %s (%s, %.1f MiB)", index+1, len(recordings), item.ID, item.RecordedAt.Format(time.RFC3339), float64(item.StoredSize)/(1<<20))
		if err := repair(ctx, db, client, ffmpegPath, item); err != nil {
			log.Fatalf("repair %s failed: %v", item.ID, err)
		}
	}
	log.Printf("repaired %d video(s)", len(recordings))
}

type gcloudTokenSource struct{}

func (gcloudTokenSource) Token() (*oauth2.Token, error) {
	command := exec.Command("gcloud", "auth", "print-access-token")
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("load gcloud access token: %w", err)
	}
	value := strings.TrimSpace(string(output))
	if value == "" {
		return nil, errors.New("gcloud returned an empty access token")
	}
	return &oauth2.Token{AccessToken: value, TokenType: "Bearer", Expiry: time.Now().Add(45 * time.Minute)}, nil
}

func openDatabase(ctx context.Context, databaseURL string, tokenSource oauth2.TokenSource) (*pgxpool.Pool, func(), error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, func() {}, err
	}
	host := strings.ReplaceAll(config.ConnConfig.Host, "\\", "/")
	const prefix = "/cloudsql/"
	if !strings.HasPrefix(host, prefix) {
		pool, err := pgxpool.NewWithConfig(ctx, config)
		return pool, func() { pool.Close() }, err
	}
	instance := strings.TrimPrefix(host, prefix)
	if separator := strings.IndexByte(instance, '/'); separator >= 0 {
		instance = instance[:separator]
	}
	if strings.Count(instance, ":") != 2 {
		return nil, func() {}, fmt.Errorf("invalid Cloud SQL instance in database host")
	}
	var dialer *cloudsqlconn.Dialer
	if tokenSource != nil {
		dialer, err = cloudsqlconn.NewDialer(ctx, cloudsqlconn.WithTokenSource(tokenSource))
	} else {
		dialer, err = cloudsqlconn.NewDialer(ctx)
	}
	if err != nil {
		return nil, func() {}, err
	}
	config.ConnConfig.DialFunc = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.Dial(ctx, instance)
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		_ = dialer.Close()
		return nil, func() {}, err
	}
	return pool, func() {
		pool.Close()
		_ = dialer.Close()
	}, nil
}

func loadRecordings(ctx context.Context, db *pgxpool.Pool, userSubject, recordingID string) ([]recording, error) {
	query := `
		SELECT r.id,r.bucket,r.video_object_name,COALESCE(r.video_content_type,'video/webm'),COALESCE(r.duration_ms,0),
		       COALESCE(r.video_size_bytes,r.video_expected_size_bytes,0),COALESCE(r.video_object_generation,0),r.recorded_at
		FROM recordings r JOIN app_users u ON u.id=r.user_id
		WHERE u.auth_subject=$1 AND r.media_kind='video' AND r.status='ready' AND r.video_object_name IS NOT NULL
		  AND NOT COALESCE(r.video_playback_optimized,false)`
	args := []any{userSubject}
	if recordingID != "" {
		id, err := uuid.Parse(recordingID)
		if err != nil {
			return nil, fmt.Errorf("invalid recording id: %w", err)
		}
		query += " AND r.id=$2"
		args = append(args, id)
	}
	query += " ORDER BY recorded_at,id"
	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []recording{}
	for rows.Next() {
		var item recording
		if err := rows.Scan(&item.ID, &item.Bucket, &item.ObjectName, &item.ContentType, &item.DurationMS, &item.StoredSize, &item.StoredGen, &item.RecordedAt); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func repair(ctx context.Context, db *pgxpool.Pool, client *storage.Client, ffmpegPath string, item recording) error {
	object := client.Bucket(item.Bucket).Object(item.ObjectName)
	attrs, err := object.Attrs(ctx)
	if err != nil {
		return fmt.Errorf("read source metadata: %w", err)
	}
	if attrs.Metadata["seekablePlayback"] == "true" {
		log.Print("already marked seekable; refreshing cache and database metadata")
		updated, updateErr := ensurePlaybackCache(ctx, object, attrs)
		if updateErr != nil {
			return updateErr
		}
		return updateDatabase(ctx, db, item, updated)
	}

	tempDir, err := os.MkdirTemp("", "jazz-video-index-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)
	extension := path.Ext(item.ObjectName)
	if extension == "" {
		extension = ".video"
	}
	inputPath := filepath.Join(tempDir, "input"+extension)
	outputPath := filepath.Join(tempDir, "seekable"+extension)
	if err := download(ctx, object, inputPath); err != nil {
		return fmt.Errorf("download source: %w", err)
	}
	sourceCodecs, codecsOK := probeCodecs(inputPath)
	if !codecsOK {
		return errors.New("could not identify source video codecs")
	}
	if isWebM(item.ContentType) {
		if duration, ok := probeDuration(inputPath); ok {
			log.Printf("source is already seekable (%.3fs); preserving bytes", duration)
			metadata := cloneMetadata(attrs.Metadata)
			metadata["seekablePlayback"] = "true"
			updated, err := object.If(storage.Conditions{MetagenerationMatch: attrs.Metageneration}).Update(ctx, storage.ObjectAttrsToUpdate{Metadata: metadata, CacheControl: playbackCacheControl})
			if err != nil {
				return fmt.Errorf("mark seekable source: %w", err)
			}
			return updateDatabase(ctx, db, item, updated)
		}
	}

	backupName := backupObjectName(item.ObjectName)
	backup := client.Bucket(item.Bucket).Object(backupName)
	if _, err := backup.Attrs(ctx); errors.Is(err, storage.ErrObjectNotExist) {
		copier := backup.If(storage.Conditions{DoesNotExist: true}).CopierFrom(object.Generation(attrs.Generation))
		copier.ContentType = attrs.ContentType
		copier.Metadata = cloneMetadata(attrs.Metadata)
		copier.Metadata["backupPurpose"] = "original-unindexed-video"
		copier.Metadata["sourceGeneration"] = strconv.FormatInt(attrs.Generation, 10)
		if _, err := copier.Run(ctx); err != nil {
			return fmt.Errorf("create original backup %s: %w", backupName, err)
		}
		log.Printf("backed up original to gs://%s/%s", item.Bucket, backupName)
	} else if err != nil {
		return fmt.Errorf("inspect original backup: %w", err)
	} else {
		log.Printf("original backup already exists at gs://%s/%s", item.Bucket, backupName)
	}

	command := exec.CommandContext(ctx, ffmpegPath, remuxArgs(item.ContentType, inputPath, outputPath)...)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg remux: %w: %s", err, strings.TrimSpace(string(output)))
	}
	duration, ok := probeDuration(outputPath)
	if !ok {
		return errors.New("repaired video still has no finite duration")
	}
	repairedCodecs, codecsOK := probeCodecs(outputPath)
	if !codecsOK || repairedCodecs != sourceCodecs {
		return fmt.Errorf("repaired codecs %q do not match source codecs %q", repairedCodecs, sourceCodecs)
	}
	inputInfo, err := os.Stat(inputPath)
	if err != nil {
		return fmt.Errorf("inspect source file: %w", err)
	}
	outputInfo, err := os.Stat(outputPath)
	if err != nil {
		return fmt.Errorf("inspect repaired file: %w", err)
	}
	if !repairedSizeIsSafe(inputInfo.Size(), outputInfo.Size()) {
		return fmt.Errorf("repaired size %d differs too much from source size %d", outputInfo.Size(), inputInfo.Size())
	}
	if item.DurationMS > 0 {
		storedDuration := float64(item.DurationMS) / 1000
		if abs(duration-storedDuration) > 5 {
			log.Printf("captured media timeline %.3fs differs from browser clock %.3fs; preserving packet timeline", duration, storedDuration)
		}
	}

	metadata := cloneMetadata(attrs.Metadata)
	metadata["seekablePlayback"] = "true"
	metadata["originalGeneration"] = strconv.FormatInt(attrs.Generation, 10)
	if isMP4(item.ContentType) {
		metadata["fastStart"] = "true"
	}
	newAttrs, err := upload(ctx, object.If(storage.Conditions{GenerationMatch: attrs.Generation}), outputPath, attrs.ContentType, metadata)
	if err != nil {
		return fmt.Errorf("upload repaired object: %w", err)
	}
	if err := updateDatabase(ctx, db, item, newAttrs); err != nil {
		return err
	}
	log.Printf("seekable playback ready (%.3fs, %.1f MiB)", duration, float64(newAttrs.Size)/(1<<20))
	return nil
}

func download(ctx context.Context, object *storage.ObjectHandle, destination string) error {
	reader, err := object.NewReader(ctx)
	if err != nil {
		return err
	}
	defer reader.Close()
	file, err := os.Create(destination)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, reader)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func upload(ctx context.Context, object *storage.ObjectHandle, source, contentType string, metadata map[string]string) (*storage.ObjectAttrs, error) {
	file, err := os.Open(source)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	writer := object.NewWriter(ctx)
	writer.ContentType = contentType
	writer.CacheControl = playbackCacheControl
	writer.Metadata = metadata
	writer.ChunkSize = 16 << 20
	if _, err := io.Copy(writer, file); err != nil {
		_ = writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return writer.Attrs(), nil
}

func probeDuration(filename string) (float64, bool) {
	command := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", filename)
	output, err := command.Output()
	if err != nil {
		return 0, false
	}
	duration, err := strconv.ParseFloat(strings.TrimSpace(string(output)), 64)
	return duration, err == nil && duration > 0
}

func probeCodecs(filename string) (string, bool) {
	command := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type,codec_name", "-of", "csv=p=0", filename)
	output, err := command.Output()
	value := strings.TrimSpace(string(output))
	return value, err == nil && value != ""
}

func updateDatabase(ctx context.Context, db *pgxpool.Pool, item recording, attrs *storage.ObjectAttrs) error {
	checksum := fmt.Sprintf("crc32c:%08x", attrs.CRC32C)
	result, err := db.Exec(ctx, `
		UPDATE recordings
		SET video_size_bytes=$1,video_object_generation=$2,video_checksum=$3,video_playback_optimized=true,updated_at=now()
		WHERE id=$4 AND video_object_name=$5`, attrs.Size, attrs.Generation, checksum, item.ID, item.ObjectName)
	if err != nil {
		return fmt.Errorf("update database metadata: %w", err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("recording changed before database metadata update")
	}
	return nil
}

const playbackCacheControl = "private, max-age=600"

func isMP4(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(contentType), "video/mp4")
}

func isWebM(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(contentType), "video/webm")
}

func remuxArgs(contentType, inputPath, outputPath string) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-i", inputPath, "-map", "0", "-c", "copy"}
	if isMP4(contentType) {
		return append(args, "-movflags", "+faststart", outputPath)
	}
	return append(args, "-reserve_index_space", "1048576", "-cues_to_front", "1", outputPath)
}

func ensurePlaybackCache(ctx context.Context, object *storage.ObjectHandle, attrs *storage.ObjectAttrs) (*storage.ObjectAttrs, error) {
	if attrs.CacheControl == playbackCacheControl {
		return attrs, nil
	}
	updated, err := object.If(storage.Conditions{MetagenerationMatch: attrs.Metageneration}).Update(ctx, storage.ObjectAttrsToUpdate{CacheControl: playbackCacheControl})
	if err != nil {
		return nil, fmt.Errorf("set playback cache metadata: %w", err)
	}
	return updated, nil
}

func backupObjectName(objectName string) string {
	extension := path.Ext(objectName)
	return strings.TrimSuffix(objectName, extension) + ".original-unindexed" + extension
}

func cloneMetadata(source map[string]string) map[string]string {
	result := make(map[string]string, len(source)+2)
	for key, value := range source {
		result[key] = value
	}
	return result
}

func abs(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

func repairedSizeIsSafe(sourceSize, repairedSize int64) bool {
	tolerance := sourceSize / 100
	if tolerance < 4<<20 {
		tolerance = 4 << 20
	}
	return absInt64(repairedSize-sourceSize) <= tolerance
}
