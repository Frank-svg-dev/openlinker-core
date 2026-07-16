package externalexecution

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/OpenLinker-ai/openlinker-core/pkg/httpx"
)

const requiredRequestBindingClaimCount = 4

type RequestBinding struct {
	Version string
	Method  string
	Path    string
	BodySHA string
}

type verifiedServiceToken struct {
	claims             *serviceTokenVerificationClaims
	principal          *Principal
	expiresAt          time.Time
	binding            RequestBinding
	bindingClaimCount  int
	bindingValuesValid bool
}

func RequestBodySHA256(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func parseRequestBindingClaims(rawToken string) (RequestBinding, int, bool, error) {
	parts := strings.Split(strings.TrimSpace(rawToken), ".")
	if len(parts) != 3 {
		return RequestBinding{}, 0, false, errors.New("external execution service JWT format is invalid")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return RequestBinding{}, 0, false, err
	}
	var rawClaims map[string]json.RawMessage
	if err := json.Unmarshal(payload, &rawClaims); err != nil {
		return RequestBinding{}, 0, false, err
	}
	values := [...]*string{
		new(string),
		new(string),
		new(string),
		new(string),
	}
	keys := [...]string{
		"request_binding_version", "request_method", "request_path", "request_body_sha256",
	}
	present, valuesValid := 0, true
	for index, key := range keys {
		raw, ok := rawClaims[key]
		if !ok {
			continue
		}
		present++
		if err := json.Unmarshal(raw, values[index]); err != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			valuesValid = false
		}
	}
	return RequestBinding{
		Version: *values[0],
		Method:  *values[1],
		Path:    *values[2],
		BodySHA: *values[3],
	}, present, valuesValid, nil
}

func (a *Authorizer) validateRequestBinding(verified *verifiedServiceToken, request *http.Request, body []byte) error {
	if a == nil || verified == nil || verified.claims == nil || request == nil || request.URL == nil {
		return requestBindingInvalidError()
	}
	if verified.bindingClaimCount == 0 {
		if a.requireRequestBinding {
			return requestBindingInvalidError()
		}
		return nil
	}
	if verified.bindingClaimCount != requiredRequestBindingClaimCount {
		return requestBindingInvalidError()
	}

	binding := verified.binding
	actualMethod := request.Method
	actualPath := request.URL.EscapedPath()
	if !verified.bindingValuesValid ||
		binding.Version != RequestBindingVersionV1 ||
		binding.Method == "" || binding.Method != strings.ToUpper(binding.Method) ||
		binding.Method != actualMethod ||
		binding.Path == "" || binding.Path != actualPath ||
		!validLowerSHA256(binding.BodySHA) ||
		binding.BodySHA != RequestBodySHA256(body) {
		return requestBindingInvalidError()
	}
	return nil
}

func validLowerSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func requestBindingInvalidError() error {
	return httpx.NewError(
		http.StatusUnauthorized,
		httpx.ErrorCode(ErrorCodeRequestBindingInvalid),
		"外部执行请求绑定无效",
	)
}

func recordLegacyRequestBindingAccepted() {
	log.Warn().
		Str("event", "external_execution_legacy_request_binding_accepted").
		Str("binding_mode", "compatibility").
		Msg("accepted legacy external execution service credential")
}
