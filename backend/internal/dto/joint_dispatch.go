package dto

import "time"

// CreateJointDispatchOrder is the public write contract for 闸门联合调度许可.
// Status is deliberately omitted so callers cannot bypass the service state
// machine, and GateCodes must hold two to five gates of one reservoir.
type CreateJointDispatchOrder struct {
	Code        string    `json:"code" binding:"required,min=2,max=64"`
	Name        string    `json:"name" binding:"required,min=2,max=160"`
	Description string    `json:"description" binding:"max=1000"`
	Owner       string    `json:"owner" binding:"required,max=120"`
	Category    string    `json:"category" binding:"required,max=80"`
	RiskLevel   string    `json:"riskLevel" binding:"required,oneof=low medium high critical"`
	EffectiveAt time.Time `json:"effectiveAt" binding:"required"`
	Evidence    string    `json:"evidence" binding:"max=2000"`
	TargetState string    `json:"targetState" binding:"required,oneof=open closed"`
	GateCodes   []string  `json:"gateCodes" binding:"required,min=2,max=5,dive,required,min=2,max=64"`
}

type UpdateJointDispatchOrder struct {
	ExpectedVersion uint      `json:"expectedVersion" binding:"required"`
	Name            string    `json:"name" binding:"required,min=2,max=160"`
	Description     string    `json:"description" binding:"max=1000"`
	Owner           string    `json:"owner" binding:"required,max=120"`
	Category        string    `json:"category" binding:"required,max=80"`
	RiskLevel       string    `json:"riskLevel" binding:"required,oneof=low medium high critical"`
	EffectiveAt     time.Time `json:"effectiveAt" binding:"required"`
	Evidence        string    `json:"evidence" binding:"max=2000"`
	TargetState     string    `json:"targetState" binding:"required,oneof=open closed"`
}
