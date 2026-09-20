package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/blueship581/hydropower-gate-dispatch-control/backend/internal/constants"
	"github.com/blueship581/hydropower-gate-dispatch-control/backend/internal/dto"
	"github.com/blueship581/hydropower-gate-dispatch-control/backend/internal/model"
	"github.com/blueship581/hydropower-gate-dispatch-control/backend/internal/repository"
)

// GateConflict names one gate that blocks a joint dispatch execution.
type GateConflict struct {
	GateCode string `json:"gateCode"`
	Reason   string `json:"reason"`
}

// GateConflictError rejects the whole dispatch and lists every conflicting
// gate so the operator can resolve them before retrying.
type GateConflictError struct{ Conflicts []GateConflict }

func (e *GateConflictError) Error() string {
	parts := make([]string, 0, len(e.Conflicts))
	for _, conflict := range e.Conflicts {
		parts = append(parts, conflict.GateCode+"("+conflict.Reason+")")
	}
	return "joint dispatch rejected by gate conflicts: " + strings.Join(parts, ", ")
}

type JointDispatchService interface {
	List(context.Context, dto.PageQuery) (repository.Page[model.JointDispatchOrder], error)
	Get(context.Context, uint) (model.JointDispatchOrder, error)
	Create(context.Context, dto.CreateJointDispatchOrder, string, string) (model.JointDispatchOrder, error)
	Update(context.Context, uint, dto.UpdateJointDispatchOrder, string, string) (model.JointDispatchOrder, error)
	Transition(context.Context, uint, dto.TransitionRequest, string, string, string) (model.JointDispatchOrder, error)
	Delete(context.Context, uint, string, string) error
	StatusCounts(context.Context) (map[string]int64, error)
}

type jointDispatchService struct {
	repository repository.JointDispatchRepository
	gates      repository.GateUnitRepository
	directives repository.OperationDirectiveRepository
	security   SecurityService
}

func NewJointDispatchService(repo repository.JointDispatchRepository, gates repository.GateUnitRepository, directives repository.OperationDirectiveRepository, security SecurityService) JointDispatchService {
	return &jointDispatchService{repository: repo, gates: gates, directives: directives, security: security}
}

func (s *jointDispatchService) List(ctx context.Context, query dto.PageQuery) (repository.Page[model.JointDispatchOrder], error) {
	page, err := s.repository.List(ctx, query)
	if err != nil {
		return page, err
	}
	return page, s.enrichGateStates(ctx, page.Items)
}

func (s *jointDispatchService) Get(ctx context.Context, id uint) (model.JointDispatchOrder, error) {
	item, err := s.repository.Get(ctx, id)
	if err != nil {
		return item, err
	}
	items := []model.JointDispatchOrder{item}
	if err := s.enrichGateStates(ctx, items); err != nil {
		return model.JointDispatchOrder{}, err
	}
	return items[0], nil
}

// enrichGateStates fills the transient current status/version of every linked
// gate so the workbench always displays a live gate list after refresh.
func (s *jointDispatchService) enrichGateStates(ctx context.Context, items []model.JointDispatchOrder) error {
	codes := make([]string, 0)
	seen := make(map[string]bool)
	for _, item := range items {
		for _, gate := range item.Gates {
			if !seen[gate.GateCode] {
				seen[gate.GateCode] = true
				codes = append(codes, gate.GateCode)
			}
		}
	}
	gates, err := s.gates.ListByCodes(ctx, codes)
	if err != nil {
		return err
	}
	byCode := make(map[string]model.GateUnit, len(gates))
	for _, gate := range gates {
		byCode[gate.Code] = gate
	}
	for i := range items {
		for j := range items[i].Gates {
			if gate, ok := byCode[items[i].Gates[j].GateCode]; ok {
				items[i].Gates[j].CurrentStatus = gate.Status
				items[i].Gates[j].CurrentVersion = gate.Version
			}
		}
	}
	return nil
}

func (s *jointDispatchService) Create(ctx context.Context, input dto.CreateJointDispatchOrder, actor, requestID string) (model.JointDispatchOrder, error) {
	if strings.TrimSpace(input.Code) == "" || strings.TrimSpace(input.Name) == "" || strings.TrimSpace(input.Owner) == "" {
		return model.JointDispatchOrder{}, ErrInvalidInput
	}
	gates, err := s.resolveGateGroup(ctx, input.GateCodes)
	if err != nil {
		return model.JointDispatchOrder{}, err
	}
	now := time.Now().UTC()
	item := model.JointDispatchOrder{
		BaseModel: model.BaseModel{
			Code: strings.ToUpper(strings.TrimSpace(input.Code)), Name: strings.TrimSpace(input.Name),
			Status: model.JointDispatchInitialStatus, Version: 1, Description: strings.TrimSpace(input.Description),
		},
		Facility: gates[0].Facility, Owner: strings.TrimSpace(input.Owner),
		Category: strings.TrimSpace(input.Category), RiskLevel: input.RiskLevel,
		ReservoirCode: gates[0].RelatedCode, TargetState: strings.TrimSpace(input.TargetState),
		EffectiveAt: input.EffectiveAt.UTC(), Evidence: strings.TrimSpace(input.Evidence),
	}
	for _, gate := range gates {
		item.Gates = append(item.Gates, model.JointDispatchGate{
			GateID: gate.ID, GateCode: gate.Code, GateName: gate.Name, GateVersion: gate.Version, CreatedAt: now,
		})
	}
	if err := s.security.WithinTransaction(ctx, func(txCtx context.Context) error {
		if err := s.repository.Create(txCtx, &item); err != nil {
			return err
		}
		return s.security.Audit(txCtx, actor, requestID, "create", "JointDispatchOrder", item.ID, "", item.Status,
			fmt.Sprintf("created 联合调度许可 with gates %s", strings.Join(gateCodesOf(item.Gates), ",")))
	}); err != nil {
		return model.JointDispatchOrder{}, fmt.Errorf("create 联合调度许可: %w", err)
	}
	return item, nil
}

// resolveGateGroup loads the requested gates and enforces the 2-5 same-
// reservoir rule before any state is written.
func (s *jointDispatchService) resolveGateGroup(ctx context.Context, rawCodes []string) ([]model.GateUnit, error) {
	codes := make([]string, 0, len(rawCodes))
	seen := make(map[string]bool)
	for _, raw := range rawCodes {
		code := strings.ToUpper(strings.TrimSpace(raw))
		if code == "" {
			continue
		}
		if seen[code] {
			return nil, fmt.Errorf("%w: duplicate gate %s", ErrInvalidInput, code)
		}
		seen[code] = true
		codes = append(codes, code)
	}
	if len(codes) < 2 || len(codes) > 5 {
		return nil, fmt.Errorf("%w: joint dispatch requires 2 to 5 gates, got %d", ErrInvalidInput, len(codes))
	}
	gates, err := s.gates.ListByCodes(ctx, codes)
	if err != nil {
		return nil, fmt.Errorf("load linked gates: %w", err)
	}
	if len(gates) != len(codes) {
		known := make(map[string]bool, len(gates))
		for _, gate := range gates {
			known[gate.Code] = true
		}
		missing := make([]string, 0)
		for _, code := range codes {
			if !known[code] {
				missing = append(missing, code)
			}
		}
		return nil, fmt.Errorf("%w: unknown gates %s", ErrInvalidInput, strings.Join(missing, ","))
	}
	byCode := make(map[string]model.GateUnit, len(gates))
	for _, gate := range gates {
		byCode[gate.Code] = gate
	}
	ordered := make([]model.GateUnit, 0, len(codes))
	reservoir := ""
	for _, code := range codes {
		gate := byCode[code]
		if reservoir == "" {
			reservoir = gate.RelatedCode
		} else if !strings.EqualFold(reservoir, gate.RelatedCode) {
			return nil, fmt.Errorf("%w: gate %s belongs to reservoir %s, expected %s", ErrInvalidInput, gate.Code, gate.RelatedCode, reservoir)
		}
		ordered = append(ordered, gate)
	}
	return ordered, nil
}

func (s *jointDispatchService) Update(ctx context.Context, id uint, input dto.UpdateJointDispatchOrder, actor, requestID string) (model.JointDispatchOrder, error) {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return model.JointDispatchOrder{}, err
	}
	if current.Status != model.JointDispatchInitialStatus {
		return model.JointDispatchOrder{}, ErrImmutableState
	}
	if strings.TrimSpace(input.Name) == "" || strings.TrimSpace(input.Owner) == "" {
		return model.JointDispatchOrder{}, ErrInvalidInput
	}
	current.Name = strings.TrimSpace(input.Name)
	current.Description = strings.TrimSpace(input.Description)
	current.Owner = strings.TrimSpace(input.Owner)
	current.Category = strings.TrimSpace(input.Category)
	current.RiskLevel = input.RiskLevel
	current.TargetState = strings.TrimSpace(input.TargetState)
	current.EffectiveAt = input.EffectiveAt.UTC()
	current.Evidence = strings.TrimSpace(input.Evidence)
	current.Version = input.ExpectedVersion + 1
	current.UpdatedAt = time.Now().UTC()
	if err := s.security.WithinTransaction(ctx, func(txCtx context.Context) error {
		if err := s.repository.Update(txCtx, id, input.ExpectedVersion, &current); err != nil {
			return err
		}
		return s.security.Audit(txCtx, actor, requestID, "update", "JointDispatchOrder", id, current.Status, current.Status, "updated business fields")
	}); err != nil {
		return model.JointDispatchOrder{}, fmt.Errorf("update 联合调度许可: %w", err)
	}
	return s.repository.Get(ctx, id)
}

func (s *jointDispatchService) Transition(ctx context.Context, id uint, input dto.TransitionRequest, actor, role, requestID string) (model.JointDispatchOrder, error) {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return model.JointDispatchOrder{}, err
	}
	target := strings.TrimSpace(input.Status)
	if !constants.CanTransition(constants.JointDispatchTransitions, current.Status, target) {
		return model.JointDispatchOrder{}, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, current.Status, target)
	}
	if !jointDispatchRoleAllowed(target, role) {
		return model.JointDispatchOrder{}, ErrForbidden
	}
	if target == string(constants.DirectiveStateApproved) && (current.SubmittedBy == "" || strings.EqualFold(current.SubmittedBy, actor)) {
		return model.JointDispatchOrder{}, ErrTwoPersonRequired
	}

	var gates []model.GateUnit
	var gateTarget, gateAction string
	switch target {
	case string(constants.DirectiveStateExecuting):
		gates, err = s.executionGatePlan(ctx, &current)
		if err != nil {
			return model.JointDispatchOrder{}, err
		}
		gateTarget, gateAction = string(constants.GateStateMoving), "joint_execution"
	case string(constants.DirectiveStateCompleted), "failed":
		gates, err = s.settlementGatePlan(ctx, &current)
		if err != nil {
			return model.JointDispatchOrder{}, err
		}
		gateAction = "joint_settlement"
		if target == string(constants.DirectiveStateCompleted) {
			gateTarget = current.TargetState
		} else {
			gateTarget = string(constants.GateStateLocked)
		}
	}

	before := current.Status
	now := time.Now().UTC()
	current.Status = target
	switch target {
	case string(constants.DirectiveStatePending):
		current.SubmittedBy, current.SubmittedAt = actor, &now
	case string(constants.DirectiveStateApproved):
		current.ApprovedBy, current.ApprovedAt = actor, &now
	case string(constants.DirectiveStateExecuting):
		current.ExecutedBy, current.ExecutedAt = actor, &now
	case string(constants.DirectiveStateCompleted), "failed":
		current.SettledBy, current.SettledAt = actor, &now
	}
	current.Version = input.ExpectedVersion + 1
	current.UpdatedAt = now
	detail := fmt.Sprintf("%s: %s", current.Code, strings.TrimSpace(input.Reason))

	if err := s.security.WithinTransaction(ctx, func(txCtx context.Context) error {
		if err := s.repository.Update(txCtx, id, input.ExpectedVersion, &current); err != nil {
			return err
		}
		if err := s.security.Audit(txCtx, actor, requestID, "transition", "JointDispatchOrder", id, before, target, input.Reason); err != nil {
			return err
		}
		for index := range gates {
			gate := gates[index]
			if gate.Status == gateTarget {
				continue
			}
			gateBefore := gate.Status
			gate.Status = gateTarget
			gate.Version++
			gate.UpdatedAt = now
			if err := s.gates.Update(txCtx, gate.ID, gate.Version-1, &gate); err != nil {
				return err
			}
			if err := s.security.Audit(txCtx, actor, requestID, gateAction, "GateUnit", gate.ID, gateBefore, gateTarget, detail); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return model.JointDispatchOrder{}, fmt.Errorf("transition 联合调度许可: %w", err)
	}
	return s.Get(ctx, id)
}

// executionGatePlan collects every gate that would block the atomic move into
// moving. Any locked gate, gate occupied by another instruction, or version
// drift rejects the whole execution with the full conflict list.
func (s *jointDispatchService) executionGatePlan(ctx context.Context, order *model.JointDispatchOrder) ([]model.GateUnit, error) {
	codes := gateCodesOf(order.Gates)
	gates, err := s.gates.ListByCodes(ctx, codes)
	if err != nil {
		return nil, fmt.Errorf("load linked gates: %w", err)
	}
	byCode := make(map[string]model.GateUnit, len(gates))
	for _, gate := range gates {
		byCode[gate.Code] = gate
	}
	conflicts := make([]GateConflict, 0)
	for _, ref := range order.Gates {
		gate, ok := byCode[ref.GateCode]
		if !ok {
			conflicts = append(conflicts, GateConflict{GateCode: ref.GateCode, Reason: "闸门不存在"})
			continue
		}
		if gate.Version != ref.GateVersion {
			conflicts = append(conflicts, GateConflict{GateCode: gate.Code, Reason: fmt.Sprintf("版本冲突(快照v%d/当前v%d)", ref.GateVersion, gate.Version)})
		}
		switch gate.Status {
		case string(constants.GateStateLocked):
			conflicts = append(conflicts, GateConflict{GateCode: gate.Code, Reason: "已闭锁"})
		case string(constants.GateStateMoving):
			conflicts = append(conflicts, GateConflict{GateCode: gate.Code, Reason: "已在动作中"})
		}
	}
	directives, err := s.directives.ListActiveByGateCodes(ctx, codes)
	if err != nil {
		return nil, fmt.Errorf("check active directives: %w", err)
	}
	for _, directive := range directives {
		conflicts = append(conflicts, GateConflict{GateCode: directive.RelatedCode, Reason: "已有执行中指令 " + directive.Code})
	}
	others, err := s.repository.ListExecutingByGateCodes(ctx, codes, order.ID)
	if err != nil {
		return nil, fmt.Errorf("check executing joint dispatches: %w", err)
	}
	for _, other := range others {
		for _, gate := range other.Gates {
			if _, ok := byCode[gate.GateCode]; ok {
				conflicts = append(conflicts, GateConflict{GateCode: gate.GateCode, Reason: "联合指令 " + other.Code + " 执行中"})
			}
		}
	}
	if len(conflicts) > 0 {
		return nil, &GateConflictError{Conflicts: conflicts}
	}
	planned := make([]model.GateUnit, 0, len(order.Gates))
	for _, ref := range order.Gates {
		planned = append(planned, byCode[ref.GateCode])
	}
	return planned, nil
}

// settlementGatePlan verifies every gate is still in moving before the whole
// group settles, so a completion or failure receipt lands on all gates at once.
func (s *jointDispatchService) settlementGatePlan(ctx context.Context, order *model.JointDispatchOrder) ([]model.GateUnit, error) {
	codes := gateCodesOf(order.Gates)
	gates, err := s.gates.ListByCodes(ctx, codes)
	if err != nil {
		return nil, fmt.Errorf("load linked gates: %w", err)
	}
	byCode := make(map[string]model.GateUnit, len(gates))
	for _, gate := range gates {
		byCode[gate.Code] = gate
	}
	conflicts := make([]GateConflict, 0)
	planned := make([]model.GateUnit, 0, len(order.Gates))
	for _, ref := range order.Gates {
		gate, ok := byCode[ref.GateCode]
		if !ok {
			conflicts = append(conflicts, GateConflict{GateCode: ref.GateCode, Reason: "闸门不存在"})
			continue
		}
		if gate.Status != string(constants.GateStateMoving) {
			conflicts = append(conflicts, GateConflict{GateCode: gate.Code, Reason: "未处于动作中，无法随指令同时落定"})
			continue
		}
		planned = append(planned, gate)
	}
	if len(conflicts) > 0 {
		return nil, &GateConflictError{Conflicts: conflicts}
	}
	return planned, nil
}

func (s *jointDispatchService) Delete(ctx context.Context, id uint, actor, requestID string) error {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return err
	}
	if current.Status != model.JointDispatchInitialStatus {
		return ErrImmutableState
	}
	return s.security.WithinTransaction(ctx, func(txCtx context.Context) error {
		if err := s.repository.Delete(txCtx, id); err != nil {
			return err
		}
		return s.security.Audit(txCtx, actor, requestID, "delete", "JointDispatchOrder", id, current.Status, "deleted", "soft deleted 联合调度许可")
	})
}

func (s *jointDispatchService) StatusCounts(ctx context.Context) (map[string]int64, error) {
	return s.repository.CountByStatus(ctx)
}

func jointDispatchRoleAllowed(target, role string) bool {
	switch target {
	case string(constants.DirectiveStatePending):
		return role == model.RoleOperator || role == model.RoleAdmin
	case string(constants.DirectiveStateApproved):
		return role == model.RoleReviewer || role == model.RoleAdmin
	case string(constants.DirectiveStateExecuting), string(constants.DirectiveStateCompleted), "failed":
		return role == model.RoleOperator || role == model.RoleAdmin
	case string(constants.DirectiveStateAborted):
		return role == model.RoleOperator || role == model.RoleReviewer || role == model.RoleAdmin
	default:
		return false
	}
}

func gateCodesOf(gates []model.JointDispatchGate) []string {
	codes := make([]string, 0, len(gates))
	for _, gate := range gates {
		codes = append(codes, gate.GateCode)
	}
	return codes
}
