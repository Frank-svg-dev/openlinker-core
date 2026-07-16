BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '0';

SELECT pg_advisory_xact_lock(hashtextextended('openlinker.runtime.migration.075', 0));

LOCK TABLE runtime_session_attachments IN ACCESS EXCLUSIVE MODE;
LOCK TABLE runtime_sessions IN ACCESS EXCLUSIVE MODE;
LOCK TABLE runtime_nodes IN ACCESS EXCLUSIVE MODE;
LOCK TABLE runs IN SHARE MODE;
LOCK TABLE runtime_schema_contracts IN ACCESS EXCLUSIVE MODE;
LOCK TABLE runtime_wire_contracts IN ACCESS EXCLUSIVE MODE;
LOCK TABLE runtime_cluster_control IN SHARE MODE;
LOCK TABLE runtime_cluster_members IN ACCESS EXCLUSIVE MODE;

DO $$
DECLARE
    previous_digest CONSTANT TEXT := 'fb92bb6ddbc65bd3353b5d7c63ad148dd510e4d0ac0a6ca6110461d91e2dec53';
    current_digest CONSTANT TEXT := '3f84df167bbe211efdc6362ad5ec876aeedf881cbfb9677606982af63c7423e9';
BEGIN
    IF EXISTS (SELECT 1 FROM runtime_cluster_members) THEN
        RAISE EXCEPTION 'migration 075 rollback requires zero registered Core cluster members';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM runtime_cluster_control
        WHERE singleton_id = 1 AND mode = 'hard_maintenance'
    ) THEN
        RAISE EXCEPTION 'migration 075 rollback requires hard maintenance';
    END IF;
    IF EXISTS (SELECT 1 FROM runs WHERE status = 'running') THEN
        RAISE EXCEPTION 'migration 075 rollback requires zero running Runs';
    END IF;
    IF (
        SELECT COUNT(*) FROM runtime_schema_contracts
        WHERE schema_version = 75
          AND migration_name = '075_runtime_wire_compatibility'
          AND runtime_contract_id = 'openlinker.runtime.v2'
          AND runtime_contract_digest = current_digest
          AND is_current
    ) <> 1 OR (SELECT COUNT(*) FROM runtime_schema_contracts WHERE is_current) <> 1 THEN
        RAISE EXCEPTION 'migration 075 rollback requires the exact current schema contract 75';
    END IF;
    IF (
        SELECT COUNT(*) FROM runtime_schema_contracts
        WHERE schema_version = 73
          AND migration_name = '073_runtime_transport_observability'
          AND runtime_contract_id = 'openlinker.runtime.v2'
          AND runtime_contract_digest = current_digest
          AND NOT is_current
    ) <> 1 THEN
        RAISE EXCEPTION 'migration 075 rollback requires the exact historical schema contract 73';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'runtime_wire_contracts'
          AND column_name = 'support_tier'
    ) THEN
        RAISE EXCEPTION 'migration 075 rollback requires Runtime wire support tiers';
    END IF;
    IF EXISTS (
        SELECT 1 FROM runtime_nodes
        WHERE status IN ('active', 'draining')
          AND runtime_contract_digest <> current_digest
    ) OR EXISTS (
        SELECT 1 FROM runtime_sessions
        WHERE status IN ('active', 'draining')
          AND runtime_contract_digest <> current_digest
    ) THEN
        RAISE EXCEPTION 'migration 075 rollback refuses live previous-generation principals';
    END IF;
    IF (
        SELECT COUNT(*) FROM runtime_wire_contracts
        WHERE runtime_contract_id = 'openlinker.runtime.v2'
          AND runtime_contract_digest = current_digest
          AND support_tier = 'current'
    ) <> 1 OR (
        SELECT COUNT(*) FROM runtime_wire_contracts
        WHERE runtime_contract_id = 'openlinker.runtime.v2'
          AND runtime_contract_digest = previous_digest
          AND support_tier = 'previous'
    ) <> 1 THEN
        RAISE EXCEPTION 'migration 075 rollback found mismatched Runtime wire support tiers';
    END IF;
END
$$;

ALTER TABLE runtime_sessions DROP CONSTRAINT runtime_sessions_contract_current;
ALTER TABLE runtime_nodes DROP CONSTRAINT runtime_nodes_contract_current;

UPDATE runtime_schema_contracts
SET is_current = FALSE
WHERE schema_version = 75
  AND migration_name = '075_runtime_wire_compatibility'
  AND runtime_contract_id = 'openlinker.runtime.v2'
  AND runtime_contract_digest = '3f84df167bbe211efdc6362ad5ec876aeedf881cbfb9677606982af63c7423e9'
  AND is_current;

DELETE FROM runtime_schema_contracts
WHERE schema_version = 75
  AND migration_name = '075_runtime_wire_compatibility'
  AND runtime_contract_id = 'openlinker.runtime.v2'
  AND runtime_contract_digest = '3f84df167bbe211efdc6362ad5ec876aeedf881cbfb9677606982af63c7423e9'
  AND NOT is_current;

UPDATE runtime_schema_contracts
SET is_current = TRUE
WHERE schema_version = 73
  AND migration_name = '073_runtime_transport_observability'
  AND runtime_contract_id = 'openlinker.runtime.v2'
  AND runtime_contract_digest = '3f84df167bbe211efdc6362ad5ec876aeedf881cbfb9677606982af63c7423e9'
  AND NOT is_current;

ALTER TABLE runtime_nodes
    ADD CONSTRAINT runtime_nodes_contract_current
        CHECK (
            protocol_version = 2
            AND runtime_contract_id = 'openlinker.runtime.v2'
            AND (
                (status IN ('active', 'draining')
                 AND runtime_contract_digest = '3f84df167bbe211efdc6362ad5ec876aeedf881cbfb9677606982af63c7423e9')
                OR
                (status = 'revoked'
                 AND runtime_contract_digest IN (
                    '60bef5cec7eeab563937187f48a458059995aebee161765032cddc17d0cdfa61',
                    '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f',
                    'fb92bb6ddbc65bd3353b5d7c63ad148dd510e4d0ac0a6ca6110461d91e2dec53',
                    '3f84df167bbe211efdc6362ad5ec876aeedf881cbfb9677606982af63c7423e9'
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
                 AND runtime_contract_digest = '3f84df167bbe211efdc6362ad5ec876aeedf881cbfb9677606982af63c7423e9')
                OR
                (status IN ('offline', 'revoked', 'closed')
                 AND runtime_contract_digest IN (
                    '60bef5cec7eeab563937187f48a458059995aebee161765032cddc17d0cdfa61',
                    '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f',
                    'fb92bb6ddbc65bd3353b5d7c63ad148dd510e4d0ac0a6ca6110461d91e2dec53',
                    '3f84df167bbe211efdc6362ad5ec876aeedf881cbfb9677606982af63c7423e9'
                 ))
            )
        );

DROP INDEX idx_runtime_wire_contracts_previous;
DROP INDEX idx_runtime_wire_contracts_current;
ALTER TABLE runtime_wire_contracts
    DROP CONSTRAINT runtime_wire_contracts_support_identity,
    DROP CONSTRAINT runtime_wire_contracts_support_tier_valid,
    DROP COLUMN support_tier;

COMMIT;
