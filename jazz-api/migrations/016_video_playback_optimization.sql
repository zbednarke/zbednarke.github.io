ALTER TABLE recordings
    ADD COLUMN IF NOT EXISTS video_playback_optimized BOOLEAN NOT NULL DEFAULT false;
