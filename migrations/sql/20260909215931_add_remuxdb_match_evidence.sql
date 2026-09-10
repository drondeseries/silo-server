-- +goose Up
CREATE TABLE IF NOT EXISTS public.remuxdb_match_evidence (
    content_id         text NOT NULL,
    episode_id         text NOT NULL DEFAULT '',
    media_folder_id    integer NOT NULL DEFAULT 0,
    candidate_uri      text NOT NULL DEFAULT '',
    match_method       text NOT NULL DEFAULT '',
    matched_size       bigint NOT NULL DEFAULT 0,
    matched_content_hash text NOT NULL DEFAULT '',
    container          text NOT NULL DEFAULT '',
    codec_video        text NOT NULL DEFAULT '',
    codec_audio        text NOT NULL DEFAULT '',
    resolution         text NOT NULL DEFAULT '',
    hdr                boolean NOT NULL DEFAULT false,
    hdr_known          boolean NOT NULL DEFAULT false,
    duration           double precision NOT NULL DEFAULT 0,
    bitrate            bigint NOT NULL DEFAULT 0,
    video_tracks       jsonb NOT NULL DEFAULT '[]'::jsonb,
    audio_tracks       jsonb NOT NULL DEFAULT '[]'::jsonb,
    subtitle_tracks    jsonb NOT NULL DEFAULT '[]'::jsonb,
    matched_at         timestamp with time zone NOT NULL DEFAULT now(),
    CONSTRAINT remuxdb_match_evidence_pkey PRIMARY KEY (content_id, episode_id, media_folder_id, candidate_uri)
);
CREATE INDEX IF NOT EXISTS idx_remuxdb_match_evidence_content
    ON public.remuxdb_match_evidence (content_id, episode_id, media_folder_id);

-- +goose Down
DROP TABLE IF EXISTS public.remuxdb_match_evidence;
