package model

import (
	"fmt"
	"math"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResellerCostReserveNeverOverdraws(t *testing.T) {
	require.NoError(t, DB.Create(&ResellerWallet{UserId: 9001, Balance: 100}).Error)

	require.NoError(t, ReserveResellerCost(DB, 9001, 60))
	assert.ErrorIs(t, ReserveResellerCost(DB, 9001, 60), ErrResellerCostInsufficient)

	var wallet ResellerWallet
	require.NoError(t, DB.Where("user_id = ?", 9001).First(&wallet).Error)
	assert.Equal(t, 40, wallet.Balance)

	require.NoError(t, CreditResellerCost(DB, 9001, 60))
	require.NoError(t, DB.Where("user_id = ?", 9001).First(&wallet).Error)
	assert.Equal(t, 100, wallet.Balance)
}

func TestEnsureResellerWalletIsIdempotent(t *testing.T) {
	require.NoError(t, EnsureResellerWallet(DB, 9002))
	require.NoError(t, EnsureResellerWallet(DB, 9002))

	var count int64
	require.NoError(t, DB.Model(&ResellerWallet{}).Where("user_id = ?", 9002).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestResellerCustomerGrantRequiresResellerPolicy(t *testing.T) {
	const resellerId, customerId = 9010, 9011
	factor := 1.5
	require.NoError(t, SetResellerModelPolicy(resellerId, "gpt-policy", &factor))

	require.NoError(t, SetResellerCustomerModel(resellerId, customerId, "gpt-policy", true))
	allowed, err := CustomerModelAllowed(resellerId, customerId, "gpt-policy")
	require.NoError(t, err)
	assert.True(t, allowed)

	assert.ErrorIs(t, SetResellerCustomerModel(resellerId, customerId, "not-sold", true), ErrModelNotInResellerPolicy)
}

func TestRevokingResellerModelRemovesCustomerAccess(t *testing.T) {
	const resellerId, customerId = 9020, 9021
	factor := 1.0
	require.NoError(t, SetResellerModelPolicy(resellerId, "gpt-revoke", &factor))
	require.NoError(t, SetResellerCustomerModel(resellerId, customerId, "gpt-revoke", true))

	require.NoError(t, SetResellerModelPolicy(resellerId, "gpt-revoke", nil))

	allowed, err := CustomerModelAllowed(resellerId, customerId, "gpt-revoke")
	require.NoError(t, err)
	assert.False(t, allowed)
	rows, err := ListCustomerModels(resellerId, customerId)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestResellerMarkupRejectsModelOutsideGrant(t *testing.T) {
	const resellerId, customerId = 9030, 9031
	factor := 1.0
	require.NoError(t, SetResellerModelPolicy(resellerId, "gpt-markup", &factor))

	assert.ErrorIs(t, SetResellerCustomerMarkup(resellerId, customerId, "gpt-markup", 2), ErrModelNotInResellerPolicy)

	require.NoError(t, SetResellerCustomerModel(resellerId, customerId, "gpt-markup", true))
	require.NoError(t, SetResellerCustomerMarkup(resellerId, customerId, "gpt-markup", 2))
	markup, err := GetCustomerMarkup(resellerId, customerId, "gpt-markup")
	require.NoError(t, err)
	assert.Equal(t, 2.0, markup)
}

func setupCreditFixture(t *testing.T, resellerId, customerId, resellerCost, customerQuota, customerOwner int) {
	t.Helper()
	require.NoError(t, DB.Create(&ResellerWallet{UserId: resellerId, Balance: resellerCost}).Error)
	require.NoError(t, DB.Create(&User{Id: customerId, Username: fmt.Sprintf("credit-cust-%d", customerId), Password: "x", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", Quota: customerQuota, AffCode: fmt.Sprintf("credit-aff-%d", customerId), ResellerId: customerOwner}).Error)
}

func customerQuotaOf(t *testing.T, userId int) int {
	t.Helper()
	var user User
	require.NoError(t, DB.Select("quota").First(&user, userId).Error)
	return user.Quota
}

func resellerCostOf(t *testing.T, resellerId int) int {
	t.Helper()
	var wallet ResellerWallet
	require.NoError(t, DB.Where("user_id = ?", resellerId).First(&wallet).Error)
	return wallet.Balance
}

func TestCreditResellerCustomerDebitsResellerCostOneToOne(t *testing.T) {
	setupCreditFixture(t, 9101, 9102, 500, 100, 9101)

	require.NoError(t, CreditResellerCustomer(9101, 9102, 200))

	assert.Equal(t, 300, resellerCostOf(t, 9101))
	assert.Equal(t, 300, customerQuotaOf(t, 9102))
}

func TestCreditResellerCustomerRejectsWhenResellerCostInsufficient(t *testing.T) {
	setupCreditFixture(t, 9111, 9112, 50, 100, 9111)

	assert.ErrorIs(t, CreditResellerCustomer(9111, 9112, 200), ErrResellerCostInsufficient)

	assert.Equal(t, 50, resellerCostOf(t, 9111))
	assert.Equal(t, 100, customerQuotaOf(t, 9112))
}

func TestCreditResellerCustomerRefusesCustomerOfAnotherReseller(t *testing.T) {
	setupCreditFixture(t, 9121, 9122, 500, 100, 9999)

	assert.Error(t, CreditResellerCustomer(9121, 9122, 200))

	assert.Equal(t, 500, resellerCostOf(t, 9121))
	assert.Equal(t, 100, customerQuotaOf(t, 9122))
}

func TestResellerMultipliersMustBeBoundedAndFinite(t *testing.T) {
	const resellerId, customerId = 9040, 9041
	for _, bad := range []float64{0, -1, math.NaN(), math.Inf(1), maxBillingMultiplier + 1} {
		assert.ErrorIs(t, SetResellerModelPolicy(resellerId, "gpt-bad", &bad), ErrInvalidBasePriceFactor)
		assert.ErrorIs(t, SetResellerCustomerMarkup(resellerId, customerId, "gpt-bad", bad), ErrInvalidMarkup)
	}
}
