CREATE TABLE IF NOT EXISTS clip_day_analyses (
    user_id UUID NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    practice_date DATE NOT NULL,
    analysis_version TEXT NOT NULL,
    recording_count INTEGER NOT NULL DEFAULT 0 CHECK (recording_count >= 0),
    recording_fingerprint TEXT NOT NULL DEFAULT '',
    analyzed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, practice_date)
);

INSERT INTO clip_day_analyses (user_id, practice_date, analysis_version, recording_count, analyzed_at)
SELECT r.user_id,
       COALESCE(pb.practice_date,(r.recorded_at AT TIME ZONE 'America/Los_Angeles')::date),
       'waveform-v1',
       COUNT(DISTINCT r.id)::int,
       MAX(c.updated_at)
FROM clip_candidates c
JOIN recordings r ON r.id=c.recording_id AND r.status='ready'
LEFT JOIN practice_blocks pb ON pb.id=r.practice_block_id AND pb.user_id=r.user_id
GROUP BY r.user_id,COALESCE(pb.practice_date,(r.recorded_at AT TIME ZONE 'America/Los_Angeles')::date)
ON CONFLICT (user_id,practice_date) DO NOTHING;
