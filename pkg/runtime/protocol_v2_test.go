package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestDecodeRuntimeEnvelopeStrict(t *testing.T) {
	t.Parallel()

	messageID := uuid.New()
	sentAt := time.Now().UTC().Format(time.RFC3339Nano)
	valid := `{
		"protocol_version":2,
		"runtime_contract_id":"openlinker.runtime.v2",
		"message_id":"` + messageID.String() + `",
		"type":"runtime.hello",
		"sent_at":"` + sentAt + `",
		"payload":{}
	}`

	tests := []struct {
		name string
		body string
		code RuntimeErrorCode
	}{
		{name: "valid", body: valid},
		{name: "empty body", body: "", code: RuntimeErrorValidationFailed},
		{name: "unknown envelope field", body: strings.Replace(valid, `"payload":{}`, `"unexpected":true,"payload":{}`, 1), code: RuntimeErrorValidationFailed},
		{name: "bad message uuid", body: strings.Replace(valid, messageID.String(), "not-a-uuid", 1), code: RuntimeErrorValidationFailed},
		{name: "zero message uuid", body: strings.Replace(valid, messageID.String(), uuid.Nil.String(), 1), code: RuntimeErrorValidationFailed},
		{name: "wrong protocol", body: strings.Replace(valid, `"protocol_version":2`, `"protocol_version":1`, 1), code: RuntimeErrorClientUpgradeRequired},
		{name: "wrong contract id", body: strings.Replace(valid, RuntimeContractID, "openlinker.runtime.v3", 1), code: RuntimeErrorClientUpgradeRequired},
		{name: "unknown message type", body: strings.Replace(valid, string(RuntimeMessageHello), "runtime.future", 1), code: RuntimeErrorValidationFailed},
		{name: "payload is null", body: strings.Replace(valid, `"payload":{}`, `"payload":null`, 1), code: RuntimeErrorValidationFailed},
		{name: "second json value", body: valid + `{}`, code: RuntimeErrorValidationFailed},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			envelope, err := DecodeRuntimeEnvelope(strings.NewReader(test.body))
			if test.code == "" {
				require.NoError(t, err)
				require.Equal(t, messageID, envelope.MessageID)
				return
			}
			requireRuntimeTransportCode(t, err, test.code)
		})
	}
}

func TestRuntimeDecoderRejectsOversizeCompleteBody(t *testing.T) {
	t.Parallel()

	body := bytes.NewReader(bytes.Repeat([]byte{' '}, int(MaxRuntimeV2MessageBytes)+1))
	_, err := DecodeRuntimeBody[RuntimeClaimRequest](body)
	requireRuntimeTransportCode(t, err, RuntimeErrorBadRequest)
}

func TestDecodeRuntimeBodyStrictPayload(t *testing.T) {
	t.Parallel()

	sessionID := uuid.New()
	tests := []struct {
		name string
		body string
	}{
		{
			name: "unknown field",
			body: `{"runtime_session_id":"` + sessionID.String() + `","capacity":0,"inflight":0,"extra":true}`,
		},
		{
			name: "required zero field omitted",
			body: `{"runtime_session_id":"` + sessionID.String() + `","inflight":0}`,
		},
		{
			name: "second payload",
			body: `{"runtime_session_id":"` + sessionID.String() + `","capacity":0,"inflight":0}{}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeRuntimeBody[RuntimeClaimRequest](strings.NewReader(test.body))
			requireRuntimeTransportCode(t, err, RuntimeErrorValidationFailed)
		})
	}
}

func TestDecodeRuntimeTypedMessageRejectsUnknownPayloadField(t *testing.T) {
	t.Parallel()

	hello := validRuntimeHelloPayload()
	payload, err := json.Marshal(hello)
	require.NoError(t, err)
	payload = bytes.Replace(payload, []byte(`}`), []byte(`,"unexpected":true}`), 1)
	body := `{
		"protocol_version":2,
		"runtime_contract_id":"` + RuntimeContractID + `",
		"message_id":"` + uuid.NewString() + `",
		"type":"runtime.hello",
		"sent_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `",
		"payload":` + string(payload) + `
	}`

	_, err = DecodeRuntimeTypedMessage[RuntimeHelloPayload](strings.NewReader(body), RuntimeMessageHello)
	requireRuntimeTransportCode(t, err, RuntimeErrorValidationFailed)
}

func TestValidateRuntimeHelloContractNegotiation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*RuntimeHelloPayload)
		code   RuntimeErrorCode
	}{
		{name: "valid"},
		{
			name: "wrong digest",
			mutate: func(payload *RuntimeHelloPayload) {
				payload.ContractDigest = strings.Repeat("0", 64)
			},
			code: RuntimeErrorClientUpgradeRequired,
		},
		{
			name: "required feature missing",
			mutate: func(payload *RuntimeHelloPayload) {
				payload.Features = payload.Features[:len(payload.Features)-1]
			},
			code: RuntimeErrorRequiredFeatureMissing,
		},
		{
			name: "duplicate feature",
			mutate: func(payload *RuntimeHelloPayload) {
				payload.Features = append(payload.Features, payload.Features[0])
			},
			code: RuntimeErrorValidationFailed,
		},
		{
			name: "zero node id",
			mutate: func(payload *RuntimeHelloPayload) {
				payload.NodeID = uuid.Nil
			},
			code: RuntimeErrorValidationFailed,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := validRuntimeHelloPayload()
			if test.mutate != nil {
				test.mutate(&payload)
			}
			err := ValidateRuntimePayload(payload)
			if test.code == "" {
				require.NoError(t, err)
				return
			}
			requireRuntimeTransportCode(t, err, test.code)
		})
	}
}

func TestValidateRuntimeReplyCorrelation(t *testing.T) {
	t.Parallel()

	request := runtimeTestEnvelope(RuntimeMessageRunAssigned, nil)
	requestID := request.MessageID
	otherID := uuid.New()

	tests := []struct {
		name  string
		reply RuntimeEnvelope
		code  RuntimeErrorCode
	}{
		{name: "assignment ack", reply: runtimeTestEnvelope(RuntimeMessageAssignmentAck, &requestID)},
		{name: "assignment reject", reply: runtimeTestEnvelope(RuntimeMessageAssignmentReject, &requestID)},
		{name: "business error", reply: runtimeTestEnvelope(RuntimeMessageError, &requestID)},
		{name: "wrong reply id", reply: runtimeTestEnvelope(RuntimeMessageAssignmentAck, &otherID), code: RuntimeErrorValidationFailed},
		{name: "wrong reply type", reply: runtimeTestEnvelope(RuntimeMessageRunEventAck, &requestID), code: RuntimeErrorValidationFailed},
		{name: "missing reply id", reply: runtimeTestEnvelope(RuntimeMessageAssignmentAck, nil), code: RuntimeErrorValidationFailed},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateRuntimeReplyCorrelation(request, test.reply)
			if test.code == "" {
				require.NoError(t, err)
				return
			}
			requireRuntimeTransportCode(t, err, test.code)
		})
	}
}

func TestRuntimeTransportErrorMappings(t *testing.T) {
	t.Parallel()

	ranges := []EventRange{{Start: 2, End: 3}}
	tests := []struct {
		name   string
		err    error
		code   RuntimeErrorCode
		ranges []EventRange
	}{
		{
			name:   "event store",
			err:    &RuntimeEventError{Code: RuntimeEventErrorEventsMissing, MissingRanges: ranges},
			code:   RuntimeErrorEventsMissing,
			ranges: ranges,
		},
		{
			name: "result finalizer",
			err:  &RuntimeResultError{Code: RuntimeResultErrorResultIDConflict},
			code: RuntimeErrorResultIDConflict,
		},
		{
			name: "invalid event",
			err:  ErrInvalidRuntimeEvent,
			code: RuntimeErrorValidationFailed,
		},
		{
			name: "private internal error",
			err:  errors.New("postgres password must not leak"),
			code: RuntimeErrorInternal,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mapped := MapRuntimeTransportError(test.err)
			require.Equal(t, test.code, mapped.Body.Code)
			require.Equal(t, test.ranges, mapped.Body.MissingEventRanges)
			require.NotContains(t, mapped.Body.Message, "password")
			require.Equal(t, mapped.Body, mapped.Envelope().Error)
		})
	}
}

func TestRuntimeHTTPAndWebSocketMappings(t *testing.T) {
	t.Parallel()

	require.Equal(t, http.StatusUpgradeRequired, RuntimeHTTPStatus(RuntimeErrorClientUpgradeRequired))
	require.Equal(t, http.StatusUnprocessableEntity, RuntimeHTTPStatus(RuntimeErrorValidationFailed))
	require.Equal(t, http.StatusConflict, RuntimeHTTPStatus(RuntimeErrorEventsMissing))
	require.Equal(t, http.StatusServiceUnavailable, RuntimeHTTPStatus(RuntimeErrorServiceUnavailable))

	tests := []struct {
		code      RuntimeErrorCode
		closeCode int
		fatal     bool
	}{
		{code: RuntimeErrorUnauthorized, closeCode: 4401, fatal: true},
		{code: RuntimeErrorClientUpgradeRequired, closeCode: 4406, fatal: true},
		{code: RuntimeErrorSessionConflict, closeCode: 4409, fatal: true},
		{code: RuntimeErrorRequiredFeatureMissing, closeCode: 4412, fatal: true},
		{code: RuntimeErrorValidationFailed, closeCode: 1002, fatal: true},
		{code: RuntimeErrorEventsMissing},
	}
	for _, test := range tests {
		closeCode, fatal := RuntimeWebSocketCloseCode(test.code)
		require.Equal(t, test.closeCode, closeCode)
		require.Equal(t, test.fatal, fatal)
	}
}

func validRuntimeHelloPayload() RuntimeHelloPayload {
	return RuntimeHelloPayload{
		NodeID:           uuid.New(),
		AgentID:          uuid.New(),
		WorkerID:         "worker-1",
		RuntimeSessionID: uuid.New(),
		SessionEpoch:     1,
		NodeVersion:      "2.0.0",
		Capacity:         1,
		Features:         RuntimeRequiredFeatures(),
		ContractDigest:   RuntimeContractDigest,
	}
}

func runtimeTestEnvelope(messageType RuntimeMessageType, replyTo *uuid.UUID) RuntimeEnvelope {
	return RuntimeEnvelope{
		RuntimeEnvelopeFields: RuntimeEnvelopeFields{
			ProtocolVersion:   RuntimeProtocolVersion,
			RuntimeContractID: RuntimeContractID,
			MessageID:         uuid.New(),
			ReplyToMessageID:  replyTo,
			Type:              messageType,
			SentAt:            time.Now().UTC(),
		},
		Payload: json.RawMessage(`{}`),
	}
}

func requireRuntimeTransportCode(t *testing.T, err error, code RuntimeErrorCode) {
	t.Helper()
	require.Error(t, err)
	var transportErr *RuntimeTransportError
	require.ErrorAs(t, err, &transportErr)
	require.Equal(t, code, transportErr.Body.Code)
}
