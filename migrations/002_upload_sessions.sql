CREATE TABLE upload_sessions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES users(id),
  video_id uuid NOT NULL REFERENCES videos(id) ON DELETE CASCADE,
  upload_id text NOT NULL,
  object_key text NOT NULL,
  filename text NOT NULL CHECK (length(filename) BETWEEN 1 AND 512),
  file_last_modified_ms bigint NOT NULL CHECK (file_last_modified_ms > 0),
  file_fingerprint text NOT NULL CHECK (file_fingerprint ~ '^[0-9a-f]{64}$'),
  part_size bigint NOT NULL CHECK (part_size >= 16777216 AND part_size <= 67108864),
  file_size bigint NOT NULL CHECK (file_size > 0),
  part_count integer NOT NULL CHECK (part_count BETWEEN 1 AND 10000),
  status text NOT NULL CHECK (status IN ('uploading', 'completed', 'cancelled', 'expired')),
  expires_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX upload_sessions_tenant_id_idx ON upload_sessions(tenant_id, created_at DESC);
