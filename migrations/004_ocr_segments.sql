-- ocr_segments 保存抽样画面的 OCR 原始结果，供 RAG 与 Summary 阶段按时间轴读取。
-- 空 text 不代表视频内容，必须结合 status 区分“无文字”和“OCR 服务不可用”。
CREATE TABLE ocr_segments (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES users(id),
  video_id uuid NOT NULL REFERENCES videos(id) ON DELETE CASCADE,
  start_ms bigint NOT NULL CHECK (start_ms >= 0),
  end_ms bigint NOT NULL CHECK (end_ms > start_ms),
  text text NOT NULL DEFAULT '',
  confidence double precision NOT NULL DEFAULT 0 CHECK (confidence >= 0 AND confidence <= 1),
  status text NOT NULL CHECK (status IN ('succeeded', 'no_text', 'service_unavailable')),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (video_id, start_ms, end_ms)
);
CREATE INDEX ocr_segments_video_time_idx ON ocr_segments(video_id, start_ms);
