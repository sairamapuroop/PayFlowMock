package merchant

import (
	"testing"

	"github.com/google/uuid"
)

func TestEnvRegistry_WebhookURL(t *testing.T) {
	t.Parallel()

	merchantA := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	merchantB := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	tests := []struct {
		name         string
		env          map[string]string
		merchantID   uuid.UUID
		wantURL      string
		wantOK       bool
	}{
		{
			name: "per_merchant_wins_over_default",
			env: map[string]string{
				"MERCHANT_WEBHOOK_URLS": merchantA.String() + "=https://merchant.example/hook",
				"DEFAULT_WEBHOOK_URL":   "https://default.example/fallback",
			},
			merchantID: merchantA,
			wantURL:    "https://merchant.example/hook",
			wantOK:     true,
		},
		{
			name: "fallback_to_default_for_unknown_merchant",
			env: map[string]string{
				"MERCHANT_WEBHOOK_URLS": merchantA.String() + "=https://merchant.example/hook",
				"DEFAULT_WEBHOOK_URL":   "https://default.example/fallback",
			},
			merchantID: merchantB,
			wantURL:    "https://default.example/fallback",
			wantOK:     true,
		},
		{
			name: "no_url_when_no_entry_and_empty_default",
			env: map[string]string{
				"MERCHANT_WEBHOOK_URLS": merchantA.String() + "=https://merchant.example/hook",
			},
			merchantID: merchantB,
			wantURL:    "",
			wantOK:     false,
		},
		{
			name: "no_url_when_only_malformed_pairs_and_no_default",
			env: map[string]string{
				"MERCHANT_WEBHOOK_URLS": "not-valid-json-pair,https://no-equals.com,baduuid=https://x.test," +
					merchantA.String() + "=,,missing-key-with-no-value=",
			},
			merchantID: merchantA,
			wantURL:    "",
			wantOK:     false,
		},
		{
			name: "valid_pair_among_malformed_skipped_entries",
			env: map[string]string{
				"MERCHANT_WEBHOOK_URLS": "bad-pair,not-a-uuid=https://skip.example," +
					merchantA.String() + "=https://good.example/webhook,=" + merchantB.String() + "=only-default",
				"DEFAULT_WEBHOOK_URL": "https://default.example/",
			},
			merchantID: merchantA,
			wantURL:    "https://good.example/webhook",
			wantOK:     true,
		},
		{
			name: "whitespace_trimmed_around_uuid_equals_and_url",
			env: map[string]string{
				"MERCHANT_WEBHOOK_URLS": "  " + merchantA.String() + "  =  https://trimmed.example/z  ",
			},
			merchantID: merchantA,
			wantURL:    "https://trimmed.example/z",
			wantOK:     true,
		},
		{
			name: "blank_url_after_equals_skipped_so_default_used",
			env: map[string]string{
				"MERCHANT_WEBHOOK_URLS": merchantA.String() + "=",
				"DEFAULT_WEBHOOK_URL":   "https://default-after-blank.example/",
			},
			merchantID: merchantA,
			wantURL:    "https://default-after-blank.example/",
			wantOK:     true,
		},
		{
			name: "stored_url_trimmed_when_returned",
			env: map[string]string{
				"MERCHANT_WEBHOOK_URLS": merchantA.String() + "=  https://spaces.example  ",
			},
			merchantID: merchantA,
			wantURL:    "https://spaces.example",
			wantOK:     true,
		},
		{
			name: "default_url_trimmed",
			env: map[string]string{
				"DEFAULT_WEBHOOK_URL": "  https://default-trim.example  ",
			},
			merchantID: merchantB,
			wantURL:    "https://default-trim.example",
			wantOK:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			getenv := func(key string) string {
				if tt.env == nil {
					return ""
				}
				return tt.env[key]
			}
			r := NewEnvRegistry(getenv)

			gotURL, gotOK := r.WebhookURL(tt.merchantID)
			if gotOK != tt.wantOK || gotURL != tt.wantURL {
				t.Fatalf("WebhookURL(%s) = (%q, %v), want (%q, %v)", tt.merchantID, gotURL, gotOK, tt.wantURL, tt.wantOK)
			}
		})
	}

	t.Run("nil_receiver", func(t *testing.T) {
		t.Parallel()

		var r *EnvRegistry = nil
		gotURL, gotOK := r.WebhookURL(merchantA)
		if gotOK || gotURL != "" {
			t.Fatalf("nil.WebhookURL() = (%q, %v), want (\"\", false)", gotURL, gotOK)
		}
	})
}

func TestEnvRegistry_Secret(t *testing.T) {
	t.Parallel()

	mid := uuid.MustParse("33333333-3333-3333-3333-333333333333")

	tests := []struct {
		name    string
		env     map[string]string
		want    string
		wantOK  bool
	}{
		{
			name: "returns_secret_when_set",
			env: map[string]string{
				"WEBHOOK_SIGNING_SECRET": "super-secret",
			},
			want:   "super-secret",
			wantOK: true,
		},
		{
			name:   "false_when_missing",
			env:    map[string]string{},
			want:   "",
			wantOK: false,
		},
		{
			name: "false_when_empty_or_whitespace",
			env: map[string]string{
				"WEBHOOK_SIGNING_SECRET": "  \t  ",
			},
			want:   "",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			getenv := func(key string) string {
				if tt.env == nil {
					return ""
				}
				return tt.env[key]
			}
			r := NewEnvRegistry(getenv)

			got, ok := r.Secret(mid)
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("Secret(%s) = (%q, %v), want (%q, %v)", mid, got, ok, tt.want, tt.wantOK)
			}
		})
	}

	t.Run("nil_receiver", func(t *testing.T) {
		t.Parallel()

		var r *EnvRegistry = nil
		got, ok := r.Secret(mid)
		if ok || got != "" {
			t.Fatalf("nil.Secret() = (%q, %v), want (\"\", false)", got, ok)
		}
	})
}
