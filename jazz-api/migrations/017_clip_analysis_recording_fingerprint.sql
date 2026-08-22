ALTER TABLE clip_day_analyses
    ADD COLUMN IF NOT EXISTS recording_fingerprint TEXT NOT NULL DEFAULT '';
