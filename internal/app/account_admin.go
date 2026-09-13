package app

import (
	_ "embed"
	"errors"
)

//go:embed account-admin-migration.sql
var accountAdminMigration string

var ErrAIRestricted = errors.New("account AI generation restricted")

const adminUserPageSize = 30

type AdminUser struct {
	BillingAccount
	LastLoginAt int64                    `json:"last_login_at"`
	Revision    int64                    `json:"revision"`
	OrderCount  int                      `json:"order_count"`
	Wallet      map[string]CreditBalance `json:"wallet"`
}

type AdminUserStats struct {
	Total      int `json:"total"`
	NewWeek    int `json:"new_week"`
	Restricted int `json:"restricted"`
}

type AdminUsers struct {
	Users    []AdminUser    `json:"users"`
	Total    int            `json:"total"`
	Page     int            `json:"page"`
	PageSize int            `json:"page_size"`
	Stats    AdminUserStats `json:"stats"`
}

type AdminUserAction struct {
	ID              int64  `json:"id"`
	Action          string `json:"action"`
	CreatedAt       int64  `json:"created_at"`
	Note            string `json:"note"`
	RevokedSessions int64  `json:"revoked_sessions"`
}

type AdminUserDetail struct {
	User           AdminUser         `json:"user"`
	Note           string            `json:"note"`
	ActiveSessions int               `json:"active_sessions"`
	ReportCount    int               `json:"report_count"`
	PaidCents      int64             `json:"paid_cents"`
	Orders         []BillingOrder    `json:"orders"`
	Events         []BillingEvent    `json:"events"`
	Actions        []AdminUserAction `json:"actions"`
}

type AdminUserChange struct {
	Action    string `json:"action"`
	Revision  int64  `json:"revision"`
	Note      string `json:"note"`
	Confirmed bool   `json:"confirmed"`
}
