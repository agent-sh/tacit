package modelrun

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Failure carries provider metadata. Fallback never searches arbitrary error
// strings; adapters classify the provider's status or error code at the edge.
type Failure struct {
	Kind    string
	Status  int
	Code    string
	Message string
}

func (e *Failure) Error() string {
	return fmt.Sprintf("model %s (status %d, code %s): %s", e.Kind, e.Status, e.Code, e.Message)
}

var statusPattern = regexp.MustCompile(`(?i)\b(?:HTTP|API error|status(?: code)?)\s*[:=]?\s*(\d{3})\b`)
var providerCodePattern = regexp.MustCompile(`(?i)\b(overloaded_error|ThrottlingException|ServiceUnavailableException|ModelNotReadyException|rate_limit_error|authentication_error|AccessDeniedException|ValidationException|ResourceNotFoundException)\b`)

// ProviderFailure accepts structured status/code first and only recognizes
// documented CLI wrappers when those fields are absent.
func ProviderFailure(status int, code, message string) *Failure {
	if status == 0 {
		if m := statusPattern.FindStringSubmatch(message); m != nil {
			status, _ = strconv.Atoi(m[1])
		}
	}
	if code == "" {
		code = providerCodePattern.FindString(message)
	}
	kind := "provider"
	switch status {
	case 401:
		kind = "authentication"
	case 429, 503, 529:
		kind = "capacity"
	case 500, 502, 504:
		kind = "unavailable"
	case 400, 403, 404:
		kind = "request"
	}
	if status == 0 || status >= 500 {
		switch strings.ToLower(code) {
		case "overloaded_error", "throttlingexception", "serviceunavailableexception", "modelnotreadyexception", "rate_limit_error":
			kind = "capacity"
		case "authentication_error":
			if status == 0 {
				kind = "authentication"
			}
		case "accessdeniedexception", "validationexception", "resourcenotfoundexception":
			kind = "request"
		}
	}
	// Claude's status-less terminal notices. Match whole provider notices,
	// never model names or request validation prose containing these words.
	if status == 0 && code == "" {
		t := strings.TrimSpace(strings.ToLower(message))
		if strings.HasPrefix(t, "repeated 529 overloaded errors") || strings.HasPrefix(t, "api error: repeated 529 overloaded errors") || strings.HasPrefix(t, "opus is experiencing high load, please use /model") {
			kind = "capacity"
		}
	}
	return &Failure{kind, status, code, message}
}

func eventFailure(event map[string]any, message string) error {
	status := int(num(event, "api_error_status"))
	if status == 0 {
		status = int(num(event, "status"))
	}
	code := str(event, "code")
	inner := obj(event, "error")
	if status == 0 {
		status = int(num(inner, "status"))
	}
	if code == "" {
		code = str(inner, "code")
		if code == "" {
			code = str(inner, "type")
		}
	}
	// Codex can wrap the original provider JSON in a message string.
	var wrapped map[string]any
	if json.Unmarshal([]byte(strings.TrimSpace(message)), &wrapped) == nil && wrapped != nil {
		return eventFailure(wrapped, str(obj(wrapped, "error"), "message"))
	}
	return ProviderFailure(status, code, message)
}
