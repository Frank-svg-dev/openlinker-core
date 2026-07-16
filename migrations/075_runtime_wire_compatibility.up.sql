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
    oldest_digest CONSTANT TEXT := '60bef5cec7eeab563937187f48a458059995aebee161765032cddc17d0cdfa61';
    entry_digest CONSTANT TEXT := '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f';
    previous_digest CONSTANT TEXT := 'fb92bb6ddbc65bd3353b5d7c63ad148dd510e4d0ac0a6ca6110461d91e2dec53';
    current_digest CONSTANT TEXT := '3f84df167bbe211efdc6362ad5ec876aeedf881cbfb9677606982af63c7423e9';
BEGIN
    IF EXISTS (SELECT 1 FROM runtime_cluster_members) THEN
        RAISE EXCEPTION 'migration 075 requires zero registered Core cluster members';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM runtime_cluster_control
        WHERE singleton_id = 1 AND mode = 'hard_maintenance'
    ) THEN
        RAISE EXCEPTION 'migration 075 requires hard maintenance';
    END IF;
    IF EXISTS (SELECT 1 FROM runs WHERE status = 'running') THEN
        RAISE EXCEPTION 'migration 075 requires zero running Runs';
    END IF;
    IF (
        SELECT COUNT(*) FROM runtime_schema_contracts
        WHERE schema_version = 73
          AND migration_name = '073_runtime_transport_observability'
          AND runtime_contract_id = 'openlinker.runtime.v2'
          AND runtime_contract_digest = current_digest
          AND is_current
    ) <> 1 OR (SELECT COUNT(*) FROM runtime_schema_contracts WHERE is_current) <> 1 THEN
        RAISE EXCEPTION 'migration 075 requires the exact current schema contract 73';
    END IF;
    IF EXISTS (
        SELECT 1 FROM runtime_schema_contracts
        WHERE schema_version = 75 OR migration_name = '075_runtime_wire_compatibility'
    ) THEN
        RAISE EXCEPTION 'migration 075 found a conflicting historical schema contract 75';
    END IF;
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'runtime_wire_contracts'
          AND column_name = 'support_tier'
    ) THEN
        RAISE EXCEPTION 'migration 075 found an existing Runtime wire support tier';
    END IF;
    IF EXISTS (
        SELECT 1 FROM runtime_nodes
        WHERE runtime_contract_id <> 'openlinker.runtime.v2'
           OR runtime_contract_digest NOT IN (oldest_digest, entry_digest, previous_digest, current_digest)
    ) OR EXISTS (
        SELECT 1 FROM runtime_sessions
        WHERE runtime_contract_id <> 'openlinker.runtime.v2'
           OR runtime_contract_digest NOT IN (oldest_digest, entry_digest, previous_digest, current_digest)
    ) THEN
        RAISE EXCEPTION 'migration 075 found an unknown Runtime wire contract identity';
    END IF;
END
$$;

ALTER TABLE runtime_wire_contracts ADD COLUMN support_tier TEXT;

UPDATE runtime_wire_contracts
SET support_tier = CASE runtime_contract_digest
    WHEN '3f84df167bbe211efdc6362ad5ec876aeedf881cbfb9677606982af63c7423e9' THEN 'current'
    WHEN 'fb92bb6ddbc65bd3353b5d7c63ad148dd510e4d0ac0a6ca6110461d91e2dec53' THEN 'previous'
    ELSE 'historical'
END;

ALTER TABLE runtime_wire_contracts
    ALTER COLUMN support_tier SET NOT NULL,
    ADD CONSTRAINT runtime_wire_contracts_support_tier_valid
        CHECK (support_tier IN ('current', 'previous', 'historical')),
    ADD CONSTRAINT runtime_wire_contracts_support_identity
        CHECK (
            runtime_contract_id = 'openlinker.runtime.v2'
            AND (
                (support_tier = 'current'
                 AND runtime_contract_digest = '3f84df167bbe211efdc6362ad5ec876aeedf881cbfb9677606982af63c7423e9')
                OR
                (support_tier = 'previous'
                 AND runtime_contract_digest = 'fb92bb6ddbc65bd3353b5d7c63ad148dd510e4d0ac0a6ca6110461d91e2dec53')
                OR
                (support_tier = 'historical'
                 AND runtime_contract_digest NOT IN (
                    '3f84df167bbe211efdc6362ad5ec876aeedf881cbfb9677606982af63c7423e9',
                    'fb92bb6ddbc65bd3353b5d7c63ad148dd510e4d0ac0a6ca6110461d91e2dec53'
                 ))
            )
        );

CREATE UNIQUE INDEX idx_runtime_wire_contracts_current
    ON runtime_wire_contracts ((1)) WHERE support_tier = 'current';
CREATE UNIQUE INDEX idx_runtime_wire_contracts_previous
    ON runtime_wire_contracts ((1)) WHERE support_tier = 'previous';

ALTER TABLE runtime_sessions DROP CONSTRAINT runtime_sessions_contract_current;
ALTER TABLE runtime_nodes DROP CONSTRAINT runtime_nodes_contract_current;

UPDATE runtime_schema_contracts
SET is_current = FALSE
WHERE schema_version = 73
  AND migration_name = '073_runtime_transport_observability'
  AND runtime_contract_id = 'openlinker.runtime.v2'
  AND runtime_contract_digest = '3f84df167bbe211efdc6362ad5ec876aeedf881cbfb9677606982af63c7423e9'
  AND is_current;

INSERT INTO runtime_schema_contracts (
    schema_version, migration_name, runtime_contract_id,
    runtime_contract_digest, is_current
) VALUES (
    75,
    '075_runtime_wire_compatibility',
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
                 AND runtime_contract_digest IN (
                    'fb92bb6ddbc65bd3353b5d7c63ad148dd510e4d0ac0a6ca6110461d91e2dec53',
                    '3f84df167bbe211efdc6362ad5ec876aeedf881cbfb9677606982af63c7423e9'
                 ))
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
                 AND runtime_contract_digest IN (
                    'fb92bb6ddbc65bd3353b5d7c63ad148dd510e4d0ac0a6ca6110461d91e2dec53',
                    '3f84df167bbe211efdc6362ad5ec876aeedf881cbfb9677606982af63c7423e9'
                 ))
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
