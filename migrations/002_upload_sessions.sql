ALTER TYPE video_status ADD VALUE IF NOT EXISTS 'uploading';
ALTER TYPE video_status ADD VALUE IF NOT EXISTS 'uploaded';

CREATE TABLE upload_sessions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES users(id),
  video_id uuid NOT NULL REFERENCES videos(id) ON DELETE CASCADE,
  upload_id text NOT NULL,
  object_key text NOT NULL,
  part_size bigint NOT NULL CHECK (part_size >= 16777216 AND part_size <= 67108864),
  file_size bigint NOT NULL CHECK (file_size > 0),
  part_count integer NOT NULL CHECK (part_count BETWEEN 1 AND 10000),
  status text NOT NULL CHECK (status IN ('uploading', 'completed', 'cancelled', 'expired')),
  expires_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX upload_sessions_tenant_id_idx ON upload_sessions(tenant_id, created_at DESC);
