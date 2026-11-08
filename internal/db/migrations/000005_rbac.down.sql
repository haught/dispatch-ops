DROP TABLE audit_log;

ALTER TABLE auth_tokens DROP COLUMN scope;

ALTER TABLE users DROP CONSTRAINT users_oidc_issuer_check;
DROP INDEX users_oidc_subject_key;
ALTER TABLE users DROP COLUMN oidc_issuer;
-- Fails if two issuers used the same subject; delete one of those users first.
CREATE UNIQUE INDEX users_oidc_subject_key ON users (oidc_subject) WHERE oidc_subject IS NOT NULL;

-- Only direct system_admin grants survive; team grants are lost.
ALTER TABLE users ADD COLUMN is_admin boolean NOT NULL DEFAULT false;
UPDATE users SET is_admin = true
WHERE id IN (SELECT user_id FROM role_grants WHERE role = 'system_admin' AND resource_type IS NULL AND user_id IS NOT NULL);

DROP TRIGGER job_templates_delete_role_grants ON job_templates;
DROP TRIGGER projects_delete_role_grants ON projects;
DROP TRIGGER credentials_delete_role_grants ON credentials;
DROP TRIGGER inventories_delete_role_grants ON inventories;
DROP FUNCTION delete_role_grants();
DROP VIEW user_roles;
DROP TABLE role_grants;
DROP TABLE team_members;
DROP TABLE teams;
