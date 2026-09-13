package app

import (
	"errors"
	"regexp"
	"strings"
)

var (
	ErrAccountRequired = errors.New("account required")
	ErrCreditRequired  = errors.New("credits required")
	ErrBillingClosed   = errors.New("billing closed")
	ErrBillingAccess   = errors.New("billing access denied")
	ErrReceiptUsed     = errors.New("receipt already used")
	ErrRoundCompleted  = errors.New("refinement round completed")
	ErrPaidRounds      = errors.New("paid interview supports up to five questions")
	ErrBadCredentials  = errors.New("invalid credentials")
	ErrUsernameTaken   = errors.New("username unavailable")
)

const billingPolicyVersion = "manual-v1"
const accountPasswordIterations = 600000

var creditKinds = []string{"diagnosis", "refine", "tailor", "interview"}
var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{3,31}$`)
var planIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)
var receiptPattern = regexp.MustCompile(`^[A-Z0-9_-]{8,80}$`)

type BillingPlan struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	PriceCents int64          `json:"price_cents"`
	Credits    map[string]int `json:"credits"`
	Active     bool           `json:"active"`
}

type BillingSettings struct {
	Revision int64         `json:"revision"`
	Enabled  bool          `json:"enabled"`
	Payee    string        `json:"payee"`
	Channel  string        `json:"channel"`
	QRCodeID string        `json:"qr_code_id"`
	Notice   string        `json:"notice"`
	Support  string        `json:"support"`
	Plans    []BillingPlan `json:"plans"`
}

func defaultBillingSettings() BillingSettings {
	return BillingSettings{Channel: "alipay", Notice: "付款后请提交交易单号，人工核实到账后发放次数。",
		Plans: []BillingPlan{{ID: "job-kit", Name: "求职准备包", Credits: map[string]int{"diagnosis": 1, "refine": 2, "tailor": 2, "interview": 1}}}}
}

func validateBillingSettings(c *BillingSettings) error {
	bad := func(message string) error { return &billingValidationError{message} }
	c.Payee, c.Notice, c.Support = cleanInput(c.Payee), cleanInput(c.Notice), cleanInput(c.Support)
	if c.Channel != "alipay" && c.Channel != "wechat" {
		return bad("请选择支付宝或微信收款。")
	}
	if !textLength(c.Payee, 0, 80) || !textLength(c.Notice, 0, 600) || !textLength(c.Support, 0, 300) || len(c.Plans) > 5 {
		return bad("收款名称、说明或套餐数量超出范围；最多设置 5 个套餐。")
	}
	seen, active := map[string]bool{}, 0
	for i := range c.Plans {
		p := &c.Plans[i]
		p.Name = cleanInput(p.Name)
		if !planIDPattern.MatchString(p.ID) || seen[p.ID] || !textLength(p.Name, 2, 60) || p.PriceCents < 0 || p.PriceCents > 100000 {
			return bad("套餐编号需唯一，名称 2—60 字符，价格不能超过 1,000 元。")
		}
		seen[p.ID] = true
		total := 0
		for kind, units := range p.Credits {
			if kind != "diagnosis" && kind != "refine" && kind != "tailor" && kind != "interview" {
				return bad("套餐包含未知的次数类型。")
			}
			if units < 0 || units > 100 {
				return bad("每类次数需为 0—100 的整数。")
			}
			total += units
		}
		if p.Active {
			active++
			if p.PriceCents < 100 || total == 0 {
				return bad("上架套餐至少 1 元，且必须包含可用次数。")
			}
		}
	}
	if c.Enabled && (c.Payee == "" || c.QRCodeID == "" || c.Notice == "" || c.Support == "" || active == 0) {
		return bad("请先上传收款码，填写收款名称、核款说明与联系方式，并上架至少一个套餐。")
	}
	return nil
}

type billingValidationError struct{ message string }

func (e *billingValidationError) Error() string { return e.message }

type BillingAccount struct {
	ID             string `json:"id"`
	Username       string `json:"username"`
	CreatedAt      int64  `json:"created_at"`
	WelcomeGranted bool   `json:"welcome_granted"`
}

type CreditBalance struct {
	Available      int `json:"available"`
	Reserved       int `json:"reserved"`
	Used           int `json:"used"`
	OnHold         int `json:"on_hold"`
	TrialAvailable int `json:"trial_available"`
}

type BillingEvent struct {
	ID        int64  `json:"id"`
	OrderID   string `json:"order_id"`
	Kind      string `json:"kind"`
	Event     string `json:"event"`
	Units     int    `json:"units"`
	CreatedAt int64  `json:"created_at"`
}

type OrderSnapshot struct {
	Plan          BillingPlan `json:"plan"`
	Payee         string      `json:"payee"`
	Channel       string      `json:"channel"`
	QRCodeID      string      `json:"qr_code_id"`
	Notice        string      `json:"notice"`
	Support       string      `json:"support"`
	PolicyVersion string      `json:"policy_version"`
}

type PaymentClaim struct {
	Receipt string `json:"receipt"`
	Note    string `json:"note"`
}

type PaymentReview struct {
	Receipt string `json:"receipt"`
	Note    string `json:"note"`
}

type BillingRefund struct {
	AmountCents int64 `json:"amount_cents"`
	CreatedAt   int64 `json:"created_at"`
	PaymentReview
}

type BillingOrder struct {
	ID                string          `json:"id"`
	OwnerID           string          `json:"-"`
	Username          string          `json:"username,omitempty"`
	PlanName          string          `json:"plan_name"`
	AmountCents       int64           `json:"amount_cents"`
	Status            string          `json:"status"`
	Revision          int64           `json:"revision"`
	CreatedAt         int64           `json:"created_at"`
	UpdatedAt         int64           `json:"updated_at"`
	ExpiresAt         int64           `json:"expires_at"`
	PaidAt            int64           `json:"paid_at"`
	RefundRequestedAt int64           `json:"refund_requested_at"`
	RefundReviewing   bool            `json:"refund_reviewing"`
	RefundedCents     int64           `json:"refunded_cents"`
	Snapshot          *OrderSnapshot  `json:"snapshot,omitempty"`
	Claim             *PaymentClaim   `json:"claim,omitempty"`
	Review            *PaymentReview  `json:"review,omitempty"`
	Remaining         map[string]int  `json:"remaining,omitempty"`
	Reserved          int             `json:"reserved,omitempty"`
	ActiveTasks       int             `json:"active_tasks"`
	Refunds           []BillingRefund `json:"refunds,omitempty"`
}

func normalizeReceipt(value string) (string, error) {
	value = strings.ToUpper(strings.TrimSpace(value))
	if !receiptPattern.MatchString(value) {
		return "", &billingValidationError{"请填写完整交易单号（8—80 位字母、数字、横线或下划线）。"}
	}
	return value, nil
}
