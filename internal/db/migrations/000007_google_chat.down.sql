DELETE FROM notification_targets WHERE kind = 'googlechat';
ALTER TABLE notification_targets
    DROP CONSTRAINT notification_targets_kind_check,
    ADD CONSTRAINT notification_targets_kind_check
        CHECK (kind IN ('webhook', 'slack', 'email'));
