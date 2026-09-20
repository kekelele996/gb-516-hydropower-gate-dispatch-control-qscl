package model

import "time"

// JointDispatchOrder models 闸门联合调度许可 as an independently versioned
// aggregate. One order coordinates two to five gates of the same reservoir so
// the whole group enters the moving state and settles under a single
// two-person permit.
type JointDispatchOrder struct {
	BaseModel
	Facility      string              `json:"facility" gorm:"size:120;index"`
	Owner         string              `json:"owner" gorm:"size:120;index"`
	Category      string              `json:"category" gorm:"size:80;index"`
	RiskLevel     string              `json:"riskLevel" gorm:"size:32;index"`
	ReservoirCode string              `json:"reservoirCode" gorm:"size:64;index"`
	TargetState   string              `json:"targetState" gorm:"size:32;not null"`
	EffectiveAt   time.Time           `json:"effectiveAt"`
	Evidence      string              `json:"evidence" gorm:"size:2000"`
	SubmittedBy   string              `json:"submittedBy" gorm:"size:80;index"`
	SubmittedAt   *time.Time          `json:"submittedAt"`
	ApprovedBy    string              `json:"approvedBy" gorm:"size:80;index"`
	ApprovedAt    *time.Time          `json:"approvedAt"`
	ExecutedBy    string              `json:"executedBy" gorm:"size:80;index"`
	ExecutedAt    *time.Time          `json:"executedAt"`
	SettledBy     string              `json:"settledBy" gorm:"size:80;index"`
	SettledAt     *time.Time          `json:"settledAt"`
	Gates         []JointDispatchGate `json:"gates" gorm:"foreignKey:OrderID;constraint:OnDelete:CASCADE"`
}

func (item *JointDispatchOrder) GetBase() *BaseModel { return &item.BaseModel }

func (item JointDispatchOrder) TableName() string { return "joint_dispatch_orders" }

var JointDispatchInitialStatus = "draft"

// JointDispatchGate snapshots each linked gate and its version when the order
// is drafted. Execution compares the snapshot with the live gate so any drift
// rejects the whole dispatch. CurrentStatus/CurrentVersion are read-time
// enrichments and never persisted.
type JointDispatchGate struct {
	ID             uint      `json:"id" gorm:"primaryKey"`
	OrderID        uint      `json:"orderId" gorm:"not null;index;uniqueIndex:idx_joint_order_gate"`
	GateID         uint      `json:"gateId" gorm:"not null;index"`
	GateCode       string    `json:"gateCode" gorm:"size:64;not null;uniqueIndex:idx_joint_order_gate"`
	GateName       string    `json:"gateName" gorm:"size:160;not null"`
	GateVersion    uint      `json:"gateVersion" gorm:"not null"`
	CurrentStatus  string    `json:"currentStatus,omitempty" gorm:"-"`
	CurrentVersion uint      `json:"currentVersion,omitempty" gorm:"-"`
	CreatedAt      time.Time `json:"createdAt"`
}

func (item JointDispatchGate) TableName() string { return "joint_dispatch_gates" }
