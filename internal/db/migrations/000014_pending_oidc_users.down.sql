-- Pending single sign-on users can't exist without this migration.
DELETE FROM users WHERE auth_source = 'oidc' AND oidc_subject IS NULL;
ALTER TABLE users DROP CONSTRAINT users_oidc_identity_check;
ALTER TABLE users ADD CONSTRAINT users_check1 CHECK (auth_source <> 'oidc' OR oidc_subject IS NOT NULL);
ALTER TABLE users ADD CONSTRAINT users_oidc_issuer_check CHECK (auth_source <> 'oidc' OR oidc_issuer IS NOT NULL);
