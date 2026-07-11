package runtime

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	// RunFingerprintSchemaV1 makes changes to the semantic fingerprint envelope
	// explicit. Changing this value is a breaking idempotency-contract change.
	RunFingerprintSchemaV1 = "openlinker.run-create-fingerprint.v1"

	maxIdempotencyKeyBytes = 255
)

// IdempotencyErrorClass is a stable, transport-independent classification.
// Handlers map these classes to their protocol-specific validation errors; the
// raw Idempotency-Key is deliberately never retained in an error.
type IdempotencyErrorClass string

const (
	IdempotencyErrorKeyRequired  IdempotencyErrorClass = "IDEMPOTENCY_KEY_REQUIRED"
	IdempotencyErrorKeyInvalid   IdempotencyErrorClass = "IDEMPOTENCY_KEY_INVALID"
	IdempotencyErrorInputInvalid IdempotencyErrorClass = "IDEMPOTENCY_INPUT_NOT_IJSON"
	IdempotencyErrorKeyReused    IdempotencyErrorClass = "IDEMPOTENCY_KEY_REUSED"
)

// IdempotencyError reports a safe error class without echoing request data.
type IdempotencyError struct {
	Class IdempotencyErrorClass
	cause error
}

func (e *IdempotencyError) Error() string {
	if e == nil {
		return ""
	}
	switch e.Class {
	case IdempotencyErrorKeyRequired:
		return "idempotency key is required"
	case IdempotencyErrorKeyInvalid:
		return "idempotency key must contain 1 to 255 printable ASCII bytes"
	case IdempotencyErrorInputInvalid:
		return "idempotency fingerprint input is not valid I-JSON"
	case IdempotencyErrorKeyReused:
		return "idempotency key was already used for a different request"
	default:
		return "idempotency validation failed"
	}
}

func (e *IdempotencyError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// IdempotencyErrorClassOf returns the stable class for service/handler mapping.
func IdempotencyErrorClassOf(err error) (IdempotencyErrorClass, bool) {
	var target *IdempotencyError
	if !errors.As(err, &target) {
		return "", false
	}
	return target.Class, true
}

// RunFingerprintTarget identifies the immutable execution target. ReleaseID is
// optional until Agent Releases are introduced, but is already part of the
// fingerprint contract so the cutover does not need another envelope shape.
type RunFingerprintTarget struct {
	AgentID   string
	ReleaseID string
}

// RunFingerprintSource identifies the entry protocol and operation after
// aliases have been normalized (for example, rest/create-run or a2a/message-send).
type RunFingerprintSource struct {
	Protocol string
	Method   string
}

// RunFingerprintDelegation contains execution-visible delegation options.
// ParentRunID and CallerAgentID live on RunFingerprintInput so they cannot be
// accidentally omitted when Delegation has no extra options.
type RunFingerprintDelegation struct {
	Reason  string
	Mode    string
	Depth   int
	Options map[string]any
}

// RunFingerprintA2A contains A2A context, references, and output semantics.
// TraceID is accepted solely to make exclusion explicit; it is transport
// correlation data and is not serialized into the fingerprint.
type RunFingerprintA2A struct {
	MessageID           string
	ProtocolContextID   string
	ProtocolTaskID      string
	RootContextID       string
	ParentContextID     string
	ParentTaskID        string
	ReferenceTaskIDs    []string
	Source              string
	Visibility          string
	AcceptedOutputModes []string
	Extensions          []string
	Options             map[string]any

	TraceID string
}

// RunFingerprintTransport records intentionally excluded delivery metadata.
// Keeping it separate prevents recursive name-based filtering from deleting a
// legitimate request_id, trace_id, retry_count, or idempotency_key inside the
// user-controlled input or metadata objects.
type RunFingerprintTransport struct {
	RequestID      string
	TraceID        string
	TraceParent    string
	RetryCount     int
	IdempotencyKey string
}

// RunFingerprintInput is the normalized semantic snapshot used by every Run
// creation entrypoint. Callback, Delivery, and Options must contain JSON-domain
// values after protocol aliases and defaults have been collapsed. Transport is
// intentionally excluded by FingerprintRunCreation.
type RunFingerprintInput struct {
	Target        RunFingerprintTarget
	Input         map[string]any
	Metadata      map[string]any
	Callback      any
	Delivery      any
	Source        RunFingerprintSource
	ParentRunID   string
	CallerAgentID string
	Delegation    *RunFingerprintDelegation
	A2A           *RunFingerprintA2A
	Visibility    string
	Options       any

	Transport RunFingerprintTransport
}

// HashIdempotencyKey validates the wire value and returns the only form that
// may be persisted. It does not trim or otherwise normalize the key.
func HashIdempotencyKey(key string) ([sha256.Size]byte, error) {
	if len(key) == 0 {
		return [sha256.Size]byte{}, &IdempotencyError{Class: IdempotencyErrorKeyRequired}
	}
	if len(key) > maxIdempotencyKeyBytes {
		return [sha256.Size]byte{}, &IdempotencyError{Class: IdempotencyErrorKeyInvalid}
	}
	for i := 0; i < len(key); i++ {
		// Printable ASCII is exactly the non-control ASCII range. DEL (0x7f)
		// is a control character as well.
		if key[i] < 0x20 || key[i] > 0x7e {
			return [sha256.Size]byte{}, &IdempotencyError{Class: IdempotencyErrorKeyInvalid}
		}
	}
	return sha256.Sum256([]byte(key)), nil
}

// FingerprintRunCreation computes SHA-256 over the RFC 8785 canonical JSON
// representation of the normalized Run creation semantics.
func FingerprintRunCreation(input RunFingerprintInput) ([sha256.Size]byte, error) {
	canonical, err := CanonicalizeRFC8785(runFingerprintValue(input))
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(canonical), nil
}

func runFingerprintValue(input RunFingerprintInput) map[string]any {
	metadata := input.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}

	var delegation any
	if input.Delegation != nil {
		delegation = map[string]any{
			"depth":   input.Delegation.Depth,
			"mode":    input.Delegation.Mode,
			"options": input.Delegation.Options,
			"reason":  input.Delegation.Reason,
		}
	}

	var a2a any
	if input.A2A != nil {
		a2a = map[string]any{
			"accepted_output_modes": input.A2A.AcceptedOutputModes,
			"extensions":            input.A2A.Extensions,
			"message_id":            input.A2A.MessageID,
			"options":               input.A2A.Options,
			"parent_context_id":     input.A2A.ParentContextID,
			"parent_task_id":        input.A2A.ParentTaskID,
			"protocol_context_id":   input.A2A.ProtocolContextID,
			"protocol_task_id":      input.A2A.ProtocolTaskID,
			"reference_task_ids":    input.A2A.ReferenceTaskIDs,
			"root_context_id":       input.A2A.RootContextID,
			"source":                input.A2A.Source,
			"visibility":            input.A2A.Visibility,
		}
	}

	return map[string]any{
		"a2a":             a2a,
		"callback":        input.Callback,
		"caller_agent_id": input.CallerAgentID,
		"delegation":      delegation,
		"delivery":        input.Delivery,
		"input":           input.Input,
		"metadata":        metadata,
		"options":         input.Options,
		"parent_run_id":   input.ParentRunID,
		"schema":          RunFingerprintSchemaV1,
		"source": map[string]any{
			"method":   input.Source.Method,
			"protocol": input.Source.Protocol,
		},
		"target": map[string]any{
			"agent_id":   input.Target.AgentID,
			"release_id": input.Target.ReleaseID,
		},
		"visibility": input.Visibility,
	}
}

// CanonicalizeRFC8785 serializes an already parsed I-JSON value using JSON
// Canonicalization Scheme rules. It accepts nil, booleans, strings, finite
// IEEE-754 numbers, string-keyed maps, arrays/slices, pointers, interfaces, and
// json.Number. Structs and custom marshalers are rejected so hidden toJSON-like
// behavior cannot change an idempotency fingerprint.
func CanonicalizeRFC8785(value any) ([]byte, error) {
	out, err := appendCanonicalJSON(nil, reflect.ValueOf(value), make(map[canonicalVisit]struct{}))
	if err != nil {
		return nil, &IdempotencyError{Class: IdempotencyErrorInputInvalid, cause: err}
	}
	return out, nil
}

type canonicalVisit struct {
	typeName reflect.Type
	pointer  uintptr
}

type canonicalObjectKey struct {
	value      reflect.Value
	text       string
	utf16Units []uint16
}

var jsonNumberType = reflect.TypeOf(json.Number(""))

func appendCanonicalJSON(dst []byte, value reflect.Value, stack map[canonicalVisit]struct{}) ([]byte, error) {
	if !value.IsValid() {
		return append(dst, "null"...), nil
	}

	for value.Kind() == reflect.Interface {
		if value.IsNil() {
			return append(dst, "null"...), nil
		}
		value = value.Elem()
	}

	if value.Type() == jsonNumberType {
		return appendCanonicalJSONNumber(dst, value.Interface().(json.Number))
	}

	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return append(dst, "null"...), nil
		}
		visit := canonicalVisit{typeName: value.Type(), pointer: value.Pointer()}
		if _, exists := stack[visit]; exists {
			return nil, fmt.Errorf("cyclic JSON pointer")
		}
		stack[visit] = struct{}{}
		defer delete(stack, visit)
		return appendCanonicalJSON(dst, value.Elem(), stack)
	}

	switch value.Kind() {
	case reflect.Bool:
		return strconv.AppendBool(dst, value.Bool()), nil
	case reflect.String:
		return appendCanonicalJSONString(dst, value.String())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n := value.Int()
		if !signedIntegerExactlyRepresentable(n) {
			return nil, fmt.Errorf("integer is not exactly representable as IEEE-754 binary64")
		}
		return appendCanonicalFloat(dst, float64(n), 64)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		n := value.Uint()
		if !unsignedIntegerExactlyRepresentable(n) {
			return nil, fmt.Errorf("unsigned integer is not exactly representable as IEEE-754 binary64")
		}
		return appendCanonicalFloat(dst, float64(n), 64)
	case reflect.Float32:
		return appendCanonicalFloat(dst, value.Float(), 32)
	case reflect.Float64:
		return appendCanonicalFloat(dst, value.Float(), 64)
	case reflect.Map:
		return appendCanonicalJSONObject(dst, value, stack)
	case reflect.Array:
		return appendCanonicalJSONArray(dst, value, stack)
	case reflect.Slice:
		if value.IsNil() {
			return append(dst, "null"...), nil
		}
		if value.Len() == 0 {
			return append(dst, '[', ']'), nil
		}
		visit := canonicalVisit{typeName: value.Type(), pointer: value.Pointer()}
		if _, exists := stack[visit]; exists {
			return nil, fmt.Errorf("cyclic JSON array")
		}
		stack[visit] = struct{}{}
		defer delete(stack, visit)
		return appendCanonicalJSONArray(dst, value, stack)
	default:
		return nil, fmt.Errorf("unsupported JSON value type %s", value.Type())
	}
}

func appendCanonicalJSONObject(dst []byte, value reflect.Value, stack map[canonicalVisit]struct{}) ([]byte, error) {
	if value.IsNil() {
		return append(dst, "null"...), nil
	}
	if value.Type().Key().Kind() != reflect.String {
		return nil, fmt.Errorf("JSON object key type must be string")
	}

	visit := canonicalVisit{typeName: value.Type(), pointer: value.Pointer()}
	if _, exists := stack[visit]; exists {
		return nil, fmt.Errorf("cyclic JSON object")
	}
	stack[visit] = struct{}{}
	defer delete(stack, visit)

	keys := make([]canonicalObjectKey, 0, value.Len())
	iter := value.MapRange()
	for iter.Next() {
		key := iter.Key()
		text := key.String()
		if !utf8.ValidString(text) {
			return nil, fmt.Errorf("JSON object key is not valid Unicode")
		}
		keys = append(keys, canonicalObjectKey{
			value:      key,
			text:       text,
			utf16Units: utf16.Encode([]rune(text)),
		})
	}
	sort.Slice(keys, func(i, j int) bool {
		return compareUTF16(keys[i].utf16Units, keys[j].utf16Units) < 0
	})

	dst = append(dst, '{')
	for i, key := range keys {
		if i > 0 {
			dst = append(dst, ',')
		}
		var err error
		dst, err = appendCanonicalJSONString(dst, key.text)
		if err != nil {
			return nil, err
		}
		dst = append(dst, ':')
		dst, err = appendCanonicalJSON(dst, value.MapIndex(key.value), stack)
		if err != nil {
			return nil, err
		}
	}
	return append(dst, '}'), nil
}

func appendCanonicalJSONArray(dst []byte, value reflect.Value, stack map[canonicalVisit]struct{}) ([]byte, error) {
	dst = append(dst, '[')
	for i := 0; i < value.Len(); i++ {
		if i > 0 {
			dst = append(dst, ',')
		}
		var err error
		dst, err = appendCanonicalJSON(dst, value.Index(i), stack)
		if err != nil {
			return nil, err
		}
	}
	return append(dst, ']'), nil
}

func appendCanonicalJSONNumber(dst []byte, number json.Number) ([]byte, error) {
	raw := number.String()
	if raw == "" || raw != strings.TrimSpace(raw) || !json.Valid([]byte(raw)) {
		return nil, fmt.Errorf("invalid JSON number")
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, fmt.Errorf("JSON number is not a finite IEEE-754 binary64 value")
	}
	return appendCanonicalFloat(dst, f, 64)
}

func appendCanonicalFloat(dst []byte, number float64, bitSize int) ([]byte, error) {
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return nil, fmt.Errorf("JSON number is not finite")
	}
	// ECMAScript JSON.stringify serializes both positive and negative zero as 0.
	if number == 0 {
		return append(dst, '0'), nil
	}

	abs := math.Abs(number)
	format := byte('f')
	if (bitSize == 64 && (abs < 1e-6 || abs >= 1e21)) ||
		(bitSize == 32 && (float32(abs) < 1e-6 || float32(abs) >= 1e21)) {
		format = 'e'
	}
	dst = strconv.AppendFloat(dst, number, format, -1, bitSize)
	if format == 'e' {
		// strconv uses e-09 while ECMAScript uses e-9.
		n := len(dst)
		if n >= 4 && dst[n-4] == 'e' && dst[n-3] == '-' && dst[n-2] == '0' {
			dst[n-2] = dst[n-1]
			dst = dst[:n-1]
		}
	}
	return dst, nil
}

func appendCanonicalJSONString(dst []byte, value string) ([]byte, error) {
	if !utf8.ValidString(value) {
		return nil, fmt.Errorf("JSON string is not valid Unicode")
	}

	const hex = "0123456789abcdef"
	dst = append(dst, '"')
	for _, r := range value {
		switch r {
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\t':
			dst = append(dst, '\\', 't')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '"', '\\':
			dst = append(dst, '\\', byte(r))
		default:
			if r < 0x20 {
				dst = append(dst, '\\', 'u', '0', '0', hex[byte(r)>>4], hex[byte(r)&0x0f])
				continue
			}
			dst = utf8.AppendRune(dst, r)
		}
	}
	return append(dst, '"'), nil
}

func compareUTF16(left, right []uint16) int {
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	for i := 0; i < limit; i++ {
		if left[i] < right[i] {
			return -1
		}
		if left[i] > right[i] {
			return 1
		}
	}
	switch {
	case len(left) < len(right):
		return -1
	case len(left) > len(right):
		return 1
	default:
		return 0
	}
}

func signedIntegerExactlyRepresentable(value int64) bool {
	if value >= 0 {
		return unsignedIntegerExactlyRepresentable(uint64(value))
	}
	// Avoid overflowing on MinInt64.
	magnitude := uint64(-(value + 1)) + 1
	return unsignedIntegerExactlyRepresentable(magnitude)
}

func unsignedIntegerExactlyRepresentable(value uint64) bool {
	width := bits.Len64(value)
	if width <= 53 {
		return true
	}
	shift := width - 53
	return value&((uint64(1)<<shift)-1) == 0
}
