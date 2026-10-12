package controller

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type resellerPolicyRequest struct {
	ModelName       string   `json:"model_name"`
	BasePriceFactor *float64 `json:"base_price_factor"`
}

type resellerCustomerModelRequest struct {
	ModelName string `json:"model_name"`
	Allowed   bool   `json:"allowed"`
}

type resellerCustomerMarkupRequest struct {
	ModelName string  `json:"model_name"`
	Markup    float64 `json:"markup"`
}

// ListResellers returns every reseller account with its profile. Admin only.
func ListResellers(c *gin.Context) {
	var users []model.User
	if err := model.DB.Where("role = ?", common.RoleReseller).Order("id asc").Find(&users).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, users)
}

// GetResellerPolicies returns a reseller's models and base price factors. Admin only.
func GetResellerPolicies(c *gin.Context) {
	resellerId, ok := parseResellerIdParam(c)
	if !ok {
		return
	}
	if _, err := model.GetResellerUser(resellerId); err != nil {
		common.ApiError(c, err)
		return
	}
	policies, err := model.ListResellerPolicies(resellerId)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, policies)
}

// SetResellerPolicy grants, reprices, or removes one model for a reseller. Admin only.
func SetResellerPolicy(c *gin.Context) {
	resellerId, ok := parseResellerIdParam(c)
	if !ok {
		return
	}
	var req resellerPolicyRequest
	if err := common.DecodeJson(c.Request.Body, &req); err != nil || req.ModelName == "" {
		common.ApiErrorMsg(c, "invalid parameters")
		return
	}
	if _, err := model.GetResellerUser(resellerId); err != nil {
		common.ApiError(c, err)
		return
	}
	if err := model.SetResellerModelPolicy(resellerId, req.ModelName, req.BasePriceFactor); err != nil {
		resellerValidationError(c, err)
		return
	}
	common.ApiSuccess(c, nil)
}

type resellerProfileRequest struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
	Name        string `json:"name"`
	ContactInfo string `json:"contact_info"`
}

// CreateReseller creates a reseller account and its profile and wallet. Admin only.
func CreateReseller(c *gin.Context) {
	var req resellerProfileRequest
	if err := common.DecodeJson(c.Request.Body, &req); err != nil || req.Username == "" || req.Password == "" {
		common.ApiErrorMsg(c, "invalid parameters")
		return
	}
	user := &model.User{
		Username:    req.Username,
		Password:    req.Password,
		DisplayName: displayNameOrUsername(req.DisplayName, req.Username),
		Role:        common.RoleReseller,
		Status:      common.UserStatusEnabled,
		Group:       "default",
	}
	if err := model.DB.Transaction(func(tx *gorm.DB) error {
		if err := user.InsertWithTx(tx, 0); err != nil {
			return err
		}
		profile := model.ResellerProfile{UserId: user.Id, Name: req.Name, ContactInfo: req.ContactInfo, CreatedAt: common.GetTimestamp(), UpdatedAt: common.GetTimestamp()}
		if err := tx.Create(&profile).Error; err != nil {
			return err
		}
		return model.EnsureResellerWallet(tx, user.Id)
	}); err != nil {
		common.ApiError(c, err)
		return
	}
	user.FinishInsert(0)
	common.ApiSuccess(c, user)
}

// UpdateResellerProfile changes a reseller's display name and contact details. Admin only.
func UpdateResellerProfile(c *gin.Context) {
	resellerId, ok := parseResellerIdParam(c)
	if !ok {
		return
	}
	var req resellerProfileRequest
	if err := common.DecodeJson(c.Request.Body, &req); err != nil {
		common.ApiErrorMsg(c, "invalid parameters")
		return
	}
	if _, err := model.GetResellerUser(resellerId); err != nil {
		common.ApiError(c, err)
		return
	}
	if err := model.UpsertResellerProfile(resellerId, req.Name, req.ContactInfo); err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, nil)
}

// SetResellerStatus enables or disables a reseller account. Admin only.
func SetResellerStatus(c *gin.Context) {
	resellerId, ok := parseResellerIdParam(c)
	if !ok {
		return
	}
	var req struct {
		Status int `json:"status"`
	}
	if err := common.DecodeJson(c.Request.Body, &req); err != nil || (req.Status != common.UserStatusEnabled && req.Status != common.UserStatusDisabled) {
		common.ApiErrorMsg(c, "invalid parameters")
		return
	}
	if _, err := model.GetResellerUser(resellerId); err != nil {
		common.ApiError(c, err)
		return
	}
	if err := model.DB.Model(&model.User{}).Where("id = ?", resellerId).Update("status", req.Status).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	if err := model.PublishUserAuthCache(resellerId); err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, nil)
}

// CreateOwnCustomer creates a customer owned by the authenticated reseller.
func CreateOwnCustomer(c *gin.Context) {
	var req resellerProfileRequest
	if err := common.DecodeJson(c.Request.Body, &req); err != nil || req.Username == "" || req.Password == "" {
		common.ApiErrorMsg(c, "invalid parameters")
		return
	}
	user := &model.User{
		Username:    req.Username,
		Password:    req.Password,
		DisplayName: displayNameOrUsername(req.DisplayName, req.Username),
		Role:        common.RoleCommonUser,
		Status:      common.UserStatusEnabled,
		Group:       "default",
		ResellerId:  c.GetInt("id"),
	}
	if err := user.InsertWithTx(model.DB, 0); err != nil {
		common.ApiError(c, err)
		return
	}
	user.FinishInsert(0)
	common.ApiSuccess(c, user)
}

// SetOwnCustomerStatus enables or disables an owned customer. Reseller only acts on its own customers.
func SetOwnCustomerStatus(c *gin.Context) {
	customerId, ok := parseCustomerIdParam(c)
	if !ok {
		return
	}
	var req struct {
		Status int `json:"status"`
	}
	if err := common.DecodeJson(c.Request.Body, &req); err != nil || (req.Status != common.UserStatusEnabled && req.Status != common.UserStatusDisabled) {
		common.ApiErrorMsg(c, "invalid parameters")
		return
	}
	if _, err := model.GetResellerCustomer(c.GetInt("id"), customerId); err != nil {
		common.ApiError(c, err)
		return
	}
	if err := model.DB.Model(&model.User{}).Where("id = ?", customerId).Update("status", req.Status).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	if err := model.PublishUserAuthCache(customerId); err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, nil)
}

func displayNameOrUsername(displayName string, username string) string {
	if displayName == "" {
		return username
	}
	return displayName
}

type resellerCreditRequest struct {
	Quota int `json:"quota"`
}

func CreditOwnCustomer(c *gin.Context) {
	customerId, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		common.ApiErrorMsg(c, "invalid parameters")
		return
	}
	var req resellerCreditRequest
	if err := common.DecodeJson(c.Request.Body, &req); err != nil {
		common.ApiErrorMsg(c, "invalid parameters")
		return
	}
	resellerId := c.GetInt("id")
	err = model.CreditResellerCustomer(resellerId, customerId, req.Quota)
	switch {
	case err == nil:
		common.ApiSuccess(c, nil)
	case errors.Is(err, model.ErrResellerCostInsufficient):
		common.ApiErrorMsg(c, "reseller cost balance insufficient")
	case errors.Is(err, model.ErrInvalidUserQuotaAdjustment):
		common.ApiErrorMsg(c, "invalid parameters")
	case errors.Is(err, model.ErrWalletQuotaLimitExceeded):
		common.ApiErrorMsg(c, "quota limit exceeded")
	case errors.Is(err, gorm.ErrRecordNotFound):
		common.ApiErrorMsg(c, "reseller customer not found")
	default:
		common.ApiError(c, err)
	}
}

// GetOwnCustomerTopUps returns the top-up history of the reseller's own customers.
func GetOwnCustomerTopUps(c *gin.Context) {
	pageInfo := common.GetPageQuery(c)
	topups, total, err := model.ListResellerCustomerTopUps(c.GetInt("id"), pageInfo)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	pageInfo.SetTotal(int(total))
	pageInfo.SetItems(topups)
	common.ApiSuccess(c, pageInfo)
}

// GetOwnCustomers lists the customers owned by the authenticated reseller.
func GetOwnCustomers(c *gin.Context) {
	customers, err := model.ListResellerCustomers(c.GetInt("id"))
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, customers)
}

// GetOwnCustomerModels returns a customer's granted models, only for an owned customer.
func GetOwnCustomerModels(c *gin.Context) {
	customerId, ok := parseCustomerIdParam(c)
	if !ok {
		return
	}
	resellerId := c.GetInt("id")
	if _, err := model.GetResellerCustomer(resellerId, customerId); err != nil {
		common.ApiError(c, err)
		return
	}
	rows, err := model.ListCustomerModels(resellerId, customerId)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, rows)
}

// SetOwnCustomerModel grants or revokes one of the reseller's models for an owned customer.
func SetOwnCustomerModel(c *gin.Context) {
	customerId, ok := parseCustomerIdParam(c)
	if !ok {
		return
	}
	var req resellerCustomerModelRequest
	if err := common.DecodeJson(c.Request.Body, &req); err != nil || req.ModelName == "" {
		common.ApiErrorMsg(c, "invalid parameters")
		return
	}
	resellerId := c.GetInt("id")
	if _, err := model.GetResellerCustomer(resellerId, customerId); err != nil {
		common.ApiError(c, err)
		return
	}
	if err := model.SetResellerCustomerModel(resellerId, customerId, req.ModelName, req.Allowed); err != nil {
		resellerValidationError(c, err)
		return
	}
	common.ApiSuccess(c, nil)
}

// SetOwnCustomerMarkup stores the markup for one of an owned customer's granted models.
func SetOwnCustomerMarkup(c *gin.Context) {
	customerId, ok := parseCustomerIdParam(c)
	if !ok {
		return
	}
	var req resellerCustomerMarkupRequest
	if err := common.DecodeJson(c.Request.Body, &req); err != nil || req.ModelName == "" {
		common.ApiErrorMsg(c, "invalid parameters")
		return
	}
	resellerId := c.GetInt("id")
	if _, err := model.GetResellerCustomer(resellerId, customerId); err != nil {
		common.ApiError(c, err)
		return
	}
	if err := model.SetResellerCustomerMarkup(resellerId, customerId, req.ModelName, req.Markup); err != nil {
		resellerValidationError(c, err)
		return
	}
	common.ApiSuccess(c, nil)
}

func resellerValidationError(c *gin.Context, err error) {
	if errors.Is(err, model.ErrModelNotInResellerPolicy) ||
		errors.Is(err, model.ErrInvalidMarkup) ||
		errors.Is(err, model.ErrInvalidBasePriceFactor) {
		common.ApiErrorStatus(c, http.StatusBadRequest, err)
		return
	}
	common.ApiError(c, err)
}

func parseResellerIdParam(c *gin.Context) (int, bool) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		common.ApiErrorMsg(c, "invalid reseller id")
		return 0, false
	}
	return id, true
}

func parseCustomerIdParam(c *gin.Context) (int, bool) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		common.ApiErrorMsg(c, "invalid customer id")
		return 0, false
	}
	return id, true
}
