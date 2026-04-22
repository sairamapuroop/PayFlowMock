package psp

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// Adapter name constants for the bundled mocks; must match StripeMock.Name() / RazorpayMock.Name().
const (
	AdapterStripeMock   = "stripe_mock"
	AdapterRazorpayMock = "razorpay_mock"
)

// DefaultLargeAmountThreshold is the plan's "amount > 100000" bound in smallest currency units
// (e.g. cents, paise). Amounts strictly greater than this value match the large-transaction rule.
const DefaultLargeAmountThreshold int64 = 100000

var (
	// ErrNoRoute means no routing rule matched the given currency and amount.
	ErrNoRoute = errors.New("psp: no routing rule matched currency and amount")
)

// RoutingRule picks a PSP when amount is within [MinAmount, MaxAmount] (inclusive) and the
// request currency matches. If Currencies is empty, the rule matches any currency (wildcard).
// MaxAmount 0 means no upper bound (use math.MaxInt64).
type RoutingRule struct {
	Currencies []string
	MinAmount  int64
	MaxAmount  int64
	PSP        string
}

// Router performs first-match routing over ordered rules.
type Router struct {
	adapters map[string]PSPAdapter
	rules    []RoutingRule
}

// NewRouter returns a router that resolves adapters by name. Rules are evaluated in order;
// the first matching rule wins.
func NewRouter(adapters map[string]PSPAdapter, rules []RoutingRule) *Router {
	cp := make([]RoutingRule, len(rules))
	copy(cp, rules)
	return &Router{adapters: adapters, rules: cp}
}

// DefaultRoutingRules returns the Week 2 plan defaults:
//   - INR → Razorpay
//   - USD / EUR / GBP → Stripe
//   - amount > DefaultLargeAmountThreshold (any other currency) → Stripe
func DefaultRoutingRules() []RoutingRule {
	return []RoutingRule{
		{Currencies: []string{"INR"}, MinAmount: 0, MaxAmount: 0, PSP: AdapterRazorpayMock},
		{Currencies: []string{"USD", "EUR", "GBP"}, MinAmount: 0, MaxAmount: 0, PSP: AdapterStripeMock},
		{Currencies: nil, MinAmount: DefaultLargeAmountThreshold + 1, MaxAmount: 0, PSP: AdapterStripeMock},
	}
}

// DefaultRouter wires DefaultRoutingRules to the given Stripe and Razorpay adapters.
func DefaultRouter(stripe, razorpay PSPAdapter) *Router {
	adapters := map[string]PSPAdapter{
		stripe.Name():   stripe,
		razorpay.Name(): razorpay,
	}
	return NewRouter(adapters, DefaultRoutingRules())
}

// Select returns the PSP adapter for the normalized currency and amount (smallest unit).
func (r *Router) Select(currency string, amount int64) (PSPAdapter, error) {
	if r == nil {
		return nil, fmt.Errorf("psp: nil router")
	}
	cur := strings.ToUpper(strings.TrimSpace(currency))
	maxBound := func(ruleMax int64) int64 {
		if ruleMax == 0 {
			return math.MaxInt64
		}
		return ruleMax
	}
	for _, rule := range r.rules {
		maxAmt := maxBound(rule.MaxAmount)
		if amount < rule.MinAmount || amount > maxAmt {
			continue
		}
		if len(rule.Currencies) == 0 {
			return r.adapter(rule.PSP)
		}
		for _, c := range rule.Currencies {
			if strings.ToUpper(strings.TrimSpace(c)) == cur {
				return r.adapter(rule.PSP)
			}
		}
	}
	return nil, ErrNoRoute
}

func (r *Router) adapter(name string) (PSPAdapter, error) {
	a, ok := r.adapters[name]
	if !ok || a == nil {
		return nil, fmt.Errorf("psp: unknown adapter %q", name)
	}
	return a, nil
}
