BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '0';

SELECT pg_advisory_xact_lock(hashtextextended('openlinker.runtime.migration.073', 0));

LOCK TABLE runtime_session_attachments IN ACCESS EXCLUSIVE MODE;
LOCK TABLE runtime_sessions IN ACCESS EXCLUSIVE MODE;
LOCK TABLE runtime_nodes IN ACCESS EXCLUSIVE MODE;
LOCK TABLE runs IN SHARE MODE;
LOCK TABLE runtime_schema_contracts IN ACCESS EXCLUSIVE MODE;
LOCK TABLE runtime_cluster_control IN SHARE MODE;
LOCK TABLE runtime_cluster_members IN ACCESS EXCLUSIVE MODE;

DO $$
DECLARE
    oldest_digest CONSTANT TEXT := '60bef5cec7eeab563937187f48a458059995aebee161765032cddc17d0cdfa61';
    entry_digest CONSTANT TEXT := '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f';
    previous_digest CONSTANT TEXT := 'fb92bb6ddbc65bd3353b5d7c63ad148dd510e4d0ac0a6ca6110461d91e2dec53';
    current_digest CONSTANT TEXT := '3f84df167bbe211efdc6362ad5ec876aeedf881cbfb9677606982af63c7423e9';
BEGIN
    IF EXISTS (SELECT 1 FROM runtime_cluster_members) THEN
        RAISE EXCEPTION 'migration 073 rollback requires zero registered Core cluster members';
    END IF;
    IF NOT EXISTS (
        SELECT 1
        FROM runtime_cluster_control
        WHERE singleton_id = 1
          AND mode = 'hard_maintenance'
    ) THEN
        RAISE EXCEPTION 'migration 073 rollback requires hard maintenance';
    END IF;
    IF EXISTS (SELECT 1 FROM runs WHERE status = 'running') THEN
        RAISE EXCEPTION 'migration 073 rollback requires zero running Runs';
    END IF;
    IF (
        SELECT COUNT(*)
        FROM runtime_schema_contracts
        WHERE schema_version = 73
          AND migration_name = '073_runtime_transport_observability'
          AND runtime_contract_id = 'openlinker.runtime.v2'
          AND runtime_contract_digest = current_digest
          AND is_current
    ) <> 1 OR (SELECT COUNT(*) FROM runtime_schema_contracts WHERE is_current) <> 1 THEN
        RAISE EXCEPTION 'migration 073 rollback requires the exact current schema contract 73';
    END IF;
    IF (
        SELECT COUNT(*)
        FROM runtime_schema_contracts
        WHERE schema_version = 71
          AND migration_name = '071_runtime_attachment_generation'
          AND runtime_contract_id = 'openlinker.runtime.v2'
          AND runtime_contract_digest = current_digest
          AND NOT is_current
    ) <> 1 THEN
        RAISE EXCEPTION 'migration 073 rollback requires the exact historical schema contract 71';
    END IF;
    IF EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'runtime_schema_contracts'::regclass
          AND conname = 'runtime_schema_contracts_runtime_pair_unique'
    ) THEN
        RAISE EXCEPTION 'migration 073 rollback requires schema-to-wire pair uniqueness to be absent';
    END IF;
    IF to_regclass('runtime_wire_contracts') IS NULL THEN
        RAISE EXCEPTION 'migration 073 rollback requires the Runtime wire contract registry';
    END IF;
    IF EXISTS (
        SELECT 1
        FROM runtime_wire_contracts wire
        WHERE NOT EXISTS (
            SELECT 1
            FROM runtime_schema_contracts schema_contract
            WHERE schema_contract.runtime_contract_id = wire.runtime_contract_id
              AND schema_contract.runtime_contract_digest = wire.runtime_contract_digest
        )
    ) THEN
        RAISE EXCEPTION 'migration 073 rollback refuses an unreferenced Runtime wire contract';
    END IF;
    IF EXISTS (
        SELECT 1
        FROM runtime_nodes
        WHERE runtime_contract_id <> 'openlinker.runtime.v2'
           OR runtime_contract_digest NOT IN (
                oldest_digest,
                entry_digest,
                previous_digest,
                current_digest
           )
    ) OR EXISTS (
        SELECT 1
        FROM runtime_sessions
        WHERE runtime_contract_id <> 'openlinker.runtime.v2'
           OR runtime_contract_digest NOT IN (
                oldest_digest,
                entry_digest,
                previous_digest,
                current_digest
           )
    ) THEN
        RAISE EXCEPTION 'migration 073 rollback found an unknown Runtime wire contract identity';
    END IF;
END
$$;

-- The schema-73 binary is the only process that can interpret the transport
-- columns. Close its live Attachment generation before removing that evidence.
UPDATE runtime_session_attachments
SET detached_at = clock_timestamp(),
    disconnect_reason = 'runtime transport observability rollback'
WHERE detached_at IS NULL;

UPDATE runtime_sessions
SET status = 'offline',
    attached_core_instance_id = NULL,
    disconnected_at = COALESCE(disconnected_at, clock_timestamp()),
    heartbeat_at = GREATEST(heartbeat_at, clock_timestamp()),
    updated_at = clock_timestamp()
WHERE status IN ('active', 'draining');

SET CONSTRAINTS ALL IMMEDIATE;
SET CONSTRAINTS ALL DEFERRED;

CREATE OR REPLACE FUNCTION enforce_runtime_session_attachment_history()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'runtime session attachment history cannot be deleted';
    END IF;

    IF ROW(
        NEW.id,
        NEW.runtime_session_id,
        NEW.core_instance_id,
        NEW.attachment_kind,
        NEW.attached_at
    ) IS DISTINCT FROM ROW(
        OLD.id,
        OLD.runtime_session_id,
        OLD.core_instance_id,
        OLD.attachment_kind,
        OLD.attached_at
    ) THEN
        RAISE EXCEPTION 'runtime session attachment identity is immutable';
    END IF;

    IF OLD.detached_at IS NOT NULL
       AND ROW(
           NEW.detached_at,
           NEW.disconnect_reason
       ) IS DISTINCT FROM ROW(
           OLD.detached_at,
           OLD.disconnect_reason
       ) THEN
        RAISE EXCEPTION 'detached runtime session attachment is immutable';
    END IF;

    RETURN NEW;
END
$$ LANGUAGE plpgsql;

ALTER TABLE runtime_session_attachments
    DROP CONSTRAINT runtime_session_attachments_transport_time_order,
    DROP CONSTRAINT runtime_session_attachments_live_transport_known,
    DROP CONSTRAINT runtime_session_attachments_transport_reason_valid,
    DROP CONSTRAINT runtime_session_attachments_transport_valid,
    DROP COLUMN transport_changed_at,
    DROP COLUMN transport_reason,
    DROP COLUMN transport;

ALTER TABLE runtime_session_attachments
    ALTER COLUMN attached_at SET DEFAULT clock_timestamp();

ALTER TABLE runtime_sessions DROP CONSTRAINT runtime_sessions_contract_current;
ALTER TABLE runtime_nodes DROP CONSTRAINT runtime_nodes_contract_current;
ALTER TABLE runtime_sessions DROP CONSTRAINT runtime_sessions_contract_fk;
ALTER TABLE runtime_nodes DROP CONSTRAINT runtime_nodes_contract_fk;
ALTER TABLE runtime_schema_contracts DROP CONSTRAINT runtime_schema_contracts_wire_fk;

UPDATE runtime_schema_contracts
SET is_current = FALSE
WHERE schema_version = 73
  AND migration_name = '073_runtime_transport_observability'
  AND runtime_contract_id = 'openlinker.runtime.v2'
  AND runtime_contract_digest = '3f84df167bbe211efdc6362ad5ec876aeedf881cbfb9677606982af63c7423e9'
  AND is_current;

DELETE FROM runtime_schema_contracts
WHERE schema_version = 73
  AND migration_name = '073_runtime_transport_observability'
  AND runtime_contract_id = 'openlinker.runtime.v2'
  AND runtime_contract_digest = '3f84df167bbe211efdc6362ad5ec876aeedf881cbfb9677606982af63c7423e9'
  AND NOT is_current;

UPDATE runtime_schema_contracts
SET is_current = TRUE
WHERE schema_version = 71
  AND migration_name = '071_runtime_attachment_generation'
  AND runtime_contract_id = 'openlinker.runtime.v2'
  AND runtime_contract_digest = '3f84df167bbe211efdc6362ad5ec876aeedf881cbfb9677606982af63c7423e9'
  AND NOT is_current;

ALTER TABLE runtime_schema_contracts
    ADD CONSTRAINT runtime_schema_contracts_runtime_pair_unique
        UNIQUE (runtime_contract_id, runtime_contract_digest);

ALTER TABLE runtime_nodes
    ADD CONSTRAINT runtime_nodes_contract_fk
        FOREIGN KEY (runtime_contract_id, runtime_contract_digest)
        REFERENCES runtime_schema_contracts (
            runtime_contract_id,
            runtime_contract_digest
        )
        ON DELETE RESTRICT;

ALTER TABLE runtime_sessions
    ADD CONSTRAINT runtime_sessions_contract_fk
        FOREIGN KEY (runtime_contract_id, runtime_contract_digest)
        REFERENCES runtime_schema_contracts (
            runtime_contract_id,
            runtime_contract_digest
        )
        ON DELETE RESTRICT;

DROP TABLE runtime_wire_contracts;

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

COMMIT;
