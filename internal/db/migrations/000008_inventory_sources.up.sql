-- Inventories synced from an inventory file in a project.

-- An inventory with a source is synced from it: a sync job runs
-- ansible-inventory on the file and replaces the inventory's hosts and
-- groups with the result. Its hosts and groups can't be edited by hand.
CREATE TABLE inventory_sources (
    inventory_id              uuid PRIMARY KEY REFERENCES inventories (id) ON DELETE CASCADE,
    project_id                uuid NOT NULL REFERENCES projects (id) ON DELETE RESTRICT,
    -- The inventory file (or directory), relative to the project.
    source_path               text NOT NULL CHECK (source_path <> ''),
    execution_environment_id  uuid NOT NULL REFERENCES execution_environments (id) ON DELETE RESTRICT,
    timeout_seconds           integer NOT NULL DEFAULT 600 CHECK (timeout_seconds > 0),
    verbosity                 integer NOT NULL DEFAULT 0 CHECK (verbosity BETWEEN 0 AND 5),
    -- The last sync job, and when a sync last replaced the inventory.
    last_job_id               uuid REFERENCES jobs (id) ON DELETE SET NULL,
    last_synced_at            timestamptz,
    created_at                timestamptz NOT NULL DEFAULT now(),
    updated_at                timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER inventory_sources_set_updated_at BEFORE UPDATE ON inventory_sources
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE INDEX inventory_sources_project_idx ON inventory_sources (project_id);

CREATE TABLE inventory_source_credentials (
    inventory_id   uuid NOT NULL REFERENCES inventory_sources (inventory_id) ON DELETE CASCADE,
    credential_id  uuid NOT NULL REFERENCES credentials (id) ON DELETE RESTRICT,
    PRIMARY KEY (inventory_id, credential_id)
);
CREATE INDEX inventory_source_credentials_credential_idx ON inventory_source_credentials (credential_id);

-- The inventory a sync job syncs. Jobs of other types have none (their
-- inventory, if any, is in config).
ALTER TABLE jobs ADD COLUMN sync_inventory_id uuid REFERENCES inventories (id) ON DELETE SET NULL;
-- At most one unfinished sync per inventory.
CREATE UNIQUE INDEX jobs_one_active_sync_idx ON jobs (sync_inventory_id)
    WHERE sync_inventory_id IS NOT NULL AND status IN ('pending', 'waiting', 'running');
CREATE INDEX jobs_sync_inventory_idx ON jobs (sync_inventory_id) WHERE sync_inventory_id IS NOT NULL;

-- A schedule launches either a job template or an inventory sync.
ALTER TABLE schedules
    ALTER COLUMN job_template_id DROP NOT NULL,
    ADD COLUMN inventory_id uuid REFERENCES inventories (id) ON DELETE CASCADE,
    ADD CONSTRAINT schedules_one_target CHECK (num_nonnulls(job_template_id, inventory_id) = 1),
    ADD CONSTRAINT schedules_inventory_id_name_key UNIQUE (inventory_id, name);
