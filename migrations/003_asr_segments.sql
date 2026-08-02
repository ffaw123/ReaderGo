-- asr_segments 保存 ASR 原始输出；RAG 消费者读取它和未来的 vision_segments 后再生成 video_segments。
CREATE TABLE asr_segments (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES users(id),
  video_id uuid NOT NULL REFERENCES videos(id) ON DELETE CASCADE,
  start_ms bigint NOT NULL CHECK (start_ms >= 0),
  end_ms bigint NOT NULL CHECK (end_ms > start_ms),
  transcript text NOT NULL CHECK (transcript <> ''),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (video_id, start_ms, end_ms, transcript)
);
CREATE INDEX asr_segments_video_time_idx ON asr_segments(video_id, start_ms);
