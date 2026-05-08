package psp

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type stubAdapter struct {
	name string
}

func (s *stubAdapter) Name() string { return s.name }

func (s *stubAdapter) Charge(context.Context, ChargeRequest) (*ChargeResponse, error) {
	return nil, errors.New("stub charge")
}

func (s *stubAdapter) Refund(context.Context, RefundRequest) (*RefundResponse, error) {
	return nil, errors.New("stub refund")
}

func TestDefaultRoutingRules_INRToRazorpay(t *testing.T) {
	stripe := &stubAdapter{name: AdapterStripeMock}
	rzp := &stubAdapter{name: AdapterRazorpayMock}
	r := NewRouter(map[string]PSPAdapter{
		AdapterStripeMock:   stripe,
		AdapterRazorpayMock: rzp,
	}, DefaultRoutingRules())

	got, err := r.Select("INR", 1)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got != rzp {
		t.Fatalf("expected razorpay adapter, got %v", got)
	}
}

func TestDefaultRoutingRules_StripeCurrencies(t *testing.T) {
	stripe := &stubAdapter{name: AdapterStripeMock}
	rzp := &stubAdapter{name: AdapterRazorpayMock}
	r := NewRouter(map[string]PSPAdapter{
		AdapterStripeMock:   stripe,
		AdapterRazorpayMock: rzp,
	}, DefaultRoutingRules())

	for _, cur := range []string{"USD", "EUR", "GBP", " usd ", "eur"} {
		t.Run(cur, func(t *testing.T) {
			got, err := r.Select(cur, 50_000)
			if err != nil {
				t.Fatalf("Select(%q): %v", cur, err)
			}
			if got != stripe {
				t.Fatalf("expected stripe adapter for %q", cur)
			}
		})
	}
}

func TestDefaultRoutingRules_LargeAmountNonINRUsesStripe(t *testing.T) {
	stripe := &stubAdapter{name: AdapterStripeMock}
	rzp := &stubAdapter{name: AdapterRazorpayMock}
	r := NewRouter(map[string]PSPAdapter{
		AdapterStripeMock:   stripe,
		AdapterRazorpayMock: rzp,
	}, DefaultRoutingRules())

	// Strictly greater than DefaultLargeAmountThreshold
	got, err := r.Select("JPY", DefaultLargeAmountThreshold+1)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got != stripe {
		t.Fatal("large JPY amount should route to stripe")
	}

	_, err = r.Select("JPY", DefaultLargeAmountThreshold)
	if !errors.Is(err, ErrNoRoute) {
		t.Fatalf("at threshold should not match large rule; got err=%v", err)
	}
}

func TestRouter_Select_NoRoute(t *testing.T) {
	stripe := &stubAdapter{name: AdapterStripeMock}
	rzp := &stubAdapter{name: AdapterRazorpayMock}
	r := NewRouter(map[string]PSPAdapter{
		AdapterStripeMock:   stripe,
		AdapterRazorpayMock: rzp,
	}, DefaultRoutingRules())

	_, err := r.Select("CHF", 100)
	if !errors.Is(err, ErrNoRoute) {
		t.Fatalf("expected ErrNoRoute, got %v", err)
	}
}

func TestRouter_Select_NilRouter(t *testing.T) {
	var r *Router
	_, err := r.Select("USD", 1)
	if err == nil || !strings.Contains(err.Error(), "nil router") {
		t.Fatalf("expected nil router error, got %v", err)
	}
}

func TestRouter_Select_UnknownAdapter(t *testing.T) {
	r := NewRouter(map[string]PSPAdapter{}, DefaultRoutingRules())
	_, err := r.Select("USD", 100)
	if err == nil {
		t.Fatal("expected error for missing stripe adapter")
	}
	if !strings.Contains(err.Error(), "unknown adapter") {
		t.Fatalf("expected unknown adapter in error: %v", err)
	}
}

func TestRouter_RulesFirstMatchWins(t *testing.T) {
	stripe := &stubAdapter{name: AdapterStripeMock}
	rzp := &stubAdapter{name: AdapterRazorpayMock}
	// If INR rule were after a wildcard that matched all, first rule should win — here we only assert
	// default order keeps INR on Razorpay even when amount is huge.
	r := NewRouter(map[string]PSPAdapter{
		AdapterStripeMock:   stripe,
		AdapterRazorpayMock: rzp,
	}, DefaultRoutingRules())

	got, err := r.Select("INR", DefaultLargeAmountThreshold+999)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got != rzp {
		t.Fatal("INR should stay on Razorpay; large-amount rule must not run first")
	}
}

func TestRouter_NewRouterCopiesRules(t *testing.T) {
	stripe := &stubAdapter{name: AdapterStripeMock}
	rzp := &stubAdapter{name: AdapterRazorpayMock}
	rules := DefaultRoutingRules()
	r := NewRouter(map[string]PSPAdapter{
		AdapterStripeMock:   stripe,
		AdapterRazorpayMock: rzp,
	}, rules)
	rules[0].PSP = AdapterStripeMock // mutate caller slice

	got, err := r.Select("INR", 1)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got != rzp {
		t.Fatal("router should keep its own copy of rules")
	}
}
