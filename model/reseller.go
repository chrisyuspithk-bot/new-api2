package model

import (
	"errors"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

var ErrResellerCostInsufficient = errors.New("reseller cost balance insufficient")

// ResellerWallet holds the reseller's prepaid upstream cost balance in quota units (USD-equivalent).
// Invariant: Balance >= 0.
type ResellerWallet struct {
	Id        int   `json:"id"`
	UserId    int   `json:"user_id" gorm:"uniqueIndex;not null"`
	Balance   int   `json:"balance" gorm:"type:int;not null;default:0"`
	UpdatedAt int64 `json:"updated_at" gorm:"bigint"`
}

// ProfitLedgerEntry records one billed customer request: Cost is the Admin base charge
// deducted from the reseller's cost balance, Price is the amount charged to the customer.
// Profit = Price - Cost and may be negative.
type ProfitLedgerEntry struct {
	Id         int    `json:"id"`
	ResellerId int    `json:"reseller_id" gorm:"index;not null"`
	CustomerId int    `json:"customer_id" gorm:"index;not null"`
	RequestId  string `json:"request_id" gorm:"type:varchar(64);index"`
	ModelName  string `json:"model_name" gorm:"type:varchar(255)"`
	Cost       int    `json:"cost" gorm:"type:int;not null;default:0"`
	Price      int    `json:"price" gorm:"type:int;not null;default:0"`
	Profit     int    `json:"profit" gorm:"type:int;not null;default:0"`
	CreatedAt  int64  `json:"created_at" gorm:"bigint;index"`
}

// ReserveResellerCost atomically deducts cost from the reseller balance, failing without
// modification when the balance is insufficient.
func ReserveResellerCost(tx *gorm.DB, resellerId int, cost int) error {
	if cost < 0 {
		return errors.New("reseller cost cannot be negative")
	}
	if cost == 0 {
		return nil
	}
	result := tx.Model(&ResellerWallet{}).
		Where("user_id = ? AND balance >= ?", resellerId, cost).
		Update("balance", gorm.Expr("balance - ?", cost))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrResellerCostInsufficient
	}
	return nil
}

// CreditResellerCost returns cost to the reseller balance (refund or manual topup).
func CreditResellerCost(tx *gorm.DB, resellerId int, cost int) error {
	if cost < 0 {
		return errors.New("reseller credit cannot be negative")
	}
	if cost == 0 {
		return nil
	}
	return tx.Model(&ResellerWallet{}).
		Where("user_id = ?", resellerId).
		Update("balance", gorm.Expr("balance + ?", cost)).Error
}

// ResellerCustomerMarkup is the reseller's per-customer, per-model multiplier applied to the
// Admin base charge. Missing rows mean markup 1 (sell at base). Values below 1 are allowed.
type ResellerCustomerMarkup struct {
	Id         int     `json:"id"`
	ResellerId int     `json:"reseller_id" gorm:"uniqueIndex:idx_reseller_markup;not null"`
	CustomerId int     `json:"customer_id" gorm:"uniqueIndex:idx_reseller_markup;not null"`
	ModelName  string  `json:"model_name" gorm:"type:varchar(255);uniqueIndex:idx_reseller_markup;not null"`
	Markup     float64 `json:"markup" gorm:"not null;default:1"`
	UpdatedAt  int64   `json:"updated_at" gorm:"bigint"`
}

// GetCustomerMarkup returns the markup for a reseller's customer and model, or 1 when unset.
func GetCustomerMarkup(resellerId, customerId int, modelName string) (float64, error) {
	var row ResellerCustomerMarkup
	err := DB.Where("reseller_id = ? AND customer_id = ? AND model_name = ?", resellerId, customerId, modelName).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	return row.Markup, nil
}

// SettleResellerProfit adjusts the reseller's cost balance by the cost delta and records the
// request's profit row, atomically. costDelta is positive when more cost was actually incurred.
func SettleResellerProfit(resellerId int, entry ProfitLedgerEntry, costDelta int) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		if costDelta > 0 {
			if err := ReserveResellerCost(tx, resellerId, costDelta); err != nil {
				return err
			}
		} else if costDelta < 0 {
			if err := CreditResellerCost(tx, resellerId, -costDelta); err != nil {
				return err
			}
		}
		entry.ResellerId = resellerId
		entry.Profit = entry.Price - entry.Cost
		entry.CreatedAt = common.GetTimestamp()
		return tx.Create(&entry).Error
	})
}

// ResellerProfile is the Admin-maintained record for a reseller account (the account itself is a
// User with Role = RoleReseller). Name and contact are shown to the reseller's customers.
type ResellerProfile struct {
	Id          int    `json:"id"`
	UserId      int    `json:"user_id" gorm:"uniqueIndex;not null"`
	Name        string `json:"name" gorm:"type:varchar(128);not null;default:''"`
	ContactInfo string `json:"contact_info" gorm:"type:varchar(255);not null;default:''"`
	CreatedAt   int64  `json:"created_at" gorm:"bigint"`
	UpdatedAt   int64  `json:"updated_at" gorm:"bigint"`
}

// ResellerModelPolicy is the Admin-configured access and base price of one model for one reseller.
// A model with no row is not available to the reseller. BasePriceFactor scales the Admin base
// charge C for that reseller; it is positive, and 1 means the global base price.
type ResellerModelPolicy struct {
	Id              int     `json:"id"`
	ResellerId      int     `json:"reseller_id" gorm:"uniqueIndex:idx_reseller_model_policy;not null"`
	ModelName       string  `json:"model_name" gorm:"type:varchar(255);uniqueIndex:idx_reseller_model_policy;not null"`
	BasePriceFactor float64 `json:"base_price_factor" gorm:"not null;default:1"`
	UpdatedAt       int64   `json:"updated_at" gorm:"bigint"`
}

// ResellerCustomerModel grants one model to a reseller's customer. It must be a subset of the
// reseller's ResellerModelPolicy; the relay enforces both.
type ResellerCustomerModel struct {
	Id         int    `json:"id"`
	ResellerId int    `json:"reseller_id" gorm:"uniqueIndex:idx_reseller_customer_model;not null"`
	CustomerId int    `json:"customer_id" gorm:"uniqueIndex:idx_reseller_customer_model;not null"`
	ModelName  string `json:"model_name" gorm:"type:varchar(255);uniqueIndex:idx_reseller_customer_model;not null"`
	UpdatedAt  int64  `json:"updated_at" gorm:"bigint"`
}

// EnsureResellerWallet creates the wallet row for a reseller if it does not exist.
func EnsureResellerWallet(tx *gorm.DB, resellerId int) error {
	wallet := ResellerWallet{UserId: resellerId, UpdatedAt: common.GetTimestamp()}
	return tx.Where("user_id = ?", resellerId).FirstOrCreate(&wallet).Error
}
