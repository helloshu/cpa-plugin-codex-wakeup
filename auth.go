package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/helloshu/cpa-plugin-codex-wakeup/internal/host"
)

var (
	bearerPattern      = regexp.MustCompile(`(?i)\bBearer\s+[^\s,;]+`)
	secretJSONPattern  = regexp.MustCompile(`(?i)(["']?(?:access[_-]?token|refresh[_-]?token|id[_-]?token|api[_-]?key|authorization)["']?\s*[:=]\s*["']?)([^"'\s,;}]+)`)
	secretKVPattern    = regexp.MustCompile(`(?i)((?:access[_-]?token|refresh[_-]?token|id[_-]?token|api[_-]?key|authorization))(\s*[:=]\s*|\s+)([^\s,;}]+)`)
	unixPathPattern    = regexp.MustCompile(`(^|[\s"'(=])/(?:[^/\s"',;)]+/)+[^/\s"',;)]*`)
	windowsPathPattern = regexp.MustCompile(`(?i)(^|[\s"'(=])[a-z]:[\\/][^\s"',;)]*`)
	emailPattern       = regexp.MustCompile(`(?i)([A-Z0-9._%+\-]{1,64})@([A-Z0-9.\-]+\.[A-Z]{2,})`)
)

type codexCredential struct {
	AccessToken string
	AccountID   string
}

func eligibleAuth(entry host.AuthFile) bool {
	return monitorableAuth(entry) && !entry.Unavailable && !strings.EqualFold(strings.TrimSpace(entry.Status), "unavailable")
}

// Host unavailability can mean a temporary quota cooldown. Monitoring and
// quota-triggered probes must not depend on the host clearing that flag first.
func monitorableAuth(entry host.AuthFile) bool {
	if entry.RuntimeOnly || entry.Disabled {
		return false
	}
	status := strings.ToLower(strings.TrimSpace(entry.Status))
	if status == "disabled" {
		return false
	}
	provider, typ := strings.ToLower(strings.TrimSpace(entry.Provider)), strings.ToLower(strings.TrimSpace(entry.Type))
	if provider != "codex" || typ != "codex" {
		return false
	}
	source := strings.ToLower(strings.TrimSpace(entry.Source))
	if source != "file" {
		return false
	}
	return strings.TrimSpace(entry.AuthIndex) != ""
}

func parseCodexCredential(raw []byte) (codexCredential, error) {
	if len(raw) == 0 || len(raw) > 2<<20 {
		return codexCredential{}, errors.New("auth material is empty or too large")
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return codexCredential{}, errors.New("auth material is not valid JSON")
	}
	root, ok := value.(map[string]any)
	if !ok {
		return codexCredential{}, errors.New("auth material must be a JSON object")
	}
	access, _, idToken, accountID := credentialFields(root)
	if access == "" || looksLikeAPIKey(access) {
		return codexCredential{}, errors.New("OAuth access token is missing")
	}
	if accountID == "" && idToken != "" {
		accountID = accountIDFromJWT(idToken)
	}
	return codexCredential{AccessToken: access, AccountID: accountID}, nil
}

// credentialFields walks only JSON objects (not arbitrary encoded strings) so
// common CPA/Codex exports such as {tokens:{...}}, {oauth:{...}},
// {session:{...}} and {credentials:{...}} all resolve consistently.
func credentialFields(root map[string]any) (access, refresh, idToken, accountID string) {
	var walk func(map[string]any)
	walk = func(object map[string]any) {
		for key, raw := range object {
			canonical := canonicalKey(key)
			switch value := raw.(type) {
			case string:
				value = strings.TrimSpace(value)
				if value == "" {
					continue
				}
				switch canonical {
				case "accesstoken", "oauthaccesstoken":
					if access == "" {
						access = value
					}
				case "refreshtoken", "oauthrefreshtoken":
					if refresh == "" {
						refresh = value
					}
				case "idtoken":
					if idToken == "" {
						idToken = value
					}
				case "accountid", "chatgptaccountid", "workspaceid", "organizationid":
					if accountID == "" {
						accountID = value
					}
				}
			case map[string]any:
				walk(value)
			}
		}
	}
	walk(root)
	return access, refresh, idToken, accountID
}

func findString(root map[string]any, keys ...string) string {
	wanted := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		wanted[canonicalKey(key)] = struct{}{}
	}
	for key, value := range root {
		if _, ok := wanted[canonicalKey(key)]; ok {
			if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
	}
	return ""
}

func accountIDFromJWT(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		decoded, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return ""
	}
	var claims map[string]any
	if json.Unmarshal(decoded, &claims) != nil {
		return ""
	}
	if value := findString(claims, "chatgpt_account_id", "chatgptAccountId", "account_id", "accountId"); value != "" {
		return value
	}
	for key, value := range claims {
		if canonicalKey(key) == canonicalKey("https://api.openai.com/auth") {
			if auth, ok := value.(map[string]any); ok {
				return findString(auth, "chatgpt_account_id", "chatgptAccountId", "account_id", "accountId")
			}
		}
	}
	return ""
}

func canonicalKey(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "_", "")
	return strings.ReplaceAll(value, "-", "")
}
func looksLikeAPIKey(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(value, "sk-") || strings.HasPrefix(value, "key-")
}

func scrubError(err error) string {
	if err == nil {
		return ""
	}
	return sanitizeText(err.Error())
}

func sanitizeWithSecrets(text string, secrets ...string) string {
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[REDACTED]")
		}
	}
	return sanitizeText(text)
}

func sanitizeText(text string) string {
	text = bearerPattern.ReplaceAllString(text, "Bearer [REDACTED]")
	text = secretJSONPattern.ReplaceAllString(text, "$1[REDACTED]")
	text = secretKVPattern.ReplaceAllString(text, "$1$2[REDACTED]")
	text = unixPathPattern.ReplaceAllString(text, "$1[PATH]")
	text = windowsPathPattern.ReplaceAllString(text, "$1[PATH]")
	text = emailPattern.ReplaceAllStringFunc(text, func(value string) string {
		parts := strings.SplitN(value, "@", 2)
		if len(parts) != 2 {
			return "[EMAIL]"
		}
		local := parts[0]
		if len(local) > 2 {
			local = local[:1] + "***" + local[len(local)-1:]
		} else {
			local = "***"
		}
		return local + "@" + parts[1]
	})
	if len(text) > 480 {
		return text[:480] + "…"
	}
	return text
}

func truncateAndScrub(raw []byte, limit int) string {
	if len(raw) > limit {
		raw = raw[:limit]
	}
	return sanitizeText(string(raw))
}

func sanitizeValue(value any) any {
	switch item := value.(type) {
	case string:
		return sanitizeText(item)
	case []byte:
		return "[REDACTED_BYTES]"
	case map[string]any:
		out := make(map[string]any, len(item))
		for key, nested := range item {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "token") || strings.Contains(lower, "authorization") || strings.Contains(lower, "api_key") {
				out[key] = "[REDACTED]"
			} else {
				out[key] = sanitizeValue(nested)
			}
		}
		return out
	default:
		return value
	}
}

func maskEmail(email string) string {
	email = strings.TrimSpace(email)
	if email == "" || !strings.Contains(email, "@") {
		return ""
	}
	parts := strings.SplitN(email, "@", 2)
	local := parts[0]
	if len(local) <= 2 {
		local = "***"
	} else {
		local = local[:1] + "***" + local[len(local)-1:]
	}
	return local + "@" + parts[1]
}
func formatAuthError(entry host.AuthFile, err error) error {
	return fmt.Errorf("%s: %s", sanitizeText(firstNonEmpty(entry.Label, entry.Name, "account")), scrubError(err))
}
