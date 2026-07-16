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
        RAISE EXCEPTION 'migration 073 requires zero registered Core cluster members';
    END IF;
    IF NOT EXISTS (
        SELECT 1
        FROM runtime_cluster_control
        WHERE singleton_id = 1
          AND mode = 'hard_maintenance'
    ) THEN
        RAISE EXCEPTION 'migration 073 requires hard maintenance';
    END IF;
    IF EXISTS (SELECT 1 FROM runs WHERE status = 'running') THEN
        RAISE EXCEPTION 'migration 073 requires zero running Runs';
    END IF;
    IF (
        SELECT COUNT(*)
        FROM runtime_schema_contracts
        WHERE schema_version = 71
          AND migration_name = '071_runtime_attachment_generation'
          AND runtime_contract_id = 'openlinker.runtime.v2'
          AND runtime_contract_digest = current_digest
          AND is_current
    ) <> 1 OR (SELECT COUNT(*) FROM runtime_schema_contracts WHERE is_current) <> 1 THEN
        RAISE EXCEPTION 'migration 073 requires the exact current schema contract 71';
    END IF;
    IF EXISTS (
        SELECT 1
        FROM runtime_schema_contracts
        WHERE schema_version = 73
           OR migration_name = '073_runtime_transport_observability'
    ) THEN
        RAISE EXCEPTION 'migration 073 found a conflicting historical schema contract 73';
    END IF;
    IF (
        SELECT COUNT(*)
        FROM pg_constraint
        WHERE conrelid = 'runtime_schema_contracts'::regclass
          AND conname = 'runtime_schema_contracts_runtime_pair_unique'
          AND contype = 'u'
    ) <> 1 THEN
        RAISE EXCEPTION 'migration 073 requires the legacy schema-to-wire pair uniqueness constraint';
    END IF;
    IF to_regclass('runtime_wire_contracts') IS NOT NULL THEN
        RAISE EXCEPTION 'migration 073 found a conflicting Runtime wire contract registry';
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
        RAISE EXCEPTION 'migration 073 found an unknown Runtime wire contract identity';
    END IF;
END
$$;

-- A pre-073 live Attachment has no trustworthy transport evidence. Preserve
-- it as detached history and make its Session reconnect through a canonical
-- endpoint so the Server can observe and persist the actual transport.
UPDATE runtime_session_attachments
SET detached_at = clock_timestamp(),
    disconnect_reason = 'runtime transport observability cutover'
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

ALTER TABLE runtime_session_attachments
    ADD COLUMN transport TEXT,
    ADD COLUMN transport_reason TEXT,
    ADD COLUMN transport_changed_at TIMESTAMPTZ;

UPDATE runtime_session_attachments
SET transport = 'unknown',
    transport_reason = NULL,
    transport_changed_at = attached_at;

-- Flush the deferred Attachment/Session consistency checks before the next
-- ALTER TABLE; PostgreSQL refuses DDL while those trigger events are pending.
SET CONSTRAINTS ALL IMMEDIATE;
SET CONSTRAINTS ALL DEFERRED;

ALTER TABLE runtime_session_attachments
    ALTER COLUMN attached_at SET DEFAULT statement_timestamp(),
    ALTER COLUMN transport SET DEFAULT 'long_poll',
    ALTER COLUMN transport SET NOT NULL,
    ALTER COLUMN transport_reason SET DEFAULT 'explicit',
    ALTER COLUMN transport_changed_at SET DEFAULT statement_timestamp(),
    ALTER COLUMN transport_changed_at SET NOT NULL,
    ADD CONSTRAINT runtime_session_attachments_transport_valid
        CHECK (transport IN ('websocket', 'long_poll', 'unknown')),
    ADD CONSTRAINT runtime_session_attachments_transport_reason_valid
        CHECK (
            (
                transport = 'unknown'
                AND detached_at IS NOT NULL
                AND transport_reason IS NULL
            )
            OR (
                transport IN ('websocket', 'long_poll')
                AND transport_reason IN (
                    'explicit',
                    'websocket_unavailable',
                    'policy_forced',
                    'recovery'
                )
            )
        ),
    ADD CONSTRAINT runtime_session_attachments_live_transport_known
        CHECK (detached_at IS NOT NULL OR transport IN ('websocket', 'long_poll')),
    ADD CONSTRAINT runtime_session_attachments_transport_time_order
        CHECK (
            transport_changed_at >= attached_at
            AND (detached_at IS NULL OR transport_changed_at <= detached_at)
        );

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
        NEW.attached_at,
        NEW.transport,
        NEW.transport_reason,
        NEW.transport_changed_at
    ) IS DISTINCT FROM ROW(
        OLD.id,
        OLD.runtime_session_id,
        OLD.core_instance_id,
        OLD.attachment_kind,
        OLD.attached_at,
        OLD.transport,
        OLD.transport_reason,
        OLD.transport_changed_at
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

-- Database schema generations and wire-contract generations are independent:
-- this migration changes only Server-owned persistence/observability and must
-- not force every SDK to negotiate a new wire digest. The old pair uniqueness
-- incorrectly encoded a one-to-one relationship, so remove it before
-- publishing schema 73 with the unchanged current wire contract.
CREATE TABLE runtime_wire_contracts (
    runtime_contract_id TEXT NOT NULL,
    runtime_contract_digest TEXT NOT NULL,
    registered_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (runtime_contract_id, runtime_contract_digest),
    CONSTRAINT runtime_wire_contracts_contract_id_len
        CHECK (char_length(runtime_contract_id) BETWEEN 1 AND 200),
    CONSTRAINT runtime_wire_contracts_digest_format
        CHECK (runtime_contract_digest ~ '^[a-f0-9]{64}$')
);

INSERT INTO runtime_wire_contracts (
    runtime_contract_id,
    runtime_contract_digest
)
SELECT DISTINCT runtime_contract_id, runtime_contract_digest
FROM runtime_schema_contracts;

ALTER TABLE runtime_sessions DROP CONSTRAINT runtime_sessions_contract_current;
ALTER TABLE runtime_nodes DROP CONSTRAINT runtime_nodes_contract_current;
ALTER TABLE runtime_sessions DROP CONSTRAINT runtime_sessions_contract_fk;
ALTER TABLE runtime_nodes DROP CONSTRAINT runtime_nodes_contract_fk;
ALTER TABLE runtime_schema_contracts
    DROP CONSTRAINT runtime_schema_contracts_runtime_pair_unique;

ALTER TABLE runtime_schema_contracts
    ADD CONSTRAINT runtime_schema_contracts_wire_fk
        FOREIGN KEY (runtime_contract_id, runtime_contract_digest)
        REFERENCES runtime_wire_contracts (
            runtime_contract_id,
            runtime_contract_digest
        )
        ON DELETE RESTRICT;

ALTER TABLE runtime_nodes
    ADD CONSTRAINT runtime_nodes_contract_fk
        FOREIGN KEY (runtime_contract_id, runtime_contract_digest)
        REFERENCES runtime_wire_contracts (
            runtime_contract_id,
            runtime_contract_digest
        )
        ON DELETE RESTRICT;

ALTER TABLE runtime_sessions
    ADD CONSTRAINT runtime_sessions_contract_fk
        FOREIGN KEY (runtime_contract_id, runtime_contract_digest)
        REFERENCES runtime_wire_contracts (
            runtime_contract_id,
            runtime_contract_digest
        )
        ON DELETE RESTRICT;

UPDATE runtime_schema_contracts
SET is_current = FALSE
WHERE schema_version = 71
  AND migration_name = '071_runtime_attachment_generation'
  AND runtime_contract_id = 'openlinker.runtime.v2'
  AND runtime_contract_digest = '3f84df167bbe211efdc6362ad5ec876aeedf881cbfb9677606982af63c7423e9'
  AND is_current;

INSERT INTO runtime_schema_contracts (
    schema_version,
    migration_name,
    runtime_contract_id,
    runtime_contract_digest,
    is_current
) VALUES (
    73,
    '073_runtime_transport_observability',
    'openlinker.runtime.v2',
    '3f84df167bbe211efdc6362ad5ec876aeedf881cbfb9677606982af63c7423e9',
    TRUE
);

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
