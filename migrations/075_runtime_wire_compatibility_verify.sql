DO $$
DECLARE
    previous_digest CONSTANT TEXT := 'fb92bb6ddbc65bd3353b5d7c63ad148dd510e4d0ac0a6ca6110461d91e2dec53';
    current_digest CONSTANT TEXT := '3f84df167bbe211efdc6362ad5ec876aeedf881cbfb9677606982af63c7423e9';
    n2_digest CONSTANT TEXT := '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f';
    node_constraint TEXT;
    session_constraint TEXT;
BEGIN
    IF (
        SELECT COUNT(*) FROM runtime_schema_contracts
        WHERE schema_version = 75
          AND migration_name = '075_runtime_wire_compatibility'
          AND runtime_contract_id = 'openlinker.runtime.v2'
          AND runtime_contract_digest = current_digest
          AND is_current
    ) <> 1 OR (SELECT COUNT(*) FROM runtime_schema_contracts WHERE is_current) <> 1 THEN
        RAISE EXCEPTION 'runtime schema contract 75 is missing or mismatched';
    END IF;
    IF (
        SELECT COUNT(*) FROM runtime_schema_contracts
        WHERE schema_version = 73
          AND migration_name = '073_runtime_transport_observability'
          AND runtime_contract_id = 'openlinker.runtime.v2'
          AND runtime_contract_digest = current_digest
          AND NOT is_current
    ) <> 1 THEN
        RAISE EXCEPTION 'runtime schema contract 73 history is missing or mismatched';
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
    ) <> 1 OR EXISTS (
        SELECT 1 FROM runtime_wire_contracts
        WHERE runtime_contract_digest = n2_digest
          AND support_tier <> 'historical'
    ) THEN
        RAISE EXCEPTION 'Runtime wire support tiers are not exact N/N-1';
    END IF;
    IF (SELECT COUNT(*) FROM runtime_wire_contracts WHERE support_tier = 'current') <> 1
       OR (SELECT COUNT(*) FROM runtime_wire_contracts WHERE support_tier = 'previous') <> 1
       OR EXISTS (
            SELECT 1 FROM runtime_wire_contracts
            WHERE support_tier IN ('current', 'previous')
              AND runtime_contract_digest NOT IN (current_digest, previous_digest)
       ) THEN
        RAISE EXCEPTION 'Runtime wire compatibility ring is not bounded to two generations';
    END IF;
    IF (
        SELECT COUNT(*) FROM pg_constraint
        WHERE conrelid = 'runtime_wire_contracts'::regclass
          AND conname IN (
              'runtime_wire_contracts_support_tier_valid',
              'runtime_wire_contracts_support_identity'
          )
          AND contype = 'c' AND convalidated
    ) <> 2 THEN
        RAISE EXCEPTION 'Runtime wire support constraints are missing';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_index
        WHERE indexrelid = 'idx_runtime_wire_contracts_current'::regclass
          AND indisunique AND indisvalid
    ) OR NOT EXISTS (
        SELECT 1 FROM pg_index
        WHERE indexrelid = 'idx_runtime_wire_contracts_previous'::regclass
          AND indisunique AND indisvalid
    ) THEN
        RAISE EXCEPTION 'Runtime wire support uniqueness indexes are missing';
    END IF;

    SELECT pg_get_constraintdef(oid) INTO STRICT node_constraint
    FROM pg_constraint
    WHERE conrelid = 'runtime_nodes'::regclass
      AND conname = 'runtime_nodes_contract_current'
      AND contype = 'c' AND convalidated;
    SELECT pg_get_constraintdef(oid) INTO STRICT session_constraint
    FROM pg_constraint
    WHERE conrelid = 'runtime_sessions'::regclass
      AND conname = 'runtime_sessions_contract_current'
      AND contype = 'c' AND convalidated;

    IF node_constraint NOT LIKE '%' || current_digest || '%'
       OR node_constraint NOT LIKE '%' || previous_digest || '%'
       OR session_constraint NOT LIKE '%' || current_digest || '%'
       OR session_constraint NOT LIKE '%' || previous_digest || '%' THEN
        RAISE EXCEPTION 'active Runtime constraints do not permit exact N/N-1';
    END IF;
    IF EXISTS (
        SELECT 1 FROM runtime_nodes
        WHERE status IN ('active', 'draining')
          AND runtime_contract_digest NOT IN (current_digest, previous_digest)
    ) OR EXISTS (
        SELECT 1 FROM runtime_sessions
        WHERE status IN ('active', 'draining')
          AND runtime_contract_digest NOT IN (current_digest, previous_digest)
    ) THEN
        RAISE EXCEPTION 'active Runtime principal uses an unsupported wire generation';
    END IF;
    IF EXISTS (
        SELECT 1
        FROM runtime_sessions session
        JOIN runtime_nodes node ON node.node_id = session.node_id
        WHERE session.status IN ('active', 'draining')
          AND (
              node.protocol_version <> session.protocol_version
              OR node.runtime_contract_id <> session.runtime_contract_id
              OR node.runtime_contract_digest <> session.runtime_contract_digest
          )
    ) THEN
        RAISE EXCEPTION 'active Runtime Node and Session generations disagree';
    END IF;
    IF EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'runtime_schema_contracts'::regclass
          AND conname = 'runtime_schema_contracts_runtime_pair_unique'
    ) THEN
        RAISE EXCEPTION 'legacy schema-to-wire pair uniqueness constraint remains installed';
    END IF;
END
$$;
