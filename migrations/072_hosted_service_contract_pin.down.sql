BEGIN;

ALTER TABLE hosted_service_executions
    DROP CONSTRAINT IF EXISTS hosted_service_executions_schema_fingerprint_valid,
    DROP CONSTRAINT IF EXISTS hosted_service_executions_contract_hash_valid,
    DROP COLUMN IF EXISTS input_schema_fingerprint,
    DROP COLUMN IF EXISTS expected_contract_hash;

COMMIT;
