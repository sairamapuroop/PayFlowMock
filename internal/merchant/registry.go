package merchant

import (
	"strings"

	"github.com/google/uuid"
)

// Registry resolves merchant webhook delivery targets and signing material.
type Registry interface {
	WebhookURL(merchantID uuid.UUID) (string, bool)
	Secret(merchantID uuid.UUID) (string, bool)
}

// EnvRegistry loads configuration from the process environment.
//
// MERCHANT_WEBHOOK_URLS: comma-separated "uuid=https://..." pairs (whitespace around pairs is trimmed).
// DEFAULT_WEBHOOK_URL: used when no entry exists for the merchant UUID.
// WEBHOOK_SIGNING_SECRET: shared HMAC secret returned for any merchant when non-empty.
type EnvRegistry struct {
	byMerchant    map[uuid.UUID]string
	defaultURL    string
	signingSecret string
}

// NewEnvRegistry builds an EnvRegistry using getenv (typically os.Getenv).
func NewEnvRegistry(getenv func(string) string) *EnvRegistry {
	return &EnvRegistry{
		byMerchant:    parseMerchantWebhookURLs(getenv("MERCHANT_WEBHOOK_URLS")),
		defaultURL:    strings.TrimSpace(getenv("DEFAULT_WEBHOOK_URL")),
		signingSecret: getenv("WEBHOOK_SIGNING_SECRET"),
	}
}

// WebhookURL returns a merchant-specific URL from MERCHANT_WEBHOOK_URLS, else DEFAULT_WEBHOOK_URL.
func (r *EnvRegistry) WebhookURL(merchantID uuid.UUID) (string, bool) {
	if r == nil {
		return "", false
	}
	if u, ok := r.byMerchant[merchantID]; ok && strings.TrimSpace(u) != "" {
		return strings.TrimSpace(u), true
	}
	if strings.TrimSpace(r.defaultURL) != "" {
		return strings.TrimSpace(r.defaultURL), true
	}
	return "", false
}

// Secret returns WEBHOOK_SIGNING_SECRET when set; merchant ID is reserved for future per-merchant keys.
func (r *EnvRegistry) Secret(_ uuid.UUID) (string, bool) {
	if r == nil || strings.TrimSpace(r.signingSecret) == "" {
		return "", false
	}
	return r.signingSecret, true
}

func parseMerchantWebhookURLs(raw string) map[uuid.UUID]string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := make(map[uuid.UUID]string)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idx := strings.Index(part, "=")
		if idx <= 0 || idx == len(part)-1 {
			continue
		}
		idStr := strings.TrimSpace(part[:idx])
		urlStr := strings.TrimSpace(part[idx+1:])
		if urlStr == "" {
			continue
		}
		id, err := uuid.Parse(idStr)
		if err != nil {
			continue
		}
		out[id] = urlStr
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
