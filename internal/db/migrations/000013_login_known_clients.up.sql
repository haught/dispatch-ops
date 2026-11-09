-- Client addresses each username has signed in from with its password, so
-- failures from elsewhere can't push the owner over the username's total
-- limit: that limit doesn't apply to a client known here. Keyed like
-- login_failures. Rows older than the retention period (90 days) no longer
-- count and are pruned.

CREATE TABLE login_known_clients (
    -- SHA-256 of the lowercased username (see login_failures).
    username_hash bytea NOT NULL,
    -- The client key: an IPv4 address or an IPv6 /64 (see LoginClientKey).
    client_ip     text NOT NULL,
    signed_in_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (username_hash, client_ip)
);
CREATE INDEX login_known_clients_signed_in_at_idx ON login_known_clients (signed_in_at);
