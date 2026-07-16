DO $$
DECLARE
    oldest_digest CONSTANT TEXT := '60bef5cec7eeab563937187f48a458059995aebee161765032cddc17d0cdfa61';
    entry_digest CONSTANT TEXT := '857598f6e8f07d87d1f7240e34d98f0911bf23e5204a865d282a6bcb7f52865f';
    previous_digest CONSTANT TEXT := 'fb92bb6ddbc65bd3353b5d7c63ad148dd510e4d0ac0a6ca6110461d91e2dec53';
    current_digest CONSTANT TEXT := '3f84df167bbe211efdc6362ad5ec876aeedf881cbfb9677606982af63c7423e9';
    history_guard TEXT;
    node_constraint TEXT;
    session_constraint TEXT;
    schema_wire_fk TEXT;
    node_wire_fk TEXT;
    session_wire_fk TEXT;
BEGIN
    IF (
        SELECT COUNT(*)
        FROM runtime_schema_contracts
        WHERE schema_version = 73
          AND migration_name = '073_runtime_transport_observability'
          AND runtime_contract_id = 'openlinker.runtime.v2'
          AND runtime_contract_digest = current_digest
          AND is_current
    ) <> 1 OR (SELECT COUNT(*) FROM runtime_schema_contracts WHERE is_current) <> 1 THEN
        RAISE EXCEPTION 'runtime schema contract 73 is missing or mismatched';
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
        RAISE EXCEPTION 'runtime schema contract 71 history is missing or mismatched';
    END IF;

    IF (
        SELECT COUNT(*)
        FROM runtime_schema_contracts
        WHERE runtime_contract_id = 'openlinker.runtime.v2'
          AND runtime_contract_digest = current_digest
          AND schema_version IN (71, 73)
    ) <> 2 THEN
        RAISE EXCEPTION 'database schema generations do not share the unchanged current wire digest';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'runtime_schema_contracts'::regclass
          AND conname = 'runtime_schema_contracts_runtime_pair_unique'
    ) THEN
        RAISE EXCEPTION 'legacy schema-to-wire pair uniqueness constraint remains installed';
    END IF;

    IF to_regclass('runtime_wire_contracts') IS NULL THEN
        RAISE EXCEPTION 'Runtime wire contract registry is missing';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM runtime_schema_contracts schema_contract
        WHERE NOT EXISTS (
            SELECT 1
            FROM runtime_wire_contracts wire
            WHERE wire.runtime_contract_id = schema_contract.runtime_contract_id
              AND wire.runtime_contract_digest = schema_contract.runtime_contract_digest
        )
    ) OR EXISTS (
        SELECT 1
        FROM runtime_wire_contracts wire
        WHERE NOT EXISTS (
            SELECT 1
            FROM runtime_schema_contracts schema_contract
            WHERE schema_contract.runtime_contract_id = wire.runtime_contract_id
              AND schema_contract.runtime_contract_digest = wire.runtime_contract_digest
        )
    ) THEN
        RAISE EXCEPTION 'Runtime wire registry and schema history are inconsistent';
    END IF;

    IF (
        SELECT COUNT(*)
        FROM pg_constraint
        WHERE conrelid = 'runtime_wire_contracts'::regclass
          AND contype = 'p'
          AND convalidated
    ) <> 1 THEN
        RAISE EXCEPTION 'Runtime wire contract registry primary key is missing';
    END IF;

    IF NOT EXISTS (
        SELECT 1
        FROM pg_index
        WHERE indexrelid = 'idx_runtime_schema_contracts_current'::regclass
          AND indisunique
          AND indisvalid
    ) THEN
        RAISE EXCEPTION 'unique current Runtime schema index is missing or invalid';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM runtime_nodes
        WHERE runtime_contract_id <> 'openlinker.runtime.v2'
           OR runtime_contract_digest NOT IN (oldest_digest, entry_digest, previous_digest, current_digest)
    ) OR EXISTS (
        SELECT 1
        FROM runtime_sessions
        WHERE runtime_contract_id <> 'openlinker.runtime.v2'
           OR runtime_contract_digest NOT IN (oldest_digest, entry_digest, previous_digest, current_digest)
    ) THEN
        RAISE EXCEPTION 'Runtime principal carries an unknown wire contract identity';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM runtime_nodes
        WHERE status IN ('active', 'draining')
          AND runtime_contract_digest <> current_digest
    ) OR EXISTS (
        SELECT 1
        FROM runtime_sessions
        WHERE status IN ('active', 'draining')
          AND runtime_contract_digest <> current_digest
    ) THEN
        RAISE EXCEPTION 'active Runtime principal does not use the current wire contract';
    END IF;

    IF (
        SELECT COUNT(*)
        FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'runtime_session_attachments'
          AND column_name IN ('transport', 'transport_reason', 'transport_changed_at')
    ) <> 3 THEN
        RAISE EXCEPTION 'Runtime Attachment transport columns are missing';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM runtime_session_attachments
        WHERE transport NOT IN ('websocket', 'long_poll', 'unknown')
           OR transport_changed_at IS NULL
           OR transport_changed_at < attached_at
           OR (detached_at IS NOT NULL AND transport_changed_at > detached_at)
           OR (
                detached_at IS NULL
                AND transport NOT IN ('websocket', 'long_poll')
              )
           OR (
                transport = 'unknown'
                AND (detached_at IS NULL OR transport_reason IS NOT NULL)
              )
           OR (
                transport IN ('websocket', 'long_poll')
                AND transport_reason NOT IN (
                    'explicit',
                    'websocket_unavailable',
                    'policy_forced',
                    'recovery'
                )
              )
    ) THEN
        RAISE EXCEPTION 'Runtime Attachment transport evidence is invalid';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM runtime_sessions session
        JOIN runtime_session_attachments attachment
          ON attachment.runtime_session_id = session.runtime_session_id
        WHERE session.status IN ('active', 'draining')
          AND attachment.detached_at IS NULL
          AND (
              attachment.core_instance_id IS DISTINCT FROM session.attached_core_instance_id
              OR attachment.transport NOT IN ('websocket', 'long_poll')
          )
    ) THEN
        RAISE EXCEPTION 'live Runtime Session lacks authoritative transport evidence';
    END IF;

    IF (
        SELECT COUNT(*)
        FROM pg_constraint
        WHERE conrelid = 'runtime_session_attachments'::regclass
          AND conname IN (
              'runtime_session_attachments_transport_valid',
              'runtime_session_attachments_transport_reason_valid',
              'runtime_session_attachments_live_transport_known',
              'runtime_session_attachments_transport_time_order'
          )
          AND contype = 'c'
          AND convalidated
    ) <> 4 THEN
        RAISE EXCEPTION 'Runtime Attachment transport constraints are missing';
    END IF;

    SELECT pg_get_functiondef('enforce_runtime_session_attachment_history()'::regprocedure)
    INTO STRICT history_guard;
    IF history_guard NOT LIKE '%NEW.transport%'
       OR history_guard NOT LIKE '%NEW.transport_reason%'
       OR history_guard NOT LIKE '%NEW.transport_changed_at%' THEN
        RAISE EXCEPTION 'Runtime Attachment history guard does not protect transport evidence';
    END IF;

    SELECT pg_get_constraintdef(oid)
    INTO STRICT node_constraint
    FROM pg_constraint
    WHERE conrelid = 'runtime_nodes'::regclass
      AND conname = 'runtime_nodes_contract_current'
      AND contype = 'c'
      AND convalidated;

    SELECT pg_get_constraintdef(oid)
    INTO STRICT session_constraint
    FROM pg_constraint
    WHERE conrelid = 'runtime_sessions'::regclass
      AND conname = 'runtime_sessions_contract_current'
      AND contype = 'c'
      AND convalidated;

    IF node_constraint NOT LIKE '%' || current_digest || '%'
       OR node_constraint NOT LIKE '%' || previous_digest || '%'
       OR node_constraint NOT LIKE '%' || entry_digest || '%'
       OR node_constraint NOT LIKE '%' || oldest_digest || '%'
       OR session_constraint NOT LIKE '%' || current_digest || '%'
       OR session_constraint NOT LIKE '%' || previous_digest || '%'
       OR session_constraint NOT LIKE '%' || entry_digest || '%'
       OR session_constraint NOT LIKE '%' || oldest_digest || '%' THEN
        RAISE EXCEPTION 'Runtime current-contract checks do not preserve wire-contract history';
    END IF;

    SELECT pg_get_constraintdef(oid)
    INTO STRICT schema_wire_fk
    FROM pg_constraint
    WHERE conrelid = 'runtime_schema_contracts'::regclass
      AND conname = 'runtime_schema_contracts_wire_fk'
      AND contype = 'f'
      AND convalidated;

    SELECT pg_get_constraintdef(oid)
    INTO STRICT node_wire_fk
    FROM pg_constraint
    WHERE conrelid = 'runtime_nodes'::regclass
      AND conname = 'runtime_nodes_contract_fk'
      AND contype = 'f'
      AND convalidated;

    SELECT pg_get_constraintdef(oid)
    INTO STRICT session_wire_fk
    FROM pg_constraint
    WHERE conrelid = 'runtime_sessions'::regclass
      AND conname = 'runtime_sessions_contract_fk'
      AND contype = 'f'
      AND convalidated;

    IF schema_wire_fk NOT LIKE '%runtime_wire_contracts%'
       OR node_wire_fk NOT LIKE '%runtime_wire_contracts%'
       OR session_wire_fk NOT LIKE '%runtime_wire_contracts%' THEN
        RAISE EXCEPTION 'Runtime principals are not bound to the independent wire registry';
    END IF;
END
$$;
