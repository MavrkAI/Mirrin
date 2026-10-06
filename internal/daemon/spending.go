package daemon

import (
	"context"
	"fmt"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/config"
)

// Spending is the Spending page: the limits, this month's total and the
// payments, newest first.
func (d *Daemon) Spending(ctx context.Context) api.SpendingInfo {
	c := d.Config().Spending
	out := api.SpendingInfo{Currency: c.Currency, PerAction: c.PerActionLimit, Monthly: c.MonthlyLimit, Payments: []api.SpendingPayment{}}
	if out.Currency == "" {
		out.Currency = config.Default().Spending.Currency
	}
	if d.purse == nil {
		return out
	}
	out.MonthTotal = d.purse.MonthTotal(ctx)
	entries := d.purse.Entries(ctx)
	for i := len(entries) - 1; i >= 0 && len(out.Payments) < 100; i-- {
		e := entries[i]
		out.Payments = append(out.Payments, api.SpendingPayment{At: e.At, Amount: e.Amount, Currency: e.Currency, Merchant: e.Merchant, Purpose: e.Purpose})
	}
	return out
}

// SetSpendingLimits saves new limits; the purse reads them live, so they
// apply to the next payment without a restart. The change is audited.
func (d *Daemon) SetSpendingLimits(ctx context.Context, perAction, monthly float64) error {
	before := d.Config().Spending
	if err := d.UpdateConfig(func(c *config.Config) {
		c.Spending.PerActionLimit, c.Spending.MonthlyLimit = perAction, monthly
	}); err != nil {
		return err
	}
	d.store.Audit(ctx, "spending.limits", "", fmt.Sprintf("per payment %.2f → %.2f, monthly %.2f → %.2f",
		before.PerActionLimit, perAction, before.MonthlyLimit, monthly))
	return nil
}
