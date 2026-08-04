-- ocr_segments 保存抽样画面的 OCR 原始结果，供 RAG 与 Summary 阶段按时间轴读取。
-- Qwen-VL-OCR 不返回置信度；空文本由 Worker 直接丢弃，本表只保存可用 OCR 证据。
CREATE TABLE ocr_segments (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES users(id),
  video_id uuid NOT NULL REFERENCES videos(id) ON DELETE CASCADE,
  start_ms bigint NOT NULL CHECK (start_ms >= 0),
  end_ms bigint NOT NULL CHECK (end_ms > start_ms),
  text text NOT NULL CONSTRAINT ocr_segments_text_nonempty_check CHECK (btrim(text) <> ''),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (video_id, start_ms, end_ms)
);
CREATE INDEX ocr_segments_video_time_idx ON ocr_segments(video_id, start_ms);
