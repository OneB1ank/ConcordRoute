-- 听写转录独立准入，已有分组保持关闭。
SET LOCAL lock_timeout = '5s';
ALTER TABLE groups ADD COLUMN IF NOT EXISTS allow_audio_transcription BOOLEAN NOT NULL DEFAULT FALSE;
