-- An administrator can add a single sign-on user before they first sign in.
-- Until then the user is pending: auth_source is 'oidc' and the issuer and
-- subject are NULL. The first sign-in with the same username sets both.
ALTER TABLE users DROP CONSTRAINT users_check1; -- auth_source <> 'oidc' OR oidc_subject IS NOT NULL
ALTER TABLE users DROP CONSTRAINT users_oidc_issuer_check;
ALTER TABLE users ADD CONSTRAINT users_oidc_identity_check
    CHECK ((oidc_issuer IS NULL) = (oidc_subject IS NULL));
