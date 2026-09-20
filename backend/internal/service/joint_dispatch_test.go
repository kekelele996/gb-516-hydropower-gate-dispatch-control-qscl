package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blueship581/hydropower-gate-dispatch-control/backend/internal/config"
	"github.com/blueship581/hydropower-gate-dispatch-control/backend/internal/dto"
	"github.com/blueship581/hydropower-gate-dispatch-control/backend/internal/model"
	"github.com/blueship581/hydropower-gate-dispatch-control/backend/internal/repository"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

const jointFacility = "联合坝段"

func newJointWorkflow(t *testing.T) (OperationDirectiveService, ExecutionConfirmationService, repository.GateUnitRepository, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&model.GateUnit{}, &model.OperationDirective{}, &model.DirectiveGate{}, &model.DirectiveApproval{}, &model.ExecutionConfirmation{}, &model.AuditLog{}); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	gateRepo := repository.NewGateUnitRepository(db)
	directiveRepo := repository.NewOperationDirectiveRepository(db)
	confirmationRepo := repository.NewExecutionConfirmationRepository(db)
	security := NewSecurityService(repository.NewSecurityRepository(db), config.Config{})
	directives := NewOperationDirectiveService(directiveRepo, gateRepo, security)
	confirmations := NewExecutionConfirmationService(confirmationRepo, directiveRepo, gateRepo, security)
	for index := 1; index <= 5; index++ {
		gate := model.GateUnit{BaseModel: model.BaseModel{Code: fmt.Sprintf("GU-J%d", index), Name: fmt.Sprintf("联合闸门%d", index), Status: "open", Version: 1}, Facility: jointFacility, Owner: "运行一组"}
		if err := gateRepo.Create(context.Background(), &gate); err != nil {
			t.Fatalf("create joint gate: %v", err)
		}
	}
	other := model.GateUnit{BaseModel: model.BaseModel{Code: "GU-OTHER", Name: "异区闸门", Status: "open", Version: 1}, Facility: "其他坝段", Owner: "运行一组"}
	if err := gateRepo.Create(context.Background(), &other); err != nil {
		t.Fatalf("create foreign gate: %v", err)
	}
	return directives, confirmations, gateRepo, db
}

func jointDirectiveInput(code string, gateCodes []string) dto.CreateOperationDirective {
	return dto.CreateOperationDirective{
		Code: code, Name: "闸门联合调度许可", Description: "验证联合调度闸门组",
		Facility: jointFacility, Owner: "运行一组", Category: "泄洪调度", RiskLevel: "high",
		MetricValue: 40, MetricUnit: "%", EffectiveAt: time.Now().UTC().Add(time.Hour),
		Evidence: "联合闸门状态、闭锁与通信已核对", RelatedCode: gateCodes[0], GateState: "closed", GateCodes: gateCodes,
	}
}

func createJointDirective(t *testing.T, directives OperationDirectiveService, code string, gateCodes []string) model.OperationDirective {
	t.Helper()
	created, err := directives.Create(context.Background(), jointDirectiveInput(code, gateCodes), "operator", "req-joint-create")
	if err != nil {
		t.Fatalf("create joint directive: %v", err)
	}
	return created
}

func advanceJointDirective(t *testing.T, directives OperationDirectiveService, item model.OperationDirective, status, actor, role string) model.OperationDirective {
	t.Helper()
	updated, err := directives.Transition(context.Background(), item.ID, dto.TransitionRequest{
		Status: status, ExpectedVersion: item.Version, Reason: "联合调度流程推进",
	}, actor, role, "req-joint-"+status)
	if err != nil {
		t.Fatalf("transition joint directive to %s: %v", status, err)
	}
	return updated
}

func prepareApprovedJointDirective(t *testing.T, directives OperationDirectiveService, code string, gateCodes []string) model.OperationDirective {
	t.Helper()
	created := createJointDirective(t, directives, code, gateCodes)
	submitted := advanceJointDirective(t, directives, created, "pending", "operator", model.RoleOperator)
	return advanceJointDirective(t, directives, submitted, "approved", "reviewer", model.RoleReviewer)
}

func gateStatuses(t *testing.T, gates repository.GateUnitRepository, codes []string) map[string]string {
	t.Helper()
	items, err := gates.GetByCodes(context.Background(), codes)
	if err != nil {
		t.Fatalf("load gates: %v", err)
	}
	statuses := make(map[string]string, len(items))
	for _, item := range items {
		statuses[item.Code] = item.Status
	}
	return statuses
}

func TestJointDirectiveCreateValidatesGateSet(t *testing.T) {
	directives, _, gates, _ := newJointWorkflow(t)
	ctx := context.Background()

	locked := model.GateUnit{BaseModel: model.BaseModel{Code: "GU-LOCKED", Name: "闭锁闸门", Status: "locked", Version: 1}, Facility: jointFacility, Owner: "运行一组"}
	if err := gates.Create(ctx, &locked); err != nil {
		t.Fatalf("create locked gate: %v", err)
	}

	cases := []struct {
		name      string
		related   string
		gateCodes []string
		want      string
	}{
		{"single gate is not a joint group", "GU-J1", []string{"GU-J1"}, "2 to 5 gates"},
		{"more than five gates", "GU-J1", []string{"GU-J1", "GU-J2", "GU-J3", "GU-J4", "GU-J5", "GU-LOCKED"}, "2 to 5 gates"},
		{"primary gate missing from list", "GU-J1", []string{"GU-J2", "GU-J3"}, "must be part of the joint gate list"},
		{"unknown gate code", "GU-J1", []string{"GU-J1", "GU-MISSING"}, "unknown gates: GU-MISSING"},
		{"gate from another facility", "GU-J1", []string{"GU-J1", "GU-OTHER"}, "belongs to another facility"},
		{"locked gate cannot join", "GU-J1", []string{"GU-J1", "GU-LOCKED"}, "is locked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := jointDirectiveInput("OD-JOINT-BAD", tc.gateCodes)
			input.RelatedCode = tc.related
			_, err := directives.Create(ctx, input, "operator", "req-joint-invalid")
			if err == nil || !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected invalid input containing %q, got %v", tc.want, err)
			}
		})
	}

	created, err := directives.Create(ctx, jointDirectiveInput("OD-JOINT-OK", []string{"gu-j1 ", "GU-J2", "gu-j1", "GU-J3"}), "operator", "req-joint-dedupe")
	if err != nil {
		t.Fatalf("joint create with duplicated codes should succeed after normalization: %v", err)
	}
	if len(created.Gates) != 3 || len(created.GateStates) != 3 {
		t.Fatalf("expected three deduplicated gate links, got gates=%d states=%d", len(created.Gates), len(created.GateStates))
	}
	for index, code := range []string{"GU-J1", "GU-J2", "GU-J3"} {
		if created.Gates[index].GateCode != code || created.GateStates[index].Code != code || created.GateStates[index].Status != "open" {
			t.Fatalf("gate links not persisted in order: %#v / %#v", created.Gates, created.GateStates)
		}
	}
}

func TestJointExecuteMovesAllGatesAtomically(t *testing.T) {
	directives, _, gates, db := newJointWorkflow(t)
	codes := []string{"GU-J1", "GU-J2", "GU-J3"}
	approved := prepareApprovedJointDirective(t, directives, "OD-JOINT-EXEC", codes)

	executing := advanceJointDirective(t, directives, approved, "executing", "operator", model.RoleOperator)
	if executing.Status != "executing" {
		t.Fatalf("directive should be executing, got %s", executing.Status)
	}
	statuses := gateStatuses(t, gates, codes)
	for _, code := range codes {
		if statuses[code] != "moving" {
			t.Fatalf("gate %s should be moving, got %s", code, statuses[code])
		}
	}
	if len(executing.GateStates) != 3 {
		t.Fatalf("executing directive should expose gate states, got %#v", executing.GateStates)
	}
	for _, snapshot := range executing.GateStates {
		if snapshot.Status != "moving" {
			t.Fatalf("read-back gate state should be moving, got %#v", snapshot)
		}
	}
	var gateAudits int64
	if err := db.Model(&model.AuditLog{}).Where("entity_type = ? AND action = ? AND after_state = ?", "GateUnit", "directive_execution", "moving").Count(&gateAudits).Error; err != nil {
		t.Fatalf("count gate audits: %v", err)
	}
	if gateAudits != 3 {
		t.Fatalf("expected one moving audit per joint gate, got %d", gateAudits)
	}
}

func TestJointExecuteRejectsAndListsConflictingGates(t *testing.T) {
	directives, _, gates, _ := newJointWorkflow(t)
	ctx := context.Background()

	// GU-J3 joins the directive while open and is locked afterwards; GU-J2 is
	// bound to another executing directive.
	approved := prepareApprovedJointDirective(t, directives, "OD-JOINT-REJECT", []string{"GU-J1", "GU-J2", "GU-J3"})
	locked, err := gates.GetByCode(ctx, "GU-J3")
	if err != nil {
		t.Fatalf("load gate: %v", err)
	}
	locked.Status = "locked"
	locked.Version++
	if err := gates.Update(ctx, locked.ID, locked.Version-1, &locked); err != nil {
		t.Fatalf("lock gate: %v", err)
	}
	busy := prepareApprovedJointDirective(t, directives, "OD-JOINT-BUSY", []string{"GU-J2", "GU-J4"})
	advanceJointDirective(t, directives, busy, "executing", "operator", model.RoleOperator)

	_, err = directives.Transition(ctx, approved.ID, dto.TransitionRequest{
		Status: "executing", ExpectedVersion: approved.Version, Reason: "存在冲突闸门必须整次拒绝",
	}, "operator", model.RoleOperator, "req-joint-reject")
	var conflict *GateConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected gate conflict error, got %v", err)
	}
	if len(conflict.Conflicts) != 2 || conflict.Conflicts[0].Code != "GU-J2" || conflict.Conflicts[1].Code != "GU-J3" {
		t.Fatalf("conflicts must list GU-J2 and GU-J3 sorted, got %#v", conflict.Conflicts)
	}
	if conflict.Conflicts[0].Reason != "already bound to an executing directive" || conflict.Conflicts[1].Reason != "locked" {
		t.Fatalf("conflict reasons not preserved: %#v", conflict.Conflicts)
	}

	stored, getErr := directives.Get(ctx, approved.ID)
	if getErr != nil {
		t.Fatalf("reload directive: %v", getErr)
	}
	if stored.Status != "approved" {
		t.Fatalf("rejected execute must keep directive approved, got %s", stored.Status)
	}
	statuses := gateStatuses(t, gates, []string{"GU-J1", "GU-J2", "GU-J3"})
	if statuses["GU-J1"] != "open" || statuses["GU-J2"] != "moving" || statuses["GU-J3"] != "locked" {
		t.Fatalf("rejected execute must not move any gate: %#v", statuses)
	}
}

// flakyGateRepository fails the update of one specific gate with a version
// conflict so the joint execute path can be tested deterministically.
type flakyGateRepository struct {
	repository.GateUnitRepository
	failOnCode string
}

func (r *flakyGateRepository) Update(ctx context.Context, id, version uint, item *model.GateUnit) error {
	if item.Code == r.failOnCode {
		return repository.ErrVersionConflict
	}
	return r.GateUnitRepository.Update(ctx, id, version, item)
}

func TestJointExecuteVersionConflictRejectsWholeGroup(t *testing.T) {
	directives, _, gateRepo, db := newJointWorkflow(t)
	flaky := &flakyGateRepository{GateUnitRepository: gateRepo, failOnCode: "GU-J2"}
	directiveRepo := repository.NewOperationDirectiveRepository(db)
	security := NewSecurityService(repository.NewSecurityRepository(db), config.Config{})
	directives = NewOperationDirectiveService(directiveRepo, flaky, security)
	ctx := context.Background()
	codes := []string{"GU-J1", "GU-J2", "GU-J3"}
	approved := prepareApprovedJointDirective(t, directives, "OD-JOINT-VCONFLICT", codes)

	_, err := directives.Transition(ctx, approved.ID, dto.TransitionRequest{
		Status: "executing", ExpectedVersion: approved.Version, Reason: "版本冲突必须整次回滚",
	}, "operator", model.RoleOperator, "req-joint-vconflict")
	var conflict *GateConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected gate conflict error, got %v", err)
	}
	if len(conflict.Conflicts) != 1 || conflict.Conflicts[0].Code != "GU-J2" || conflict.Conflicts[0].Reason != "version conflict" {
		t.Fatalf("version conflict must list the failed gate, got %#v", conflict.Conflicts)
	}
	stored, _ := directives.Get(ctx, approved.ID)
	if stored.Status != "approved" {
		t.Fatalf("directive must stay approved after rollback, got %s", stored.Status)
	}
	statuses := gateStatuses(t, gateRepo, codes)
	for _, code := range codes {
		if statuses[code] != "open" {
			t.Fatalf("gate %s must stay open after whole-group rollback, got %s", code, statuses[code])
		}
	}
}

func TestJointExecuteSucceedsOnlyOnceUnderConcurrency(t *testing.T) {
	directives, _, gates, _ := newJointWorkflow(t)
	codes := []string{"GU-J1", "GU-J2", "GU-J3", "GU-J4", "GU-J5"}
	approved := prepareApprovedJointDirective(t, directives, "OD-JOINT-RACE", codes)

	const attempts = 8
	var successes int64
	var wait sync.WaitGroup
	wait.Add(attempts)
	for index := 0; index < attempts; index++ {
		go func() {
			defer wait.Done()
			_, err := directives.Transition(context.Background(), approved.ID, dto.TransitionRequest{
				Status: "executing", ExpectedVersion: approved.Version, Reason: "并发执行只能成功一次",
			}, "operator", model.RoleOperator, "req-joint-race")
			if err == nil {
				atomic.AddInt64(&successes, 1)
			}
		}()
	}
	wait.Wait()
	if successes != 1 {
		t.Fatalf("exactly one concurrent execute may succeed, got %d", successes)
	}
	stored, err := directives.Get(context.Background(), approved.ID)
	if err != nil {
		t.Fatalf("reload directive: %v", err)
	}
	if stored.Status != "executing" {
		t.Fatalf("directive should be executing, got %s", stored.Status)
	}
	statuses := gateStatuses(t, gates, codes)
	for _, code := range codes {
		if statuses[code] != "moving" {
			t.Fatalf("gate %s should be moving exactly once, got %s", code, statuses[code])
		}
	}
}

func TestJointConfirmationSettlesAllGates(t *testing.T) {
	directives, confirmations, gates, _ := newJointWorkflow(t)
	ctx := context.Background()
	codes := []string{"GU-J1", "GU-J2"}
	approved := prepareApprovedJointDirective(t, directives, "OD-JOINT-SETTLE", codes)
	executing := advanceJointDirective(t, directives, approved, "executing", "operator", model.RoleOperator)

	receipt, err := confirmations.Create(ctx, dto.CreateExecutionConfirmation{
		Code: "EC-JOINT-SETTLE", Name: "联合执行回执", Description: "联合闸门同时落定",
		Facility: jointFacility, Owner: "运行一组", Category: "执行回执", RiskLevel: "high",
		MetricValue: 40, MetricUnit: "%", EffectiveAt: time.Now().UTC(), Evidence: "全部闸门开度反馈一致", RelatedCode: executing.Code,
	}, "operator", "req-joint-receipt")
	if err != nil {
		t.Fatalf("create joint confirmation: %v", err)
	}
	if _, err := confirmations.Transition(ctx, receipt.ID, dto.TransitionRequest{
		Status: "confirmed", ExpectedVersion: receipt.Version, Reason: "联合闸门全部到达目标开度",
	}, "operator", "req-joint-confirm"); err != nil {
		t.Fatalf("confirm joint receipt: %v", err)
	}
	stored, _ := directives.Get(ctx, executing.ID)
	if stored.Status != "completed" {
		t.Fatalf("directive should complete with the receipt, got %s", stored.Status)
	}
	statuses := gateStatuses(t, gates, codes)
	for _, code := range codes {
		if statuses[code] != "closed" {
			t.Fatalf("gate %s should settle at the target state closed, got %s", code, statuses[code])
		}
	}
	if len(stored.GateStates) != 2 || stored.GateStates[0].Status != "closed" || stored.GateStates[1].Status != "closed" {
		t.Fatalf("read-back gate states must reflect the settled gates: %#v", stored.GateStates)
	}
}

func TestJointFailedConfirmationLocksAllGates(t *testing.T) {
	directives, confirmations, gates, _ := newJointWorkflow(t)
	ctx := context.Background()
	codes := []string{"GU-J1", "GU-J2", "GU-J3"}
	approved := prepareApprovedJointDirective(t, directives, "OD-JOINT-FAIL", codes)
	executing := advanceJointDirective(t, directives, approved, "executing", "operator", model.RoleOperator)

	receipt, err := confirmations.Create(ctx, dto.CreateExecutionConfirmation{
		Code: "EC-JOINT-FAIL", Name: "联合执行失败回执", Description: "执行失败全部闭锁",
		Facility: jointFacility, Owner: "运行一组", Category: "执行回执", RiskLevel: "critical",
		MetricValue: 40, MetricUnit: "%", EffectiveAt: time.Now().UTC(), Evidence: "2号闸门开度反馈异常", RelatedCode: executing.Code,
	}, "operator", "req-joint-fail-receipt")
	if err != nil {
		t.Fatalf("create joint confirmation: %v", err)
	}
	if _, err := confirmations.Transition(ctx, receipt.ID, dto.TransitionRequest{
		Status: "failed", ExpectedVersion: receipt.Version, Reason: "执行失败，全部闸门闭锁",
	}, "operator", "req-joint-fail"); err != nil {
		t.Fatalf("fail joint receipt: %v", err)
	}
	stored, _ := directives.Get(ctx, executing.ID)
	if stored.Status != "aborted" {
		t.Fatalf("directive should abort on failed receipt, got %s", stored.Status)
	}
	statuses := gateStatuses(t, gates, codes)
	for _, code := range codes {
		if statuses[code] != "locked" {
			t.Fatalf("gate %s should lock on failed receipt, got %s", code, statuses[code])
		}
	}
}

func TestJointAbortLocksAllGates(t *testing.T) {
	directives, _, gates, _ := newJointWorkflow(t)
	codes := []string{"GU-J1", "GU-J2"}
	approved := prepareApprovedJointDirective(t, directives, "OD-JOINT-ABORT", codes)
	executing := advanceJointDirective(t, directives, approved, "executing", "operator", model.RoleOperator)

	aborted := advanceJointDirective(t, directives, executing, "aborted", "reviewer", model.RoleReviewer)
	if aborted.Status != "aborted" {
		t.Fatalf("directive should be aborted, got %s", aborted.Status)
	}
	statuses := gateStatuses(t, gates, codes)
	for _, code := range codes {
		if statuses[code] != "locked" {
			t.Fatalf("gate %s should lock when the executing directive aborts, got %s", code, statuses[code])
		}
	}
}

func TestJointDraftUpdateReplacesGateSet(t *testing.T) {
	directives, _, _, _ := newJointWorkflow(t)
	ctx := context.Background()
	created := createJointDirective(t, directives, "OD-JOINT-EDIT", []string{"GU-J1", "GU-J2"})

	updated, err := directives.Update(ctx, created.ID, dto.UpdateOperationDirective{
		ExpectedVersion: created.Version, Name: "调整后联合调度", Description: "扩展联动闸门组",
		Facility: jointFacility, Owner: "运行一组", Category: "泄洪调度", RiskLevel: "high",
		MetricValue: 45, MetricUnit: "%", EffectiveAt: time.Now().UTC().Add(2 * time.Hour),
		Evidence: "联动闸门组已扩展并复核", RelatedCode: "GU-J1", GateState: "closed",
		GateCodes: []string{"GU-J1", "GU-J3", "GU-J4"},
	}, "operator", "req-joint-edit")
	if err != nil {
		t.Fatalf("update joint draft: %v", err)
	}
	codes := updated.GateCodes()
	if len(codes) != 3 || codes[0] != "GU-J1" || codes[1] != "GU-J3" || codes[2] != "GU-J4" {
		t.Fatalf("gate links should be replaced, got %#v", codes)
	}

	submitted := advanceJointDirective(t, directives, updated, "pending", "operator", model.RoleOperator)
	_, err = directives.Update(ctx, submitted.ID, dto.UpdateOperationDirective{
		ExpectedVersion: submitted.Version, Name: "提交后禁止编辑", Facility: jointFacility, Owner: "运行一组",
		Category: "泄洪调度", RiskLevel: "high", MetricValue: 45, MetricUnit: "%",
		EffectiveAt: time.Now().UTC().Add(2 * time.Hour), Evidence: "提交后不可修改", RelatedCode: "GU-J1",
	}, "operator", "req-joint-edit-late")
	if !errors.Is(err, ErrImmutableState) {
		t.Fatalf("submitted joint directive must be immutable, got %v", err)
	}
}

func TestJointCreateRollsBackWhenAuditCannotPersist(t *testing.T) {
	directives, _, _, db := newJointWorkflow(t)
	if err := db.Migrator().DropTable(&model.AuditLog{}); err != nil {
		t.Fatalf("drop audit table: %v", err)
	}
	_, err := directives.Create(context.Background(), jointDirectiveInput("OD-JOINT-ROLLBACK", []string{"GU-J1", "GU-J2"}), "operator", "req-joint-rollback")
	if err == nil {
		t.Fatal("joint create should fail when audit cannot persist")
	}
	var directiveCount, linkCount int64
	if err := db.Model(&model.OperationDirective{}).Where("code = ?", "OD-JOINT-ROLLBACK").Count(&directiveCount).Error; err != nil {
		t.Fatalf("count directives: %v", err)
	}
	if err := db.Model(&model.DirectiveGate{}).Count(&linkCount).Error; err != nil {
		t.Fatalf("count gate links: %v", err)
	}
	if directiveCount != 0 || linkCount != 0 {
		t.Fatalf("joint create must roll back directive and links, got directives=%d links=%d", directiveCount, linkCount)
	}
}
