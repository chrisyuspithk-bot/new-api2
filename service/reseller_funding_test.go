package service

import (
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func openResellerFundingTestDB(t *testing.T) {
	t.Helper()
	var db *gorm.DB
	var err error
	switch dsn := os.Getenv("SQL_DSN"); {
	case strings.HasPrefix(dsn, "postgres://"):
		db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{})
		common.SetMainDatabaseType(common.DatabaseTypePostgreSQL)
	case dsn != "":
		db, err = gorm.Open(mysql.Open(dsn), &gorm.Config{})
		common.SetMainDatabaseType(common.DatabaseTypeMySQL)
	default:
		db, err = gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
		common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	}
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.ResellerWallet{}, &model.ProfitLedgerEntry{}, &model.ResellerCustomerMarkup{}))
	prevDB, prevRedis, prevBatch := model.DB, common.RedisEnabled, common.BatchUpdateEnabled
	t.Cleanup(func() {
		model.DB = prevDB
		common.RedisEnabled = prevRedis
		common.BatchUpdateEnabled = prevBatch
	})
	model.DB = db
	common.RedisEnabled = false
	common.BatchUpdateEnabled = false
	for _, table := range []any{&model.ResellerCustomerMarkup{}, &model.ProfitLedgerEntry{}, &model.ResellerWallet{}, &model.User{}} {
		require.NoError(t, db.Session(&gorm.Session{AllowGlobalUpdate: true}).Unscoped().Delete(table).Error)
	}
}

func setupResellerFundingUsers(t *testing.T, resellerCost, customerQuota int) (resellerId, customerId int) {
	t.Helper()
	openResellerFundingTestDB(t)
	require.NoError(t, model.DB.Create(&model.ResellerWallet{UserId: 7101, Balance: resellerCost}).Error)
	customer := model.User{Id: 7102, Username: "reseller-funding-cust", Password: "x", Role: 1, Status: 1, Group: "default", Quota: customerQuota, AuthVersion: 1, AffCode: "rf-aff-7102", ResellerId: 7101}
	require.NoError(t, model.DB.Create(&customer).Error)
	return 7101, 7102
}

func customerQuotaOf(t *testing.T, id int) int {
	t.Helper()
	var u model.User
	require.NoError(t, model.DB.Select("quota").First(&u, id).Error)
	return u.Quota
}

func resellerCostOf(t *testing.T, id int) int {
	t.Helper()
	var w model.ResellerWallet
	require.NoError(t, model.DB.Where("user_id = ?", id).First(&w).Error)
	return w.Balance
}

func TestResellerFundingSettlesBothBalancesAndRecordsProfit(t *testing.T) {
	resellerId, customerId := setupResellerFundingUsers(t, 1000, 1000)
	f := NewResellerWalletFunding(resellerId, customerId, 2.0)

	require.NoError(t, f.PreConsume(100))
	assert.Equal(t, 900, resellerCostOf(t, resellerId))
	assert.Equal(t, 800, customerQuotaOf(t, customerId))

	require.NoError(t, f.Settle(-30))
	assert.Equal(t, 930, resellerCostOf(t, resellerId))
	assert.Equal(t, 860, customerQuotaOf(t, customerId))

	var entries []model.ProfitLedgerEntry
	require.NoError(t, model.DB.Where("reseller_id = ?", resellerId).Find(&entries).Error)
	require.Len(t, entries, 1)
	assert.Equal(t, 70, entries[0].Cost)
	assert.Equal(t, 140, entries[0].Price)
	assert.Equal(t, 70, entries[0].Profit)
}

func TestResellerSessionCustomerFacingQuotaIsMarkedUpPrice(t *testing.T) {
	resellerId, customerId := setupResellerFundingUsers(t, 1000, 1000)
	require.NoError(t, model.DB.Create(&model.ResellerCustomerMarkup{
		ResellerId: resellerId, CustomerId: customerId, ModelName: "gpt-test", Markup: 1.5,
	}).Error)
	markup, err := model.GetCustomerMarkup(resellerId, customerId, "gpt-test")
	require.NoError(t, err)
	session := &BillingSession{funding: NewResellerWalletFunding(resellerId, customerId, markup)}

	require.NoError(t, session.funding.PreConsume(100))
	assert.Equal(t, 150, session.CustomerFacingQuota(100))
	assert.Equal(t, 100, (&BillingSession{funding: &WalletFunding{userId: customerId}}).CustomerFacingQuota(100))
}

func TestGetCustomerMarkupDefaultsToOne(t *testing.T) {
	resellerId, customerId := setupResellerFundingUsers(t, 1000, 1000)
	markup, err := model.GetCustomerMarkup(resellerId, customerId, "unset-model")
	require.NoError(t, err)
	assert.Equal(t, 1.0, markup)
}

func TestResellerFundingRejectsWhenResellerCostInsufficient(t *testing.T) {
	resellerId, customerId := setupResellerFundingUsers(t, 50, 1000)
	f := NewResellerWalletFunding(resellerId, customerId, 2.0)

	err := f.PreConsume(100)
	assert.ErrorIs(t, err, model.ErrResellerCostInsufficient)
	assert.Equal(t, 50, resellerCostOf(t, resellerId))
	assert.Equal(t, 1000, customerQuotaOf(t, customerId))
}

func TestResellerFundingReleasesCostWhenCustomerInsufficient(t *testing.T) {
	resellerId, customerId := setupResellerFundingUsers(t, 1000, 10)
	f := NewResellerWalletFunding(resellerId, customerId, 2.0)

	err := f.PreConsume(100)
	assert.ErrorIs(t, err, ErrInsufficientWalletQuota)
	assert.Equal(t, 1000, resellerCostOf(t, resellerId))
	assert.Equal(t, 10, customerQuotaOf(t, customerId))
}

func TestResellerFundingRefundReturnsBothBalances(t *testing.T) {
	resellerId, customerId := setupResellerFundingUsers(t, 1000, 1000)
	f := NewResellerWalletFunding(resellerId, customerId, 1.5)

	require.NoError(t, f.PreConsume(200))
	require.NoError(t, f.Refund())
	assert.Equal(t, 1000, resellerCostOf(t, resellerId))
	assert.Equal(t, 1000, customerQuotaOf(t, customerId))
}

func TestResellerFundingConcurrentReservesNeverOverdraw(t *testing.T) {
	resellerId, customerId := setupResellerFundingUsers(t, 300, 100000)
	const workers = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f := NewResellerWalletFunding(resellerId, customerId, 1.0)
			if err := f.PreConsume(50); err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, 6, successes)
	assert.Equal(t, 0, resellerCostOf(t, resellerId))
}
