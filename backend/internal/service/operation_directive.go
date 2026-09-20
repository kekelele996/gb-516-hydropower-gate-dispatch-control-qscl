package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/blueship581/hydropower-gate-dispatch-control/backend/internal/constants"
	"github.com/blueship581/hydropower-gate-dispatch-control/backend/internal/dto"
	"github.com/blueship581/hydropower-gate-dispatch-control/backend/internal/model"
	"github.com/blueship581/hydropower-gate-dispatch-control/backend/internal/repository"
)

type OperationDirectiveService interface {
	List(context.Context, dto.PageQuery) (repository.Page[model.OperationDirective], error)
	Get(context.Context, uint) (model.OperationDirective, error)
	Create(context.Context, dto.CreateOperationDirective, string, string) (model.OperationDirective, error)
	Update(context.Context, uint, dto.UpdateOperationDirective, string, string) (model.OperationDirective, error)
	Transition(context.Context, uint, dto.TransitionRequest, string, string, string) (model.OperationDirective, error)
	Delete(context.Context, uint, string, string) error
	StatusCounts(context.Context) (map[string]int64, error)
}

type operationDirectiveService struct {
	repository repository.OperationDirectiveRepository
	gates      repository.GateUnitRepository
	security   SecurityService
}

func NewOperationDirectiveService(repo repository.OperationDirectiveRepository, gates repository.GateUnitRepository, security SecurityService) OperationDirectiveService {
	return &operationDirectiveService{repository: repo, gates: gates, security: security}
}

func (s *operationDirectiveService) List(ctx context.Context, query dto.PageQuery) (repository.Page[model.OperationDirective], error) {
	page, err := s.repository.List(ctx, query)
	if err != nil {
		return page, err
	}
	items := make([]*model.OperationDirective, 0, len(page.Items))
	for index := range page.Items {
		items = append(items, &page.Items[index])
	}
	if err := s.hydrateGateStates(ctx, items...); err != nil {
		return repository.Page[model.OperationDirective]{}, err
	}
	return page, nil
}

func (s *operationDirectiveService) Get(ctx context.Context, id uint) (model.OperationDirective, error) {
	item, err := s.repository.Get(ctx, id)
	if err != nil {
		return item, err
	}
	if err := s.hydrateGateStates(ctx, &item); err != nil {
		return model.OperationDirective{}, err
	}
	return item, nil
}

func (s *operationDirectiveService) Create(ctx context.Context, input dto.CreateOperationDirective, actor, requestID string) (model.OperationDirective, error) {
	if err := validateOperationDirectiveBusinessFields(input.Code, input.Name, input.Facility, input.Owner); err != nil {
		return model.OperationDirective{}, err
	}
	gateCodes := normalizeGateCodes(input.GateCodes)
	var links []model.DirectiveGate
	if len(gateCodes) > 0 {
		gates, err := s.validateJointGateSet(ctx, input.Facility, input.RelatedCode, gateCodes)
		if err != nil {
			return model.OperationDirective{}, err
		}
		links = buildGateLinks(gates)
	} else {
		gate, err := s.gates.GetByCode(ctx, input.RelatedCode)
		if err != nil {
			return model.OperationDirective{}, fmt.Errorf("linked gate %q: %w", input.RelatedCode, err)
		}
		if !strings.EqualFold(strings.TrimSpace(input.Facility), gate.Facility) || gate.Status == string(constants.GateStateLocked) {
			return model.OperationDirective{}, fmt.Errorf("%w: linked gate is locked or belongs to another facility", ErrInvalidInput)
		}
	}
	gateState := strings.TrimSpace(input.GateState)
	if gateState == "" {
		gateState = string(constants.GateStateClosed)
	}
	item := model.OperationDirective{
		BaseModel: model.BaseModel{
			Code: strings.ToUpper(strings.TrimSpace(input.Code)), Name: strings.TrimSpace(input.Name),
			Status: model.OperationDirectiveInitialStatus, Version: 1, Description: strings.TrimSpace(input.Description),
		},
		Facility: strings.TrimSpace(input.Facility), Owner: strings.TrimSpace(input.Owner),
		Category: strings.TrimSpace(input.Category), RiskLevel: input.RiskLevel,
		MetricValue: input.MetricValue, MetricUnit: strings.TrimSpace(input.MetricUnit),
		EffectiveAt: input.EffectiveAt.UTC(), Evidence: strings.TrimSpace(input.Evidence),
		RelatedCode: strings.ToUpper(strings.TrimSpace(input.RelatedCode)), GateState: gateState,
	}
	detail := "created 操作指令"
	if len(links) > 0 {
		detail = fmt.Sprintf("created joint 操作指令 with gates %s", strings.Join(gateCodes, ", "))
	}
	if err := s.security.WithinTransaction(ctx, func(txCtx context.Context) error {
		if len(links) > 0 {
			if err := s.repository.CreateWithGates(txCtx, &item, links); err != nil {
				return err
			}
		} else if err := s.repository.Create(txCtx, &item); err != nil {
			return err
		}
		return s.security.Audit(txCtx, actor, requestID, "create", "OperationDirective", item.ID, "", item.Status, detail)
	}); err != nil {
		return model.OperationDirective{}, fmt.Errorf("create 操作指令: %w", err)
	}
	if err := s.hydrateGateStates(ctx, &item); err != nil {
		return model.OperationDirective{}, err
	}
	return item, nil
}

func (s *operationDirectiveService) Update(ctx context.Context, id uint, input dto.UpdateOperationDirective, actor, requestID string) (model.OperationDirective, error) {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return model.OperationDirective{}, err
	}
	if current.Status != string(constants.DirectiveStateDraft) {
		return model.OperationDirective{}, ErrImmutableState
	}
	if err := validateOperationDirectiveBusinessFields(current.Code, input.Name, input.Facility, input.Owner); err != nil {
		return model.OperationDirective{}, err
	}
	gateCodes := normalizeGateCodes(input.GateCodes)
	var links []model.DirectiveGate
	if len(gateCodes) > 0 {
		gates, err := s.validateJointGateSet(ctx, input.Facility, input.RelatedCode, gateCodes)
		if err != nil {
			return model.OperationDirective{}, err
		}
		links = buildGateLinks(gates)
	} else {
		gate, err := s.gates.GetByCode(ctx, input.RelatedCode)
		if err != nil {
			return model.OperationDirective{}, fmt.Errorf("linked gate %q: %w", input.RelatedCode, err)
		}
		if !strings.EqualFold(strings.TrimSpace(input.Facility), gate.Facility) || gate.Status == string(constants.GateStateLocked) {
			return model.OperationDirective{}, fmt.Errorf("%w: linked gate is locked or belongs to another facility", ErrInvalidInput)
		}
		if current.JointDispatch() && !containsCode(current.GateCodes(), strings.ToUpper(strings.TrimSpace(input.RelatedCode))) {
			return model.OperationDirective{}, fmt.Errorf("%w: primary gate must stay within the existing joint gate list", ErrInvalidInput)
		}
	}
	current.Name = strings.TrimSpace(input.Name)
	current.Description = strings.TrimSpace(input.Description)
	current.Facility = strings.TrimSpace(input.Facility)
	current.Owner = strings.TrimSpace(input.Owner)
	current.Category = strings.TrimSpace(input.Category)
	current.RiskLevel = input.RiskLevel
	current.MetricValue = input.MetricValue
	current.MetricUnit = strings.TrimSpace(input.MetricUnit)
	current.EffectiveAt = input.EffectiveAt.UTC()
	current.Evidence = strings.TrimSpace(input.Evidence)
	current.RelatedCode = strings.ToUpper(strings.TrimSpace(input.RelatedCode))
	if gateState := strings.TrimSpace(input.GateState); gateState != "" {
		current.GateState = gateState
	}
	current.Version = input.ExpectedVersion + 1
	current.UpdatedAt = time.Now().UTC()
	if err := s.security.WithinTransaction(ctx, func(txCtx context.Context) error {
		if err := s.repository.Update(txCtx, id, input.ExpectedVersion, &current); err != nil {
			return err
		}
		if len(links) > 0 {
			if err := s.repository.ReplaceGates(txCtx, id, links); err != nil {
				return err
			}
		}
		return s.security.Audit(txCtx, actor, requestID, "update", "OperationDirective", id, current.Status, current.Status, "updated business fields")
	}); err != nil {
		return model.OperationDirective{}, fmt.Errorf("update 操作指令: %w", err)
	}
	return s.Get(ctx, id)
}

func (s *operationDirectiveService) Transition(ctx context.Context, id uint, input dto.TransitionRequest, actor, role, requestID string) (model.OperationDirective, error) {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return model.OperationDirective{}, err
	}
	target := strings.TrimSpace(input.Status)
	if !constants.CanTransition(constants.OperationDirectiveTransitions, current.Status, target) {
		return model.OperationDirective{}, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, current.Status, target)
	}
	if target == string(constants.DirectiveStateCompleted) {
		return model.OperationDirective{}, fmt.Errorf("%w: completion is recorded by an execution confirmation", ErrInvalidTransition)
	}
	if !directiveRoleAllowed(current.Status, target, role) {
		return model.OperationDirective{}, ErrForbidden
	}
	var gate *model.GateUnit
	var gateTarget string
	var jointGates []model.GateUnit
	var jointTarget string
	movesGates := target == string(constants.DirectiveStateExecuting) ||
		(current.Status == string(constants.DirectiveStateExecuting) && target == string(constants.DirectiveStateAborted))
	if movesGates && current.JointDispatch() {
		planned, plannedTarget, planErr := s.planJointTransition(ctx, &current, target)
		if planErr != nil {
			return model.OperationDirective{}, planErr
		}
		jointGates, jointTarget = planned, plannedTarget
	} else if movesGates {
		linkedGate, gateErr := s.gates.GetByCode(ctx, current.RelatedCode)
		if gateErr != nil {
			return model.OperationDirective{}, fmt.Errorf("linked gate %q: %w", current.RelatedCode, gateErr)
		}
		if target == string(constants.DirectiveStateExecuting) && linkedGate.Status == string(constants.GateStateLocked) {
			return model.OperationDirective{}, fmt.Errorf("%w: locked gate cannot execute a directive", ErrInvalidInput)
		}
		gate = &linkedGate
		if target == string(constants.DirectiveStateAborted) {
			gateTarget = string(constants.GateStateLocked)
		} else if linkedGate.Status != current.GateState {
			gateTarget = string(constants.GateStateMoving)
		}
		if gateTarget != "" && linkedGate.Status != gateTarget && !constants.CanTransition(constants.GateUnitTransitions, linkedGate.Status, gateTarget) {
			return model.OperationDirective{}, fmt.Errorf("%w: gate %s cannot move from %s to %s", ErrInvalidTransition, linkedGate.Code, linkedGate.Status, gateTarget)
		}
	}
	if target == string(constants.DirectiveStateApproved) {
		if current.SubmittedBy == "" || strings.EqualFold(current.SubmittedBy, actor) {
			return model.OperationDirective{}, ErrTwoPersonRequired
		}
	}
	before := current.Status
	now := time.Now().UTC()
	current.Status = target
	var approval *model.DirectiveApproval
	if target == string(constants.DirectiveStatePending) {
		current.SubmittedBy = actor
		current.SubmittedAt = &now
		approval = &model.DirectiveApproval{Stage: "submitted", Actor: actor, Role: role, RequestID: requestID,
			Reason: strings.TrimSpace(input.Reason), FromState: before, ToState: target, CreatedAt: now}
	}
	if target == string(constants.DirectiveStateApproved) {
		current.ApprovedBy = actor
		current.ApprovedAt = &now
		approval = &model.DirectiveApproval{Stage: "approved", Actor: actor, Role: role, RequestID: requestID,
			Reason: strings.TrimSpace(input.Reason), FromState: before, ToState: target, CreatedAt: now}
	}
	current.Version = input.ExpectedVersion + 1
	current.UpdatedAt = now
	audit := &model.AuditLog{Actor: actor, RequestID: requestID, Action: "transition", EntityType: "OperationDirective", EntityID: id,
		BeforeState: before, AfterState: target, Detail: input.Reason, CreatedAt: now}
	if err := s.security.WithinTransaction(ctx, func(txCtx context.Context) error {
		if err := s.repository.TransitionWithApproval(txCtx, id, input.ExpectedVersion, &current, approval, audit); err != nil {
			return err
		}
		for index := range jointGates {
			linked := &jointGates[index]
			if linked.Status == jointTarget {
				continue
			}
			gateBefore := linked.Status
			linked.Status = jointTarget
			linked.Version++
			linked.UpdatedAt = now
			if err := s.gates.Update(txCtx, linked.ID, linked.Version-1, linked); err != nil {
				if errors.Is(err, repository.ErrVersionConflict) {
					return &GateConflictError{Conflicts: []GateConflict{{Code: linked.Code, Reason: "version conflict"}}}
				}
				return err
			}
			if err := s.security.Audit(txCtx, actor, requestID, "directive_execution", "GateUnit", linked.ID, gateBefore, jointTarget, input.Reason); err != nil {
				return err
			}
		}
		if gate == nil || gateTarget == "" || gate.Status == gateTarget {
			return nil
		}
		gateBefore := gate.Status
		gate.Status = gateTarget
		gate.Version++
		gate.UpdatedAt = now
		if err := s.gates.Update(txCtx, gate.ID, gate.Version-1, gate); err != nil {
			return err
		}
		return s.security.Audit(txCtx, actor, requestID, "directive_execution", "GateUnit", gate.ID, gateBefore, gateTarget, input.Reason)
	}); err != nil {
		return model.OperationDirective{}, fmt.Errorf("transition 操作指令: %w", err)
	}
	return s.Get(ctx, id)
}

func (s *operationDirectiveService) Delete(ctx context.Context, id uint, actor, requestID string) error {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return err
	}
	if current.Status != string(constants.DirectiveStateDraft) {
		return ErrImmutableState
	}
	return s.security.WithinTransaction(ctx, func(txCtx context.Context) error {
		if err := s.repository.Delete(txCtx, id); err != nil {
			return err
		}
		return s.security.Audit(txCtx, actor, requestID, "delete", "OperationDirective", id, current.Status, "deleted", "soft deleted 操作指令")
	})
}

// planJointTransition loads the whole joint gate set and either returns the
// gates in link order with their common target state, or rejects the entire
// step with every conflicting gate listed.
func (s *operationDirectiveService) planJointTransition(ctx context.Context, current *model.OperationDirective, target string) ([]model.GateUnit, string, error) {
	codes := current.GateCodes()
	gateTarget := string(constants.GateStateMoving)
	if target == string(constants.DirectiveStateAborted) {
		gateTarget = string(constants.GateStateLocked)
	}
	gates, err := s.gates.GetByCodes(ctx, codes)
	if err != nil {
		return nil, "", err
	}
	byCode := make(map[string]model.GateUnit, len(gates))
	for _, item := range gates {
		byCode[item.Code] = item
	}
	busy := map[string]bool{}
	if gateTarget == string(constants.GateStateMoving) {
		occupied, err := s.repository.ExecutingGateConflicts(ctx, codes, current.ID)
		if err != nil {
			return nil, "", err
		}
		for _, code := range occupied {
			busy[code] = true
		}
	}
	conflicts := make([]GateConflict, 0)
	seen := map[string]bool{}
	addConflict := func(code, reason string) {
		if !seen[code] {
			seen[code] = true
			conflicts = append(conflicts, GateConflict{Code: code, Reason: reason})
		}
	}
	ordered := make([]model.GateUnit, 0, len(codes))
	moving := gateTarget == string(constants.GateStateMoving)
	for _, code := range codes {
		gate, ok := byCode[code]
		if !ok {
			addConflict(code, "gate not found")
			continue
		}
		ordered = append(ordered, gate)
		switch {
		case moving && gate.Status == string(constants.GateStateLocked):
			addConflict(code, "locked")
		case moving && busy[code]:
			addConflict(code, "already bound to an executing directive")
		case gate.Status == gateTarget:
			// A gate already moving at execute time belongs to an untracked
			// operation, so the whole group must be rejected.
			if moving {
				addConflict(code, "already moving")
			}
		case !constants.CanTransition(constants.GateUnitTransitions, gate.Status, gateTarget):
			addConflict(code, fmt.Sprintf("cannot move from %s to %s", gate.Status, gateTarget))
		}
	}
	if len(conflicts) > 0 {
		sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].Code < conflicts[j].Code })
		return nil, "", &GateConflictError{Conflicts: conflicts}
	}
	return ordered, gateTarget, nil
}

// validateJointGateSet enforces the joint dispatch contract: two to five
// distinct gates of one facility, none locked, primary gate included.
func (s *operationDirectiveService) validateJointGateSet(ctx context.Context, facility, relatedCode string, gateCodes []string) ([]model.GateUnit, error) {
	if len(gateCodes) < 2 || len(gateCodes) > 5 {
		return nil, fmt.Errorf("%w: joint dispatch requires 2 to 5 gates, got %d", ErrInvalidInput, len(gateCodes))
	}
	related := strings.ToUpper(strings.TrimSpace(relatedCode))
	if !containsCode(gateCodes, related) {
		return nil, fmt.Errorf("%w: primary gate %s must be part of the joint gate list", ErrInvalidInput, related)
	}
	gates, err := s.gates.GetByCodes(ctx, gateCodes)
	if err != nil {
		return nil, err
	}
	byCode := make(map[string]model.GateUnit, len(gates))
	for _, gate := range gates {
		byCode[gate.Code] = gate
	}
	missing := make([]string, 0)
	for _, code := range gateCodes {
		if _, ok := byCode[code]; !ok {
			missing = append(missing, code)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: unknown gates: %s", ErrInvalidInput, strings.Join(missing, ", "))
	}
	ordered := make([]model.GateUnit, 0, len(gateCodes))
	for _, code := range gateCodes {
		gate := byCode[code]
		if !strings.EqualFold(strings.TrimSpace(facility), gate.Facility) {
			return nil, fmt.Errorf("%w: gate %s belongs to another facility", ErrInvalidInput, gate.Code)
		}
		if gate.Status == string(constants.GateStateLocked) {
			return nil, fmt.Errorf("%w: gate %s is locked", ErrInvalidInput, gate.Code)
		}
		ordered = append(ordered, gate)
	}
	return ordered, nil
}

// hydrateGateStates attaches the live gate states so list and detail reads
// always reflect the persisted gate rows after a refresh.
func (s *operationDirectiveService) hydrateGateStates(ctx context.Context, items ...*model.OperationDirective) error {
	codes := make([]string, 0)
	seen := map[string]bool{}
	for _, item := range items {
		linked := item.GateCodes()
		if len(linked) == 0 && item.RelatedCode != "" {
			linked = []string{item.RelatedCode}
		}
		for _, code := range linked {
			if !seen[code] {
				seen[code] = true
				codes = append(codes, code)
			}
		}
	}
	if len(codes) == 0 {
		return nil
	}
	gates, err := s.gates.GetByCodes(ctx, codes)
	if err != nil {
		return err
	}
	statusByCode := make(map[string]string, len(gates))
	for _, gate := range gates {
		statusByCode[gate.Code] = gate.Status
	}
	for _, item := range items {
		linked := item.GateCodes()
		if len(linked) == 0 && item.RelatedCode != "" {
			linked = []string{item.RelatedCode}
		}
		snapshots := make([]model.GateStateSnapshot, 0, len(linked))
		for _, code := range linked {
			if status, ok := statusByCode[code]; ok {
				snapshots = append(snapshots, model.GateStateSnapshot{Code: code, Status: status})
			}
		}
		item.GateStates = snapshots
	}
	return nil
}

func buildGateLinks(gates []model.GateUnit) []model.DirectiveGate {
	links := make([]model.DirectiveGate, 0, len(gates))
	for _, gate := range gates {
		links = append(links, model.DirectiveGate{GateID: gate.ID, GateCode: gate.Code})
	}
	return links
}

func normalizeGateCodes(codes []string) []string {
	normalized := make([]string, 0, len(codes))
	seen := map[string]bool{}
	for _, code := range codes {
		trimmed := strings.ToUpper(strings.TrimSpace(code))
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		normalized = append(normalized, trimmed)
	}
	return normalized
}

func containsCode(codes []string, code string) bool {
	for _, item := range codes {
		if item == code {
			return true
		}
	}
	return false
}

func directiveRoleAllowed(from, target, role string) bool {
	switch target {
	case string(constants.DirectiveStatePending):
		return role == model.RoleOperator || role == model.RoleAdmin
	case string(constants.DirectiveStateApproved):
		return role == model.RoleReviewer || role == model.RoleAdmin
	case string(constants.DirectiveStateExecuting):
		return role == model.RoleOperator || role == model.RoleAdmin
	case string(constants.DirectiveStateAborted):
		return role == model.RoleOperator || role == model.RoleReviewer || role == model.RoleAdmin
	default:
		return false
	}
}

func (s *operationDirectiveService) StatusCounts(ctx context.Context) (map[string]int64, error) {
	return s.repository.CountByStatus(ctx)
}

func validateOperationDirectiveBusinessFields(code, name, facility, owner string) error {
	if strings.TrimSpace(code) == "" || strings.TrimSpace(name) == "" || strings.TrimSpace(facility) == "" || strings.TrimSpace(owner) == "" {
		return ErrInvalidInput
	}
	return nil
}
