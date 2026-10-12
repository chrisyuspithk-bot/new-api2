package model

import (
	"errors"
	"math"

	"github.com/QuantumNous/new-api/common"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

var (
	ErrResellerNotFound         = errors.New("reseller not found")
	ErrResellerCustomerNotFound = errors.New("reseller customer not found")
	ErrModelNotInResellerPolicy = errors.New("model is not available to this reseller")
	ErrInvalidBasePriceFactor   = errors.New("base price factor must be positive and finite")
	ErrInvalidMarkup            = errors.New("markup must be positive and finite")
)

// maxBillingMultiplier bounds configured multipliers so a typo cannot inflate a charge
// beyond a sane range (billing rules: every billing multiplier must be bounded).
const maxBillingMultiplier = 1000

// GetResellerUser returns the reseller account, failing when the user is not a reseller.
func GetResellerUser(resellerId int) (*User, error) {
	var user User
	err := DB.Where("id = ? AND role = ?", resellerId, common.RoleReseller).First(&user).Error
	if err != nil {
		return nil, ErrResellerNotFound
	}
	return &user, nil
}

// GetResellerCustomer returns a customer only when it belongs to the given reseller.
func GetResellerCustomer(resellerId, customerId int) (*User, error) {
	var user User
	err := DB.Where("id = ? AND reseller_id = ?", customerId, resellerId).First(&user).Error
	if err != nil {
		return nil, ErrResellerCustomerNotFound
	}
	return &user, nil
}

// UpsertResellerProfile sets the display name and contact details shown to a reseller's customers.
func UpsertResellerProfile(resellerId int, name string, contactInfo string) error {
	now := common.GetTimestamp()
	profile := ResellerProfile{UserId: resellerId, Name: name, ContactInfo: contactInfo, CreatedAt: now, UpdatedAt: now}
	return DB.Where("user_id = ?", resellerId).
		Assign(map[string]any{"name": name, "contact_info": contactInfo, "updated_at": now}).
		FirstOrCreate(&profile).Error
}

// CreditResellerCustomer adds quota to a reseller's own customer and debits the same amount from
// the reseller cost balance, in one transaction. A reseller with insufficient cost balance is
// rejected before the customer is credited.
func CreditResellerCustomer(resellerId, customerId, quota int) error {
	if quota <= 0 || quota > common.MaxWalletQuota {
		return ErrInvalidUserQuotaAdjustment
	}
	err := DB.Transaction(func(tx *gorm.DB) error {
		// Reserve the reseller cost first so an exhausted reseller never touches the customer row.
		if err := ReserveResellerCost(tx, resellerId, quota); err != nil {
			return err
		}
		var customer User
		if err := lockForUpdate(tx).Where("id = ? AND reseller_id = ?", customerId, resellerId).First(&customer).Error; err != nil {
			return err
		}
		next, err := common.WalletQuotaFromDecimalStrict(decimal.NewFromInt(int64(customer.Quota)).Add(decimal.NewFromInt(int64(quota))))
		if err != nil {
			return ErrWalletQuotaLimitExceeded
		}
		return tx.Model(&User{}).Where("id = ?", customerId).Update("quota", next).Error
	})
	if err != nil {
		return err
	}
	if err := cacheIncrUserQuota(customerId, int64(quota)); err != nil {
		common.SysError(common.LogText("failed to sync reseller manual credit for customer %d: %s", customerId, err))
	}
	return nil
}

// ListResellerCustomerTopUps returns the top-up history of a reseller's own customers only.
func ListResellerCustomerTopUps(resellerId int, pageInfo *common.PageInfo) ([]*TopUp, int64, error) {
	customerIds := DB.Model(&User{}).Select("id").Where("reseller_id = ?", resellerId)
	var total int64
	if err := DB.Model(&TopUp{}).Where("user_id IN (?)", customerIds).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var topups []*TopUp
	err := DB.Where("user_id IN (?)", customerIds).Order("id desc").
		Limit(pageInfo.GetPageSize()).Offset(pageInfo.GetStartIdx()).Find(&topups).Error
	return topups, total, err
}

func ListResellerPolicies(resellerId int) ([]ResellerModelPolicy, error) {
	var rows []ResellerModelPolicy
	err := DB.Where("reseller_id = ?", resellerId).Order("model_name asc").Find(&rows).Error
	return rows, err
}

func ListResellerCustomers(resellerId int) ([]User, error) {
	var users []User
	err := DB.Where("reseller_id = ?", resellerId).Order("id asc").Find(&users).Error
	return users, err
}

func ListCustomerModels(resellerId, customerId int) ([]ResellerCustomerModel, error) {
	var rows []ResellerCustomerModel
	err := DB.Where("reseller_id = ? AND customer_id = ?", resellerId, customerId).
		Order("model_name asc").Find(&rows).Error
	return rows, err
}

// SetResellerModelPolicy upserts a model's base price factor for a reseller. A nil factor
// removes the model from the reseller, which also revokes it from all of that reseller's customers.
func SetResellerModelPolicy(resellerId int, modelName string, factor *float64) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		if factor == nil {
			if err := tx.Where("reseller_id = ? AND model_name = ?", resellerId, modelName).
				Delete(&ResellerModelPolicy{}).Error; err != nil {
				return err
			}
			return tx.Where("reseller_id = ? AND model_name = ?", resellerId, modelName).
				Delete(&ResellerCustomerModel{}).Error
		}
		if err := validateMultiplier(*factor, ErrInvalidBasePriceFactor); err != nil {
			return err
		}
		now := common.GetTimestamp()
		policy := ResellerModelPolicy{ResellerId: resellerId, ModelName: modelName, BasePriceFactor: *factor, UpdatedAt: now}
		return tx.Where("reseller_id = ? AND model_name = ?", resellerId, modelName).
			Assign(map[string]any{"base_price_factor": *factor, "updated_at": now}).
			FirstOrCreate(&policy).Error
	})
}

// SetResellerCustomerModel grants or revokes a model for a customer. A grant is rejected unless
// the reseller's own policy includes the model, so a customer cannot exceed its reseller.
func SetResellerCustomerModel(resellerId, customerId int, modelName string, allowed bool) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		if !allowed {
			return tx.Where("reseller_id = ? AND customer_id = ? AND model_name = ?", resellerId, customerId, modelName).
				Delete(&ResellerCustomerModel{}).Error
		}
		var policyCount int64
		if err := tx.Model(&ResellerModelPolicy{}).
			Where("reseller_id = ? AND model_name = ?", resellerId, modelName).
			Count(&policyCount).Error; err != nil {
			return err
		}
		if policyCount == 0 {
			return ErrModelNotInResellerPolicy
		}
		now := common.GetTimestamp()
		row := ResellerCustomerModel{ResellerId: resellerId, CustomerId: customerId, ModelName: modelName, UpdatedAt: now}
		return tx.Where("reseller_id = ? AND customer_id = ? AND model_name = ?", resellerId, customerId, modelName).
			Assign(map[string]any{"updated_at": now}).
			FirstOrCreate(&row).Error
	})
}

// CustomerModelAllowed reports whether a reseller customer may call a model: the model must be
// granted to the customer and still present in the reseller's policy.
func CustomerModelAllowed(resellerId, customerId int, modelName string) (bool, error) {
	var grantCount int64
	if err := DB.Model(&ResellerCustomerModel{}).
		Where("reseller_id = ? AND customer_id = ? AND model_name = ?", resellerId, customerId, modelName).
		Count(&grantCount).Error; err != nil {
		return false, err
	}
	if grantCount == 0 {
		return false, nil
	}
	var policyCount int64
	err := DB.Model(&ResellerModelPolicy{}).
		Where("reseller_id = ? AND model_name = ?", resellerId, modelName).
		Count(&policyCount).Error
	return policyCount > 0, err
}

// ResellerModelFactor returns the base price factor for a reseller's model, and false when the
// reseller may not sell that model.
func ResellerModelFactor(resellerId int, modelName string) (float64, bool, error) {
	var policy ResellerModelPolicy
	err := DB.Where("reseller_id = ? AND model_name = ?", resellerId, modelName).First(&policy).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return policy.BasePriceFactor, true, nil
}

// SetResellerCustomerMarkup stores the markup a reseller charges one customer for one model.
// The customer may only be priced on a model it has been granted.
func SetResellerCustomerMarkup(resellerId, customerId int, modelName string, markup float64) error {
	if err := validateMultiplier(markup, ErrInvalidMarkup); err != nil {
		return err
	}
	allowed, err := CustomerModelAllowed(resellerId, customerId, modelName)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrModelNotInResellerPolicy
	}
	now := common.GetTimestamp()
	row := ResellerCustomerMarkup{ResellerId: resellerId, CustomerId: customerId, ModelName: modelName, Markup: markup, UpdatedAt: now}
	return DB.Where("reseller_id = ? AND customer_id = ? AND model_name = ?", resellerId, customerId, modelName).
		Assign(map[string]any{"markup": markup, "updated_at": now}).
		FirstOrCreate(&row).Error
}

func validateMultiplier(value float64, invalid error) error {
	if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) || value > maxBillingMultiplier {
		return invalid
	}
	return nil
}
