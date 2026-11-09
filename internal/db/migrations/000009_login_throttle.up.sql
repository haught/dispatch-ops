-- Failed password sign-ins, for throttling by account and by client address
-- across API replicas. Rows older than the throttle window are pruned.

CREATE TABLE login_failures (
    id            bigserial PRIMARY KEY,
    -- SHA-256 of the lowercased username as typed, so a password typed into
    -- the username field is never stored, and unknown usernames count too.
    username_hash bytea NOT NULL,
    -- The client address the attempt came from (see DISPATCH_TRUSTED_PROXIES).
    client_ip     text NOT NULL,
    failed_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX login_failures_username_idx ON login_failures (username_hash, failed_at);
CREATE INDEX login_failures_client_ip_idx ON login_failures (client_ip, failed_at);
CREATE INDEX login_failures_failed_at_idx ON login_failures (failed_at);
