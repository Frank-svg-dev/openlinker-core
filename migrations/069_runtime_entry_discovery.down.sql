BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '0';

SELECT pg_advisory_xact_lock(hashtextextended('openlinker.runtime.v2.migration.069', 0));

LOCK TABLE runtime_session_attachments IN ACCESS EXCLUSIVE MODE;
LOCK TABLE runtime_sessions IN ACCESS EXCLUSIVE MODE;
LOCK TABLE runtime_nodes IN ACCESS EXCLUSIVE MODE;
LOCK TABLE runs IN SHARE MODE;
LOCK TABLE runtime_schema_contracts IN ACCESS EXCLUSIVE MODE;
LOCK TABLE runtime_cluster_control IN SHARE MODE;
LOCK TABLE runtime_cluster_members IN ACCESS EXCLUSIVE MODE;

DO $$
DECLARE
    old_digest CONSTANT TEXT := '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f';
    new_digest CONSTANT TEXT := '052ed16553eeb896bc7a88dabd1ada77466a4db0c87b55c997c6b91ab72a72de';
BEGIN
    IF EXISTS (SELECT 1 FROM runtime_cluster_members) THEN
        RAISE EXCEPTION 'migration 069 rollback requires zero registered Core cluster members';
    END IF;
    IF NOT EXISTS (
        SELECT 1
        FROM runtime_cluster_control
        WHERE singleton_id = 1
          AND mode = 'hard_maintenance'
    ) THEN
        RAISE EXCEPTION 'migration 069 rollback requires hard maintenance';
    END IF;
    IF EXISTS (SELECT 1 FROM runs WHERE status = 'running') THEN
        RAISE EXCEPTION 'migration 069 rollback requires zero running Runs';
    END IF;
    IF (
        SELECT COUNT(*)
        FROM runtime_schema_contracts
        WHERE schema_version = 69
          AND migration_name = '069_runtime_entry_discovery'
          AND runtime_contract_id = 'openlinker.runtime.v2'
          AND runtime_contract_digest = new_digest
          AND is_current
    ) <> 1 OR (SELECT COUNT(*) FROM runtime_schema_contracts WHERE is_current) <> 1 THEN
        RAISE EXCEPTION 'migration 069 rollback requires the exact current schema contract 69';
    END IF;
    IF (
        SELECT COUNT(*)
        FROM runtime_schema_contracts
        WHERE schema_version = 67
          AND migration_name = '067_runtime_v2_core_execution'
          AND runtime_contract_id = 'openlinker.runtime.v2'
          AND runtime_contract_digest = old_digest
          AND NOT is_current
    ) <> 1 THEN
        RAISE EXCEPTION 'migration 069 rollback requires the exact historical schema contract 67';
    END IF;
END
$$;

UPDATE runtime_session_attachments attachment
SET detached_at = clock_timestamp(),
    disconnect_reason = 'runtime entry contract rollback'
FROM runtime_sessions session
WHERE session.runtime_session_id = attachment.runtime_session_id
  AND session.runtime_contract_digest = '052ed16553eeb896bc7a88dabd1ada77466a4db0c87b55c997c6b91ab72a72de'
  AND session.status IN ('active', 'draining')
  AND attachment.detached_at IS NULL;

UPDATE runtime_sessions
SET status = 'closed',
    attached_core_instance_id = NULL,
    disconnected_at = COALESCE(disconnected_at, clock_timestamp()),
    heartbeat_at = GREATEST(heartbeat_at, clock_timestamp()),
    updated_at = clock_timestamp()
WHERE runtime_contract_digest = '052ed16553eeb896bc7a88dabd1ada77466a4db0c87b55c997c6b91ab72a72de'
  AND status IN ('active', 'draining');

SET CONSTRAINTS ALL IMMEDIATE;
SET CONSTRAINTS ALL DEFERRED;

ALTER TABLE runtime_sessions DROP CONSTRAINT runtime_sessions_contract_current;
ALTER TABLE runtime_nodes DROP CONSTRAINT runtime_nodes_contract_current;

UPDATE runtime_schema_contracts
SET is_current = FALSE
WHERE schema_version = 69
  AND migration_name = '069_runtime_entry_discovery'
  AND runtime_contract_id = 'openlinker.runtime.v2'
  AND runtime_contract_digest = '052ed16553eeb896bc7a88dabd1ada77466a4db0c87b55c997c6b91ab72a72de'
  AND is_current;

UPDATE runtime_schema_contracts
SET is_current = TRUE
WHERE schema_version = 67
  AND migration_name = '067_runtime_v2_core_execution'
  AND runtime_contract_id = 'openlinker.runtime.v2'
  AND runtime_contract_digest = '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f'
  AND NOT is_current;

UPDATE runtime_nodes
SET runtime_contract_digest = '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f',
    updated_at = clock_timestamp()
WHERE status <> 'revoked'
  AND runtime_contract_digest = '052ed16553eeb896bc7a88dabd1ada77466a4db0c87b55c997c6b91ab72a72de';

SET CONSTRAINTS ALL IMMEDIATE;
SET CONSTRAINTS ALL DEFERRED;

ALTER TABLE runtime_nodes
    ADD CONSTRAINT runtime_nodes_contract_current
        CHECK (
            protocol_version = 2
            AND runtime_contract_id = 'openlinker.runtime.v2'
            AND (
                (status IN ('active', 'draining')
                 AND runtime_contract_digest = '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f')
                OR
                (status = 'revoked'
                 AND runtime_contract_digest IN (
                    '60bef5cec7eeab563937187f48a458059995aebee161765032cddc17d0cdfa61',
                    '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f',
                    '052ed16553eeb896bc7a88dabd1ada77466a4db0c87b55c997c6b91ab72a72de'
                 ))
            )
        );

ALTER TABLE runtime_sessions
    ADD CONSTRAINT runtime_sessions_contract_current
        CHECK (
            protocol_version = 2
            AND runtime_contract_id = 'openlinker.runtime.v2'
            AND (
                (status IN ('active', 'draining')
                 AND runtime_contract_digest = '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f')
                OR
                (status IN ('offline', 'revoked', 'closed')
                 AND runtime_contract_digest IN (
                    '60bef5cec7eeab563937187f48a458059995aebee161765032cddc17d0cdfa61',
                    '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f',
                    '052ed16553eeb896bc7a88dabd1ada77466a4db0c87b55c997c6b91ab72a72de'
                 ))
            )
        );

COMMIT;
