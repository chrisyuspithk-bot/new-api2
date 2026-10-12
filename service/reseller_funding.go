package service

import (
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
)

// ResellerWalletFunding bills a reseller-owned customer against two balances. The session
// works in Admin base-cost units C; the reseller cost balance is debited C and the customer
// wallet is debited the marked-up price P = round(C * markup). Reseller cost is reserved first,
// so an exhausted reseller balance rejects the request before the customer wallet is touched.
type ResellerWalletFunding struct {
	resellerId int
	customerId int
	markup     float64
	cost       int // cumulative reserved reseller cost C
	price      int // cumulative reserved customer price P
}

func NewResellerWalletFunding(resellerId, customerId int, markup float64) *ResellerWalletFunding {
	return &ResellerWalletFunding{resellerId: resellerId, customerId: customerId, markup: markup}
}

func (r *ResellerWalletFunding) Source() string { return BillingSourceWallet }

// newWalletFunding returns the funding for a request: reseller customers get the dual-balance
// funding with their per-model markup, everyone else keeps the plain customer wallet.
func newWalletFunding(relayInfo *relaycommon.RelayInfo) (FundingSource, error) {
	if relayInfo.ResellerId <= 0 {
		return &WalletFunding{userId: relayInfo.UserId}, nil
	}
	markup, err := model.GetCustomerMarkup(relayInfo.ResellerId, relayInfo.UserId, relayInfo.GetBillingModelName())
	if err != nil {
		return nil, err
	}
	return NewResellerWalletFunding(relayInfo.ResellerId, relayInfo.UserId, markup), nil
}

func (r *ResellerWalletFunding) priceFor(cost int) int {
	price, clamp := common.QuotaRoundChecked(float64(cost) * r.markup)
	if clamp != nil {
		common.SysError(fmt.Sprintf("reseller markup price clamped (resellerId=%d, customerId=%d, kind=%s)", r.resellerId, r.customerId, clamp.Kind))
	}
	return price
}

// PreConsume reserves cost C from the reseller, then price P from the customer, and releases
// the cost reservation if the customer reservation fails.
func (r *ResellerWalletFunding) PreConsume(amount int) error {
	return r.reserve(amount)
}

// reserve atomically adds a cost increment. Both balances must succeed or neither changes.
func (r *ResellerWalletFunding) reserve(costDelta int) error {
	if costDelta <= 0 {
		return nil
	}
	if err := model.ReserveResellerCost(model.DB, r.resellerId, costDelta); err != nil {
		return err
	}
	priceDelta := r.priceFor(r.cost+costDelta) - r.price
	if err := r.reserveCustomer(priceDelta); err != nil {
		r.releaseCost(costDelta)
		return err
	}
	r.cost += costDelta
	r.price += priceDelta
	return nil
}

// reserveCustomer atomically reserves the customer price increment; it never lets the
// customer balance go negative, matching the pre-consume guarantee of the plain wallet.
func (r *ResellerWalletFunding) reserveCustomer(priceDelta int) error {
	if priceDelta <= 0 {
		return nil
	}
	reserved, err := model.TryReserveUserQuota(r.customerId, priceDelta)
	if err != nil {
		return err
	}
	if !reserved {
		return ErrInsufficientWalletQuota
	}
	return nil
}

// Settle applies the actual cost delta to both balances and records one profit row.
func (r *ResellerWalletFunding) Settle(costDelta int) error {
	newCost := r.cost + costDelta
	newPrice := r.priceFor(newCost)
	priceDelta := newPrice - r.price
	if priceDelta > 0 {
		if err := model.DecreaseUserQuota(r.customerId, priceDelta, false); err != nil {
			return err
		}
	} else if priceDelta < 0 {
		if err := model.IncreaseUserQuota(r.customerId, -priceDelta, false); err != nil {
			return err
		}
	}
	entry := model.ProfitLedgerEntry{CustomerId: r.customerId, Cost: newCost, Price: newPrice}
	if err := model.SettleResellerProfit(r.resellerId, entry, costDelta); err != nil {
		return err
	}
	r.cost = newCost
	r.price = newPrice
	return nil
}

// Refund returns both reservations.
func (r *ResellerWalletFunding) Refund() error {
	if r.price > 0 {
		if err := model.IncreaseUserQuota(r.customerId, r.price, false); err != nil {
			return err
		}
	}
	r.releaseCost(r.cost)
	return nil
}

// CustomerQuota is the marked-up price the customer is charged; the base cost stays internal.
func (r *ResellerWalletFunding) CustomerQuota() int { return r.price }

// rollback undoes a reserve increment of costDelta, returning both balances.
func (r *ResellerWalletFunding) rollback(costDelta int) error {
	priceDelta := r.priceFor(r.cost) - r.priceFor(r.cost-costDelta)
	if priceDelta > 0 {
		if err := model.IncreaseUserQuota(r.customerId, priceDelta, false); err != nil {
			return err
		}
	}
	if err := model.CreditResellerCost(model.DB, r.resellerId, costDelta); err != nil {
		return err
	}
	r.cost -= costDelta
	r.price -= priceDelta
	return nil
}

func (r *ResellerWalletFunding) releaseCost(cost int) {
	if cost <= 0 {
		return
	}
	if err := model.CreditResellerCost(model.DB, r.resellerId, cost); err != nil {
		common.SysLog(common.LogText("failed to release reseller cost reserve (resellerId=%d, cost=%d): %s", r.resellerId, cost, err.Error()))
		return
	}
	r.cost -= cost
}
