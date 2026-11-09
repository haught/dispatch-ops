-- Notes that a sign-in limit has started refusing attempts, so the audit log
-- records the first refusal in a throttle window rather than every one.
-- Rows past `until` are pruned.

CREATE TABLE login_lockouts (
    -- SHA-256 of the limit's name and what it counts (username hash, client
    -- address, or both).
    key   bytea PRIMARY KEY,
    until timestamptz NOT NULL
);
CREATE INDEX login_lockouts_until_idx ON login_lockouts (until);
