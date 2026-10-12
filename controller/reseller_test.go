package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resellerRequest drives a reseller handler with an authenticated reseller session, the same
// context ResellerAuth populates. The reseller id is never taken from the request body.
func resellerRequest(t *testing.T, method, body string, sessionUserId int, params gin.Params, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(method, "/api/user/reseller", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = params
	c.Set("id", sessionUserId)
	c.Set("role", common.RoleReseller)
	handler(c)
	return response
}

func createResellerTestUser(t *testing.T, username string, role int, resellerId int) *model.User {
	t.Helper()
	user := &model.User{Username: username, Password: "x", Role: role, Status: common.UserStatusEnabled, Group: "default", ResellerId: resellerId, AffCode: username + "-aff"}
	require.NoError(t, model.DB.Create(user).Error)
	return user
}

func TestResellerCannotManageCustomerOfAnotherReseller(t *testing.T) {
	setupSecurityEnrollmentTest(t)
	resellerA := createResellerTestUser(t, "reseller-a", common.RoleReseller, 0)
	resellerB := createResellerTestUser(t, "reseller-b", common.RoleReseller, 0)
	customerOfB := createResellerTestUser(t, "customer-of-b", common.RoleCommonUser, resellerB.Id)

	factor := 1.0
	require.NoError(t, model.SetResellerModelPolicy(resellerA.Id, "model-a", &factor))
	params := gin.Params{{Key: "id", Value: strconv.Itoa(customerOfB.Id)}}
	body := `{"model_name":"model-a","allowed":true}`

	response := resellerRequest(t, http.MethodPut, body, resellerA.Id, params, SetOwnCustomerModel)
	assert.JSONEq(t, `{"success":false,"message":"reseller customer not found"}`, response.Body.String())

	var grants int64
	require.NoError(t, model.DB.Model(&model.ResellerCustomerModel{}).Where("customer_id = ?", customerOfB.Id).Count(&grants).Error)
	assert.Equal(t, int64(0), grants)
}

func TestResellerCannotGrantModelOutsideOwnPolicy(t *testing.T) {
	setupSecurityEnrollmentTest(t)
	reseller := createResellerTestUser(t, "reseller-policy", common.RoleReseller, 0)
	customer := createResellerTestUser(t, "customer-policy", common.RoleCommonUser, reseller.Id)
	params := gin.Params{{Key: "id", Value: strconv.Itoa(customer.Id)}}

	response := resellerRequest(t, http.MethodPut, `{"model_name":"not-sold","allowed":true}`, reseller.Id, params, SetOwnCustomerModel)
	assert.Equal(t, http.StatusBadRequest, response.Code)
	assert.Contains(t, response.Body.String(), model.ErrModelNotInResellerPolicy.Error())
}

func TestResellerListsOnlyOwnCustomers(t *testing.T) {
	setupSecurityEnrollmentTest(t)
	reseller := createResellerTestUser(t, "reseller-list", common.RoleReseller, 0)
	other := createResellerTestUser(t, "reseller-other", common.RoleReseller, 0)
	mine := createResellerTestUser(t, "mine", common.RoleCommonUser, reseller.Id)
	createResellerTestUser(t, "theirs", common.RoleCommonUser, other.Id)

	response := resellerRequest(t, http.MethodGet, "", reseller.Id, nil, GetOwnCustomers)
	require.Equal(t, http.StatusOK, response.Code)
	var body struct {
		Data []model.User `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Len(t, body.Data, 1)
	assert.Equal(t, mine.Id, body.Data[0].Id)
}

func TestCreatedResellerAndCustomerAccountsAreEnabled(t *testing.T) {
	setupSecurityEnrollmentTest(t)
	reseller := createResellerTestUser(t, "reseller-created-by-admin", common.RoleReseller, 0)

	created := resellerRequest(t, http.MethodPost, `{"username":"new-reseller","password":"secret-pass","name":"Acme"}`, reseller.Id, nil, CreateReseller)
	require.Equal(t, http.StatusOK, created.Code)
	require.Contains(t, created.Body.String(), `"success":true`)
	var newReseller model.User
	require.NoError(t, model.DB.Where("username = ?", "new-reseller").First(&newReseller).Error)
	assert.Equal(t, common.UserStatusEnabled, newReseller.Status)
	assert.Equal(t, common.RoleReseller, newReseller.Role)

	customerResponse := resellerRequest(t, http.MethodPost, `{"username":"new-customer","password":"secret-pass"}`, reseller.Id, nil, CreateOwnCustomer)
	require.Equal(t, http.StatusOK, customerResponse.Code)
	var customer model.User
	require.NoError(t, model.DB.Where("username = ?", "new-customer").First(&customer).Error)
	assert.Equal(t, common.UserStatusEnabled, customer.Status)
	assert.Equal(t, reseller.Id, customer.ResellerId)
}

func TestResellerTopUpHistoryIsScopedToOwnCustomers(t *testing.T) {
	setupSecurityEnrollmentTest(t)
	resellerA := createResellerTestUser(t, "reseller-topup-a", common.RoleReseller, 0)
	resellerB := createResellerTestUser(t, "reseller-topup-b", common.RoleReseller, 0)
	customerA := createResellerTestUser(t, "customer-topup-a", common.RoleCommonUser, resellerA.Id)
	customerB := createResellerTestUser(t, "customer-topup-b", common.RoleCommonUser, resellerB.Id)
	directUser := createResellerTestUser(t, "direct-topup-user", common.RoleCommonUser, 0)

	for i, owner := range []int{customerA.Id, customerB.Id, directUser.Id} {
		require.NoError(t, model.DB.Create(&model.TopUp{UserId: owner, Amount: 10, Money: 10, TradeNo: "trade-" + strconv.Itoa(i), PaymentMethod: "stripe", CreateTime: common.GetTimestamp(), Status: "success"}).Error)
	}

	response := resellerRequest(t, http.MethodGet, "", resellerA.Id, nil, GetOwnCustomerTopUps)
	require.Equal(t, http.StatusOK, response.Code)
	var body struct {
		Data struct {
			Total int           `json:"total"`
			Items []model.TopUp `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Equal(t, 1, body.Data.Total)
	require.Len(t, body.Data.Items, 1)
	assert.Equal(t, customerA.Id, body.Data.Items[0].UserId)
}

func TestResellerCreditsOwnCustomerThroughHandler(t *testing.T) {
	setupSecurityEnrollmentTest(t)
	reseller := createResellerTestUser(t, "reseller-credit-own", common.RoleReseller, 0)
	require.NoError(t, model.DB.Create(&model.ResellerWallet{UserId: reseller.Id, Balance: 500}).Error)
	customer := createResellerTestUser(t, "customer-credit-own", common.RoleCommonUser, reseller.Id)
	params := gin.Params{{Key: "id", Value: strconv.Itoa(customer.Id)}}

	response := resellerRequest(t, http.MethodPost, `{"quota":200}`, reseller.Id, params, CreditOwnCustomer)

	require.Equal(t, http.StatusOK, response.Code)
	assert.Contains(t, response.Body.String(), `"success":true`)
	var wallet model.ResellerWallet
	require.NoError(t, model.DB.Where("user_id = ?", reseller.Id).First(&wallet).Error)
	assert.Equal(t, 300, wallet.Balance)
	var refreshed model.User
	require.NoError(t, model.DB.Select("quota").First(&refreshed, customer.Id).Error)
	assert.Equal(t, 200, refreshed.Quota)
}

func TestResellerCannotCreditCustomerOfAnotherReseller(t *testing.T) {
	setupSecurityEnrollmentTest(t)
	resellerA := createResellerTestUser(t, "reseller-credit-a", common.RoleReseller, 0)
	resellerB := createResellerTestUser(t, "reseller-credit-b", common.RoleReseller, 0)
	require.NoError(t, model.DB.Create(&model.ResellerWallet{UserId: resellerA.Id, Balance: 500}).Error)
	customerOfB := createResellerTestUser(t, "customer-credit-b", common.RoleCommonUser, resellerB.Id)
	params := gin.Params{{Key: "id", Value: strconv.Itoa(customerOfB.Id)}}

	response := resellerRequest(t, http.MethodPost, `{"quota":200}`, resellerA.Id, params, CreditOwnCustomer)

	assert.Contains(t, response.Body.String(), `"success":false`)
	var wallet model.ResellerWallet
	require.NoError(t, model.DB.Where("user_id = ?", resellerA.Id).First(&wallet).Error)
	assert.Equal(t, 500, wallet.Balance)
	var refreshed model.User
	require.NoError(t, model.DB.Select("quota").First(&refreshed, customerOfB.Id).Error)
	assert.Equal(t, 0, refreshed.Quota)
}

func TestSetResellerPolicyRejectsInvalidFactor(t *testing.T) {
	setupSecurityEnrollmentTest(t)
	reseller := createResellerTestUser(t, "reseller-factor", common.RoleReseller, 0)
	params := gin.Params{{Key: "id", Value: strconv.Itoa(reseller.Id)}}

	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/user/admin/resellers", strings.NewReader(`{"model_name":"gpt","base_price_factor":-2}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = params
	SetResellerPolicy(c)
	assert.Equal(t, http.StatusBadRequest, response.Code)
	assert.Contains(t, response.Body.String(), model.ErrInvalidBasePriceFactor.Error())
}
