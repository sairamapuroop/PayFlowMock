package repository

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sairamapuroop/PayFlowMock/internal/domain"
	"github.com/sairamapuroop/PayFlowMock/internal/merchant"
	"github.com/sairamapuroop/PayFlowMock/internal/testutil"
)

func newTestPaymentRepo(t *testing.T) *PaymentRepo {
	t.Helper()
	pool := testutil.MustPool(t)
	testutil.MustMigrateUp(t, pool)
	testutil.Truncate(t, pool)
	outbox := NewOutboxRepo(pool)
	reg := merchant.NewEnvRegistry(func(string) string { return "" })
	return NewPaymentRepo(pool, outbox, reg)
}

func TestCreatePayment_HappyPath(t *testing.T) {
	ctx := context.Background()
	repo := newTestPaymentRepo(t)
	merchantID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	p := testutil.NewPayment(t, merchantID, "idem-happy-1")

	if err := repo.CreatePayment(ctx, p); err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	got, err := repo.GetPaymentByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetPaymentByID: %v", err)
	}
	if got.ID != p.ID {
		t.Fatalf("ID: got %v want %v", got.ID, p.ID)
	}
	if got.MerchantID != merchantID {
		t.Fatalf("MerchantID: got %v want %v", got.MerchantID, merchantID)
	}
	if got.Amount.Cmp(&p.Amount) != 0 {
		t.Fatalf("Amount: got %v want %v", got.Amount.String(), p.Amount.String())
	}
	if got.Currency != "USD" {
		t.Fatalf("Currency: got %q", got.Currency)
	}
	if got.Status != domain.StatusInitiated {
		t.Fatalf("Status: got %q want initiated", got.Status)
	}
	if got.IdempotencyKey != "idem-happy-1" {
		t.Fatalf("IdempotencyKey: got %q", got.IdempotencyKey)
	}
	if got.PSP != "" || got.PSPReferenceID != "" || got.FailureReason != "" {
		t.Fatalf("unexpected PSP fields: psp=%q ref=%q reason=%q", got.PSP, got.PSPReferenceID, got.FailureReason)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatal("timestamps should be set")
	}
}

func TestCreatePayment_DuplicateIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	repo := newTestPaymentRepo(t)
	merchantID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	key := "idem-dup-1"

	p1 := testutil.NewPayment(t, merchantID, key)
	if err := repo.CreatePayment(ctx, p1); err != nil {
		t.Fatalf("first CreatePayment: %v", err)
	}

	p2 := testutil.NewPayment(t, merchantID, key)
	err := repo.CreatePayment(ctx, p2)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("second CreatePayment: got %v want %v", err, ErrConflict)
	}
}

func TestCreatePayment_Validation(t *testing.T) {
	ctx := context.Background()
	merchantID := uuid.MustParse("33333333-3333-3333-3333-333333333333")

	tooBig := new(big.Int)
	tooBig.SetString("9223372036854775808", 10) // > max int64

	tests := []struct {
		name    string
		payment func(t *testing.T) *domain.Payment
		wantErr error
	}{
		{
			name: "nil payment",
			payment: func(t *testing.T) *domain.Payment {
				t.Helper()
				return nil
			},
			wantErr: ErrInvalidPayment,
		},
		{
			name: "missing payment id",
			payment: func(t *testing.T) *domain.Payment {
				t.Helper()
				p := testutil.NewPayment(t, merchantID, "v-id")
				p.ID = uuid.Nil
				return p
			},
			wantErr: ErrInvalidPayment,
		},
		{
			name: "missing merchant id",
			payment: func(t *testing.T) *domain.Payment {
				t.Helper()
				p := testutil.NewPayment(t, merchantID, "v-merchant")
				p.MerchantID = uuid.Nil
				return p
			},
			wantErr: ErrInvalidPayment,
		},
		{
			name: "blank idempotency key",
			payment: func(t *testing.T) *domain.Payment {
				t.Helper()
				p := testutil.NewPayment(t, merchantID, "   ")
				return p
			},
			wantErr: ErrInvalidIdempotencyKey,
		},
		{
			name: "idempotency key too long",
			payment: func(t *testing.T) *domain.Payment {
				t.Helper()
				return testutil.NewPayment(t, merchantID, strings.Repeat("x", 256))
			},
			wantErr: ErrInvalidIdempotencyKey,
		},
		{
			name: "non-positive amount",
			payment: func(t *testing.T) *domain.Payment {
				t.Helper()
				p := testutil.NewPayment(t, merchantID, "v-amt0")
				p.Amount = *big.NewInt(0)
				return p
			},
			wantErr: ErrInvalidAmount,
		},
		{
			name: "amount does not fit int64",
			payment: func(t *testing.T) *domain.Payment {
				t.Helper()
				p := testutil.NewPayment(t, merchantID, "v-big")
				p.Amount = *tooBig
				return p
			},
			wantErr: ErrInvalidAmount,
		},
		{
			name: "currency wrong length",
			payment: func(t *testing.T) *domain.Payment {
				t.Helper()
				p := testutil.NewPayment(t, merchantID, "v-cur-len")
				p.Currency = "US"
				return p
			},
			wantErr: ErrInvalidPayment,
		},
		{
			name: "unsupported currency",
			payment: func(t *testing.T) *domain.Payment {
				t.Helper()
				p := testutil.NewPayment(t, merchantID, "v-cur-xyz")
				p.Currency = "XYZ"
				return p
			},
			wantErr: ErrInvalidPayment,
		},
		{
			name: "initial status must be initiated or empty",
			payment: func(t *testing.T) *domain.Payment {
				t.Helper()
				p := testutil.NewPayment(t, merchantID, "v-st")
				p.Status = domain.StatusProcessing
				return p
			},
			wantErr: ErrInvalidPayment,
		},
		{
			name: "unknown status",
			payment: func(t *testing.T) *domain.Payment {
				t.Helper()
				p := testutil.NewPayment(t, merchantID, "v-unknown-st")
				p.Status = domain.Status("not-a-status")
				return p
			},
			wantErr: ErrInvalidPayment,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newTestPaymentRepo(t)
			p := tt.payment(t)
			err := repo.CreatePayment(ctx, p)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("CreatePayment: got %v want %v", err, tt.wantErr)
			}
		})
	}
}

func TestGetPaymentByID_NotFound(t *testing.T) {
	ctx := context.Background()
	repo := newTestPaymentRepo(t)
	_, err := repo.GetPaymentByID(ctx, uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetPaymentByID: got %v want %v", err, ErrNotFound)
	}
}

func TestGetPaymentByID_RequiresID(t *testing.T) {
	ctx := context.Background()
	repo := newTestPaymentRepo(t)
	_, err := repo.GetPaymentByID(ctx, uuid.Nil)
	if !errors.Is(err, ErrInvalidPayment) {
		t.Fatalf("GetPaymentByID: got %v want %v", err, ErrInvalidPayment)
	}
}

func TestUpdatePaymentStatus_Transitions(t *testing.T) {
	ctx := context.Background()

	t.Run("initiated to processing", func(t *testing.T) {
		repo := newTestPaymentRepo(t)
		p := testutil.NewPayment(t, uuid.MustParse("44444444-4444-4444-4444-444444444444"), "u-1")
		if err := repo.CreatePayment(ctx, p); err != nil {
			t.Fatalf("CreatePayment: %v", err)
		}
		if err := repo.UpdatePaymentStatus(ctx, p.ID, domain.StatusInitiated, domain.StatusProcessing); err != nil {
			t.Fatalf("UpdatePaymentStatus: %v", err)
		}
		got, err := repo.GetPaymentByID(ctx, p.ID)
		if err != nil {
			t.Fatalf("GetPaymentByID: %v", err)
		}
		if got.Status != domain.StatusProcessing {
			t.Fatalf("status: got %q want processing", got.Status)
		}
	})

	t.Run("invalid skip initiated to success", func(t *testing.T) {
		repo := newTestPaymentRepo(t)
		p := testutil.NewPayment(t, uuid.MustParse("88888888-8888-8888-8888-888888888888"), "u-5")
		if err := repo.CreatePayment(ctx, p); err != nil {
			t.Fatalf("CreatePayment: %v", err)
		}
		err := repo.UpdatePaymentStatus(ctx, p.ID, domain.StatusInitiated, domain.StatusSuccess)
		if !errors.Is(err, ErrInvalidStatusTransition) {
			t.Fatalf("UpdatePaymentStatus: got %v want %v", err, ErrInvalidStatusTransition)
		}
	})

	t.Run("status mismatch wrong fromStatus", func(t *testing.T) {
		repo := newTestPaymentRepo(t)
		p := testutil.NewPayment(t, uuid.MustParse("99999999-9999-9999-9999-999999999999"), "u-6")
		if err := repo.CreatePayment(ctx, p); err != nil {
			t.Fatalf("CreatePayment: %v", err)
		}
		if err := repo.UpdatePaymentStatus(ctx, p.ID, domain.StatusInitiated, domain.StatusProcessing); err != nil {
			t.Fatalf("to processing: %v", err)
		}
		err := repo.UpdatePaymentStatus(ctx, p.ID, domain.StatusInitiated, domain.StatusSuccess)
		if !errors.Is(err, ErrStatusMismatch) {
			t.Fatalf("UpdatePaymentStatus: got %v want %v", err, ErrStatusMismatch)
		}
	})

	t.Run("not found", func(t *testing.T) {
		repo := newTestPaymentRepo(t)
		id := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
		err := repo.UpdatePaymentStatus(ctx, id, domain.StatusInitiated, domain.StatusProcessing)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("UpdatePaymentStatus: got %v want %v", err, ErrNotFound)
		}
	})

	t.Run("same status is no-op", func(t *testing.T) {
		repo := newTestPaymentRepo(t)
		p := testutil.NewPayment(t, uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"), "u-7")
		if err := repo.CreatePayment(ctx, p); err != nil {
			t.Fatalf("CreatePayment: %v", err)
		}
		if err := repo.UpdatePaymentStatus(ctx, p.ID, domain.StatusInitiated, domain.StatusInitiated); err != nil {
			t.Fatalf("no-op: %v", err)
		}
	})
}
