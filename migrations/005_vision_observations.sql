-- 保存按需画面理解的结构化结果；原始图片不进入数据库。
CREATE TABLE vision_observations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES users(id),
  video_id uuid NOT NULL REFERENCES videos(id) ON DELETE CASCADE,
  conversation_id uuid NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  user_message_id uuid NOT NULL UNIQUE REFERENCES messages(id) ON DELETE CASCADE,
  start_ms bigint NOT NULL,
  end_ms bigint NOT NULL,
  status text NOT NULL CHECK (status IN ('grounded', 'no_evidence')),
  observation_text text NOT NULL DEFAULT '',
  sampled_frame_timestamps jsonb NOT NULL DEFAULT '[]'::jsonb,
  used_frame_timestamps jsonb NOT NULL DEFAULT '[]'::jsonb,
  tool_calls integer NOT NULL DEFAULT 0,
  model text NOT NULL,
  prompt_version text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK (start_ms >= 0 AND end_ms > start_ms),
  CHECK (end_ms - start_ms <= 30000),
  CHECK (
    (status = 'grounded' AND length(btrim(observation_text)) > 0)
    OR
    (status = 'no_evidence' AND length(btrim(observation_text)) = 0)
  )
);

-- 加速同一会话的最近记忆读取。
CREATE INDEX vision_observations_conversation_created_idx
ON vision_observations(conversation_id, created_at DESC);

-- 加速按视频和时间重叠查询成功观察。
CREATE INDEX vision_observations_video_time_idx
ON vision_observations(tenant_id, video_id, start_ms, end_ms);
