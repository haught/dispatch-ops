DELETE FROM schedules WHERE inventory_id IS NOT NULL;
ALTER TABLE schedules
    DROP CONSTRAINT schedules_inventory_id_name_key,
    DROP CONSTRAINT schedules_one_target,
    DROP COLUMN inventory_id,
    ALTER COLUMN job_template_id SET NOT NULL;

ALTER TABLE jobs DROP COLUMN sync_inventory_id;

DROP TABLE inventory_source_credentials;
DROP TABLE inventory_sources;
