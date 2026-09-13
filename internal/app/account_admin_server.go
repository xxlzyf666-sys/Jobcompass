package app

import (
	"errors"
	"net/http"
	"time"
)

func adminUserError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		jsonError(w, 404, "没有找到这个用户，请返回用户列表重新选择。")
	case errors.Is(err, ErrConflict):
		jsonError(w, 409, "这个用户的管理信息已更新，请核对最新状态后重新操作。")
	default:
		billingStoreError(w, err)
	}
}

func (a *App) adminUsers(w http.ResponseWriter, r *http.Request) {
	if !a.requireBillingAdmin(w, r, false) {
		return
	}
	result, err := a.store.AdminUsers(r.Context(), r.URL.Query().Get("q"), r.URL.Query().Get("status"), billingPage(r), time.Now())
	if err != nil {
		adminUserError(w, err)
		return
	}
	jsonResponse(w, 200, result)
}

func (a *App) adminUser(w http.ResponseWriter, r *http.Request) {
	if !a.requireBillingAdmin(w, r, false) {
		return
	}
	detail, err := a.store.AdminUser(r.Context(), r.PathValue("user"), time.Now())
	if err != nil {
		adminUserError(w, err)
		return
	}
	jsonResponse(w, 200, detail)
}

func (a *App) adminChangeUser(w http.ResponseWriter, r *http.Request) {
	if !a.requireBillingAdmin(w, r, true) {
		return
	}
	var change AdminUserChange
	if !decodeRequest(w, r, &change) {
		return
	}
	token, _ := a.adminAuthenticated(r)
	if err := a.store.ChangeAdminUser(r.Context(), r.PathValue("user"), a.store.privateHash("account-operator:"+token), change, time.Now()); err != nil {
		adminUserError(w, err)
		return
	}
	a.adminUser(w, r)
}
