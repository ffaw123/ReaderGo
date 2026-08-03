-- 启用向量检索和由数据库生成 UUID 所需的 PostgreSQL 扩展。
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- 视频与任务使用受限状态集合，避免写入无法识别的生命周期状态。
CREATE TYPE video_status AS ENUM ('uploading','processing','paused','ready','failed');
CREATE TYPE job_status AS ENUM ('pending','running','succeeded','failed');

-- users 是第一版的登录身份与租户边界。
CREATE TABLE users (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(), 
  email text NOT NULL UNIQUE,
  password_hash text NOT NULL, 
  display_name text NOT NULL DEFAULT '', 
  created_at timestamptz NOT NULL DEFAULT now(), 
  updated_at timestamptz NOT NULL DEFAULT now()
);
-- videos 保存用户提交的视频源及处理结果元数据。
CREATE TABLE videos (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(), 
  tenant_id uuid NOT NULL REFERENCES users(id), 
  source_url text NOT NULL,
  title text NOT NULL DEFAULT '', 
  status video_status NOT NULL DEFAULT 'uploading',
  upload_method text NOT NULL DEFAULT 'url' CHECK (upload_method IN ('local_direct','url')),
  paused_from_status text CHECK (paused_from_status IS NULL OR paused_from_status IN ('uploading','processing')),
  duration_ms bigint NOT NULL DEFAULT 0,
  object_key text NOT NULL DEFAULT '', 
  error_message text NOT NULL DEFAULT '', 
  created_at timestamptz NOT NULL DEFAULT now(), 
  updated_at timestamptz NOT NULL DEFAULT now()
);
-- 常用列表查询按租户与创建时间过滤。
CREATE INDEX videos_tenant_id_idx ON videos(tenant_id, created_at DESC);
-- processing_jobs 记录可重试的异步处理任务。
CREATE TABLE processing_jobs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(), 
  tenant_id uuid NOT NULL REFERENCES users(id), 
  video_id uuid NOT NULL REFERENCES videos(id) ON DELETE CASCADE,
  task_type text NOT NULL CHECK (task_type IN ('prepare','asr','vision','rag_index','summary')),
  attempt integer NOT NULL DEFAULT 1, 
  status job_status NOT NULL DEFAULT 'pending', 
  error_message text NOT NULL DEFAULT '', 
  lease_owner text NOT NULL DEFAULT '',
  lease_expires_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(), 
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX processing_jobs_video_task_idx ON processing_jobs(video_id, task_type);
-- message_outbox 与任务状态在同一事务落库，再由独立 Dispatcher 可靠发布到 RabbitMQ。
CREATE TABLE message_outbox (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES users(id),
  video_id uuid NOT NULL REFERENCES videos(id) ON DELETE CASCADE,
  job_id uuid NOT NULL REFERENCES processing_jobs(id) ON DELETE CASCADE,
  task_type text NOT NULL CHECK (task_type IN ('prepare','asr','vision','rag_index','summary')),
  attempt integer NOT NULL,
  exchange_name text NOT NULL,
  routing_key text NOT NULL,
  payload jsonb NOT NULL,
  status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','publishing','published')),
  available_at timestamptz NOT NULL DEFAULT now(),
  publish_attempts integer NOT NULL DEFAULT 0,
  last_error text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  published_at timestamptz
);
CREATE INDEX message_outbox_pending_idx ON message_outbox(status, available_at, created_at);
-- 每个视频只有一个最终摘要。
CREATE TABLE video_summaries (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(), 
  video_id uuid NOT NULL UNIQUE REFERENCES videos(id) ON DELETE CASCADE, 
  content text NOT NULL, 
  created_at timestamptz NOT NULL DEFAULT now()
);
-- video_segments 是带时间戳的 RAG 最小证据单元，融合语音转写与画面描述。
CREATE TABLE video_segments (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(), 
  tenant_id uuid NOT NULL REFERENCES users(id), 
  video_id uuid NOT NULL REFERENCES videos(id) ON DELETE CASCADE, 
  start_ms bigint NOT NULL, 
  end_ms bigint NOT NULL, 
  transcript text NOT NULL DEFAULT '', 
  visual_caption text NOT NULL DEFAULT '', 
  retrieval_text text NOT NULL DEFAULT '', 
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX video_segments_video_time_idx ON video_segments(video_id,start_ms);
-- segment_embeddings 保存第一版融合文本的 1536 维向量。
CREATE TABLE segment_embeddings (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(), 
  tenant_id uuid NOT NULL REFERENCES users(id), 
  video_id uuid NOT NULL REFERENCES videos(id) ON DELETE CASCADE, 
  segment_id uuid NOT NULL UNIQUE REFERENCES video_segments(id) ON DELETE CASCADE, 
  embedding_model text NOT NULL, 
  embedding vector(1536), 
  created_at timestamptz NOT NULL DEFAULT now()
);
-- IVFFlat 索引加速余弦相似度向量检索；数据量较小时也保留正确性。
CREATE INDEX segment_embeddings_vector_idx ON segment_embeddings USING ivfflat (embedding vector_cosine_ops) WITH (lists = 100);
-- 对话按视频和租户归属，禁止跨用户问答。
CREATE TABLE conversations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(), 
  tenant_id uuid NOT NULL REFERENCES users(id), 
  video_id uuid NOT NULL REFERENCES videos(id) ON DELETE CASCADE, 
  title text NOT NULL DEFAULT '', 
  created_at timestamptz NOT NULL DEFAULT now(), 
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE messages (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(), 
  conversation_id uuid NOT NULL REFERENCES conversations(id) ON DELETE CASCADE, 
  role text NOT NULL CHECK (role IN ('user','assistant','system')), 
  content text NOT NULL, 
  citations jsonb NOT NULL DEFAULT '[]', 
  created_at timestamptz NOT NULL DEFAULT now()
);
