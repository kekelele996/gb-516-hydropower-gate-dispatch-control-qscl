package repository

import (
	"context"

	"github.com/blueship581/hydropower-gate-dispatch-control/backend/internal/dto"
	"github.com/blueship581/hydropower-gate-dispatch-control/backend/internal/model"
	"gorm.io/gorm"
)

// OperationDirectiveRepository owns all persistence operations for 操作指令.
type OperationDirectiveRepository interface {
	List(context.Context, dto.PageQuery) (Page[model.OperationDirective], error)
	Get(context.Context, uint) (model.OperationDirective, error)
	GetByCode(context.Context, string) (model.OperationDirective, error)
	Create(context.Context, *model.OperationDirective) error
	CreateWithGates(context.Context, *model.OperationDirective, []model.DirectiveGate) error
	ReplaceGates(context.Context, uint, []model.DirectiveGate) error
	ExecutingGateConflicts(context.Context, []string, uint) ([]string, error)
	Update(context.Context, uint, uint, *model.OperationDirective) error
	TransitionWithApproval(context.Context, uint, uint, *model.OperationDirective, *model.DirectiveApproval, *model.AuditLog) error
	Delete(context.Context, uint) error
	CountByStatus(context.Context) (map[string]int64, error)
}

type operationDirectiveRepository struct {
	store *Store[model.OperationDirective]
	db    *gorm.DB
}

func NewOperationDirectiveRepository(db *gorm.DB) OperationDirectiveRepository {
	return &operationDirectiveRepository{store: NewStore[model.OperationDirective](db), db: db}
}

func (r *operationDirectiveRepository) List(ctx context.Context, q dto.PageQuery) (Page[model.OperationDirective], error) {
	page, err := r.store.List(ctx, q)
	if err != nil {
		return Page[model.OperationDirective]{}, err
	}
	for index := range page.Items {
		if err := r.loadAssociations(ctx, &page.Items[index]); err != nil {
			return Page[model.OperationDirective]{}, err
		}
	}
	return page, nil
}
func (r *operationDirectiveRepository) Get(ctx context.Context, id uint) (model.OperationDirective, error) {
	var item model.OperationDirective
	err := databaseForContext(ctx, r.db).Preload("Approvals", func(db *gorm.DB) *gorm.DB {
		return db.Order("created_at ASC, id ASC")
	}).Preload("Gates", func(db *gorm.DB) *gorm.DB {
		return db.Order("id ASC")
	}).First(&item, id).Error
	return item, err
}
func (r *operationDirectiveRepository) GetByCode(ctx context.Context, code string) (model.OperationDirective, error) {
	item, err := r.store.GetByCode(ctx, code)
	if err != nil {
		return model.OperationDirective{}, err
	}
	return r.Get(ctx, item.ID)
}
func (r *operationDirectiveRepository) Create(ctx context.Context, item *model.OperationDirective) error {
	return r.store.Create(ctx, item)
}

// CreateWithGates persists a joint directive and its gate links as one unit.
func (r *operationDirectiveRepository) CreateWithGates(ctx context.Context, item *model.OperationDirective, links []model.DirectiveGate) error {
	db := databaseForContext(ctx, r.db)
	if err := db.Create(item).Error; err != nil {
		return err
	}
	for index := range links {
		links[index].DirectiveID = item.ID
	}
	if len(links) > 0 {
		if err := db.Create(&links).Error; err != nil {
			return err
		}
	}
	item.Gates = links
	return nil
}

// ReplaceGates swaps the whole gate set of a draft directive in one statement
// group so a draft never exposes a partial joint group.
func (r *operationDirectiveRepository) ReplaceGates(ctx context.Context, directiveID uint, links []model.DirectiveGate) error {
	db := databaseForContext(ctx, r.db)
	if err := db.Where("directive_id = ?", directiveID).Delete(&model.DirectiveGate{}).Error; err != nil {
		return err
	}
	for index := range links {
		links[index].DirectiveID = directiveID
	}
	if len(links) == 0 {
		return nil
	}
	return db.Create(&links).Error
}

// ExecutingGateConflicts returns the gate codes that are already bound to a
// different executing directive, either as its primary gate or as a member of
// its joint gate set.
func (r *operationDirectiveRepository) ExecutingGateConflicts(ctx context.Context, gateCodes []string, excludeDirectiveID uint) ([]string, error) {
	if len(gateCodes) == 0 {
		return nil, nil
	}
	db := databaseForContext(ctx, r.db)
	conflicts := make([]string, 0)
	var primary []string
	if err := db.Model(&model.OperationDirective{}).
		Where("status = ? AND id <> ? AND related_code IN ?", "executing", excludeDirectiveID, gateCodes).
		Pluck("related_code", &primary).Error; err != nil {
		return nil, err
	}
	conflicts = append(conflicts, primary...)
	var linked []string
	if err := db.Model(&model.DirectiveGate{}).
		Joins("JOIN operation_directives ON operation_directives.id = directive_gates.directive_id").
		Where("operation_directives.status = ? AND operation_directives.id <> ?", "executing", excludeDirectiveID).
		Where("operation_directives.deleted_at IS NULL AND directive_gates.gate_code IN ?", gateCodes).
		Pluck("directive_gates.gate_code", &linked).Error; err != nil {
		return nil, err
	}
	conflicts = append(conflicts, linked...)
	return conflicts, nil
}
func (r *operationDirectiveRepository) Update(ctx context.Context, id, version uint, item *model.OperationDirective) error {
	return r.store.Update(ctx, id, version, item)
}
func (r *operationDirectiveRepository) TransitionWithApproval(ctx context.Context, id, version uint, item *model.OperationDirective, approval *model.DirectiveApproval, audit *model.AuditLog) error {
	return databaseForContext(ctx, r.db).Transaction(func(tx *gorm.DB) error {
		if err := r.store.update(tx, id, version, item); err != nil {
			return err
		}
		if approval != nil {
			approval.DirectiveID = id
			if err := tx.Create(approval).Error; err != nil {
				return err
			}
		}
		return tx.Create(audit).Error
	})
}
func (r *operationDirectiveRepository) Delete(ctx context.Context, id uint) error {
	return r.store.Delete(ctx, id)
}
func (r *operationDirectiveRepository) CountByStatus(ctx context.Context) (map[string]int64, error) {
	return r.store.CountByStatus(ctx)
}

func (r *operationDirectiveRepository) loadAssociations(ctx context.Context, item *model.OperationDirective) error {
	db := databaseForContext(ctx, r.db)
	if err := db.Where("directive_id = ?", item.ID).Order("created_at ASC, id ASC").Find(&item.Approvals).Error; err != nil {
		return err
	}
	return db.Where("directive_id = ?", item.ID).Order("id ASC").Find(&item.Gates).Error
}
