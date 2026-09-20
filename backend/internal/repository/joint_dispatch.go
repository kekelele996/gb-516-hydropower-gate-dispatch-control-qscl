package repository

import (
	"context"

	"github.com/blueship581/hydropower-gate-dispatch-control/backend/internal/dto"
	"github.com/blueship581/hydropower-gate-dispatch-control/backend/internal/model"
	"gorm.io/gorm"
)

// JointDispatchRepository owns all persistence operations for 闸门联合调度许可.
type JointDispatchRepository interface {
	List(context.Context, dto.PageQuery) (Page[model.JointDispatchOrder], error)
	Get(context.Context, uint) (model.JointDispatchOrder, error)
	Create(context.Context, *model.JointDispatchOrder) error
	Update(context.Context, uint, uint, *model.JointDispatchOrder) error
	Delete(context.Context, uint) error
	CountByStatus(context.Context) (map[string]int64, error)
	ListExecutingByGateCodes(context.Context, []string, uint) ([]model.JointDispatchOrder, error)
}

type jointDispatchRepository struct {
	store *Store[model.JointDispatchOrder]
	db    *gorm.DB
}

func NewJointDispatchRepository(db *gorm.DB) JointDispatchRepository {
	return &jointDispatchRepository{store: NewStore[model.JointDispatchOrder](db), db: db}
}

func (r *jointDispatchRepository) List(ctx context.Context, q dto.PageQuery) (Page[model.JointDispatchOrder], error) {
	page, err := r.store.List(ctx, q)
	if err != nil {
		return Page[model.JointDispatchOrder]{}, err
	}
	ids := make([]uint, 0, len(page.Items))
	for _, item := range page.Items {
		ids = append(ids, item.ID)
	}
	gates := make([]model.JointDispatchGate, 0)
	if len(ids) > 0 {
		if err := databaseForContext(ctx, r.db).Where("order_id IN ?", ids).Order("id ASC").Find(&gates).Error; err != nil {
			return Page[model.JointDispatchOrder]{}, err
		}
	}
	byOrder := make(map[uint][]model.JointDispatchGate, len(ids))
	for _, gate := range gates {
		byOrder[gate.OrderID] = append(byOrder[gate.OrderID], gate)
	}
	for index := range page.Items {
		page.Items[index].Gates = byOrder[page.Items[index].ID]
	}
	return page, nil
}

func (r *jointDispatchRepository) Get(ctx context.Context, id uint) (model.JointDispatchOrder, error) {
	var item model.JointDispatchOrder
	err := databaseForContext(ctx, r.db).Preload("Gates", func(db *gorm.DB) *gorm.DB {
		return db.Order("id ASC")
	}).First(&item, id).Error
	return item, err
}

func (r *jointDispatchRepository) Create(ctx context.Context, item *model.JointDispatchOrder) error {
	return r.store.Create(ctx, item)
}

func (r *jointDispatchRepository) Update(ctx context.Context, id, version uint, item *model.JointDispatchOrder) error {
	return r.store.Update(ctx, id, version, item)
}

func (r *jointDispatchRepository) Delete(ctx context.Context, id uint) error {
	return r.store.Delete(ctx, id)
}

func (r *jointDispatchRepository) CountByStatus(ctx context.Context) (map[string]int64, error) {
	return r.store.CountByStatus(ctx)
}

// ListExecutingByGateCodes finds other executing orders that already hold any
// of the given gates, so a new execution can be rejected as a whole.
func (r *jointDispatchRepository) ListExecutingByGateCodes(ctx context.Context, gateCodes []string, excludeOrderID uint) ([]model.JointDispatchOrder, error) {
	items := make([]model.JointDispatchOrder, 0)
	err := databaseForContext(ctx, r.db).
		Joins("JOIN joint_dispatch_gates ON joint_dispatch_gates.order_id = joint_dispatch_orders.id").
		Where("joint_dispatch_orders.status = ? AND joint_dispatch_gates.gate_code IN ? AND joint_dispatch_orders.id <> ?",
			"executing", gateCodes, excludeOrderID).
		Group("joint_dispatch_orders.id").
		Preload("Gates").
		Find(&items).Error
	return items, err
}
