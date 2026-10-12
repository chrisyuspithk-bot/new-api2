package helper

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func openResellerPriceTestDB(t *testing.T) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.ResellerModelPolicy{}))
	prevDB := model.DB
	t.Cleanup(func() { model.DB = prevDB })
	model.DB = db
}

func TestApplyResellerBaseFactorLeavesDirectCustomerUnchanged(t *testing.T) {
	openResellerPriceTestDB(t)
	info := &relaycommon.RelayInfo{ResellerId: 0}
	scaled, err := applyResellerBaseFactor(info, 1000)
	require.NoError(t, err)
	assert.Equal(t, 1000, scaled)
}

func TestApplyResellerBaseFactorScalesBaseCostByStoredFactor(t *testing.T) {
	openResellerPriceTestDB(t)
	factor := 1.5
	require.NoError(t, model.SetResellerModelPolicy(42, "gpt-test", &factor))
	info := &relaycommon.RelayInfo{ResellerId: 42}
	info.BillingModelName = "gpt-test"

	scaled, err := applyResellerBaseFactor(info, 1000)
	require.NoError(t, err)
	assert.Equal(t, 1500, scaled)
	assert.Nil(t, info.QuotaClamp)
}

func TestApplyResellerBaseFactorRefusesModelOutsideResellerPolicy(t *testing.T) {
	openResellerPriceTestDB(t)
	info := &relaycommon.RelayInfo{ResellerId: 42}
	info.BillingModelName = "not-sold"

	_, err := applyResellerBaseFactor(info, 1000)
	assert.ErrorIs(t, err, model.ErrModelNotInResellerPolicy)
}

func TestApplyResellerBaseFactorReportsSaturation(t *testing.T) {
	openResellerPriceTestDB(t)
	factor := 1000.0
	require.NoError(t, model.SetResellerModelPolicy(42, "gpt-huge", &factor))
	info := &relaycommon.RelayInfo{ResellerId: 42}
	info.BillingModelName = "gpt-huge"

	scaled, err := applyResellerBaseFactor(info, 1<<30)
	require.NoError(t, err)
	assert.Equal(t, common.MaxQuota, scaled)
	require.NotNil(t, info.QuotaClamp)
	assert.Equal(t, common.QuotaClampOverflow, info.QuotaClamp.Kind)
}
