package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/blueship581/hydropower-gate-dispatch-control/backend/internal/config"
	"github.com/blueship581/hydropower-gate-dispatch-control/backend/internal/dto"
	"github.com/blueship581/hydropower-gate-dispatch-control/backend/internal/model"
	"github.com/blueship581/hydropower-gate-dispatch-control/backend/internal/repository"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func newJointDispatchService(t *testing.T) (JointDispatchService, *gorm.DB) {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("database handle: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&model.GateUnit{}, &model.OperationDirective{}, &model.JointDispatchOrder{}, &model.JointDispatchGate{}, &model.AuditLog{}); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	gates := []model.GateUnit{
		{BaseModel: model.BaseModel{Code: "GU-A", Name: "左岸一号闸", Status: "closed", Version: 1}, Facility: "左岸坝段", Owner: "运行一组", RelatedCode: "R-1"},
		{BaseModel: model.BaseModel{Code: "GU-B", Name: "左岸二号闸", Status: "closed", Version: 1}, Facility: "左岸坝段", Owner: "运行一组", RelatedCode: "R-1"},
		{BaseModel: model.BaseModel{Code: "GU-C", Name: "左岸三号闸", Status: "open", Version: 1}, Facility: "左岸坝段", Owner: "运行一组", RelatedCode: "R-1"},
		{BaseModel: model.BaseModel{Code: "GU-X", Name: "右岸一号闸", Status: "closed", Version: 1}, Facility: "右岸坝段", Owner: "运行二组", RelatedCode: "R-2"},
	}
	for i := range gates {
		if err := db.Create(&gates[i]).Error; err != nil {
			t.Fatalf("create test gate: %v", err)
		}
	}
	security := NewSecurityService(repository.NewSecurityRepository(db), config.Config{})
	return NewJointDispatchService(
		repository.NewJointDispatchRepository(db),
		repository.NewGateUnitRepository(db),
		repository.NewOperationDirectiveRepository(db),
		security,
	), db
}

func jointInput(code string, gateCodes ...string) dto.CreateJointDispatchOrder {
	return dto.CreateJointDispatchOrder{
		Code: code, Name: "左岸闸群联合调度", Description: "测试联合调度许可",
		Owner: "运行一组", Category: "联合调度", RiskLevel: "high",
		EffectiveAt: time.Now().UTC().Add(time.Hour), Evidence: "水位窗口与闸门状态已核对",
		TargetState: "open", GateCodes: gateCodes,
	}
}

func submitAndApprove(t *testing.T, service JointDispatchService, id uint, version uint) model.JointDispatchOrder {
	t.Helper()
	ctx := context.Background()
	submitted, err := service.Transition(ctx, id, dto.TransitionRequest{
		Status: "pending", ExpectedVersion: version, Reason: "提交联合调度复核",
	}, "operator", model.RoleOperator, "req-jd-submit")
	if err != nil {
		t.Fatalf("submit joint dispatch: %v", err)
	}
	approved, err := service.Transition(ctx, id, dto.TransitionRequest{
		Status: "approved", ExpectedVersion: submitted.Version, Reason: "复核闸门清单与水位窗口一致",
	}, "reviewer", model.RoleReviewer, "req-jd-approve")
	if err != nil {
		t.Fatalf("approve joint dispatch: %v", err)
	}
	return approved
}

func gateStatus(t *testing.T, db *gorm.DB, code string) (string, uint) {
	t.Helper()
	var gate model.GateUnit
	if err := db.Where("code = ?", code).First(&gate).Error; err != nil {
		t.Fatalf("load gate %s: %v", code, err)
	}
	return gate.Status, gate.Version
}

func TestJointDispatchValidatesGateGroup(t *testing.T) {
	service, _ := newJointDispatchService(t)
	ctx := context.Background()
	cases := []struct {
		name  string
		gates []string
	}{
		{"single gate rejected", []string{"GU-A"}},
		{"six gates rejected", []string{"GU-A", "GU-B", "GU-C", "GU-X", "GU-A2", "GU-A3"}},
		{"duplicate gates rejected", []string{"GU-A", "GU-A"}},
		{"cross reservoir rejected", []string{"GU-A", "GU-X"}},
		{"unknown gate rejected", []string{"GU-A", "GU-UNKNOWN"}},
	}
	for _, tc := range cases {
		if _, err := service.Create(ctx, jointInput("JD-CHECK", tc.gates...), "operator", "req-create"); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%s: expected ErrInvalidInput, got %v", tc.name, err)
		}
	}
	created, err := service.Create(ctx, jointInput("JD-OK", "GU-A", "gu-b", "GU-C"), "operator", "req-create")
	if err != nil {
		t.Fatalf("create with 3 same-reservoir gates: %v", err)
	}
	if len(created.Gates) != 3 || created.ReservoirCode != "R-1" || created.Facility != "左岸坝段" {
		t.Fatalf("gate group not persisted as snapshot: %#v", created)
	}
	for _, gate := range created.Gates {
		if gate.GateVersion != 1 || gate.GateCode == "" || gate.GateName == "" {
			t.Fatalf("gate snapshot incomplete: %#v", gate)
		}
	}
}

func TestJointDispatchExecutesAndSettlesAtomically(t *testing.T) {
	service, db := newJointDispatchService(t)
	ctx := context.Background()
	created, err := service.Create(ctx, jointInput("JD-FLOW", "GU-A", "GU-B"), "operator", "req-create")
	if err != nil {
		t.Fatalf("create joint dispatch: %v", err)
	}
	approved := submitAndApprove(t, service, created.ID, created.Version)

	executing, err := service.Transition(ctx, approved.ID, dto.TransitionRequest{
		Status: "executing", ExpectedVersion: approved.Version, Reason: "双人许可完成，全部闸门进入动作中",
	}, "operator", model.RoleOperator, "req-jd-execute")
	if err != nil {
		t.Fatalf("execute joint dispatch: %v", err)
	}
	if executing.Status != "executing" || executing.ExecutedBy != "operator" {
		t.Fatalf("execution evidence missing: %#v", executing)
	}
	for _, code := range []string{"GU-A", "GU-B"} {
		if status, _ := gateStatus(t, db, code); status != "moving" {
			t.Fatalf("gate %s should be moving after atomic execution, got %s", code, status)
		}
		if status, _ := gateStatus(t, db, "GU-C"); status != "open" {
			t.Fatalf("unrelated gate GU-C must stay untouched, got %s", status)
		}
	}

	completed, err := service.Transition(ctx, executing.ID, dto.TransitionRequest{
		Status: "completed", ExpectedVersion: executing.Version, Reason: "现场回执确认全部闸门到位",
	}, "operator", model.RoleOperator, "req-jd-complete")
	if err != nil {
		t.Fatalf("complete joint dispatch: %v", err)
	}
	if completed.Status != "completed" || completed.SettledBy != "operator" || completed.SettledAt == nil {
		t.Fatalf("settlement evidence missing: %#v", completed)
	}
	for _, code := range []string{"GU-A", "GU-B"} {
		if status, _ := gateStatus(t, db, code); status != "open" {
			t.Fatalf("gate %s should settle to target open, got %s", code, status)
		}
	}
	var gateAudits int64
	if err := db.Model(&model.AuditLog{}).Where("action IN ? AND entity_type = ?", []string{"joint_execution", "joint_settlement"}, "GateUnit").Count(&gateAudits).Error; err != nil {
		t.Fatalf("count gate audits: %v", err)
	}
	if gateAudits != 4 {
		t.Fatalf("expected 2 execution + 2 settlement gate audits, got %d", gateAudits)
	}
}

func TestJointDispatchExecuteRejectsConflictingGates(t *testing.T) {
	service, db := newJointDispatchService(t)
	ctx := context.Background()
	created, err := service.Create(ctx, jointInput("JD-CONFLICT", "GU-A", "GU-B", "GU-C"), "operator", "req-create")
	if err != nil {
		t.Fatalf("create joint dispatch: %v", err)
	}
	approved := submitAndApprove(t, service, created.ID, created.Version)

	// GU-A locked, GU-B version drifted, GU-C occupied by an active directive.
	if err := db.Model(&model.GateUnit{}).Where("code = ?", "GU-A").Updates(map[string]any{"status": "locked", "version": 2}).Error; err != nil {
		t.Fatalf("lock gate: %v", err)
	}
	if err := db.Model(&model.GateUnit{}).Where("code = ?", "GU-B").Update("version", 7).Error; err != nil {
		t.Fatalf("drift gate version: %v", err)
	}
	directive := model.OperationDirective{
		BaseModel: model.BaseModel{Code: "OD-BUSY", Name: "占用闸门的执行指令", Status: "executing", Version: 1},
		Facility:  "左岸坝段", Owner: "运行一组", RelatedCode: "GU-C", GateState: "open",
	}
	if err := db.Create(&directive).Error; err != nil {
		t.Fatalf("create occupying directive: %v", err)
	}

	_, err = service.Transition(ctx, approved.ID, dto.TransitionRequest{
		Status: "executing", ExpectedVersion: approved.Version, Reason: "应被整次拒绝并列出冲突闸门",
	}, "operator", model.RoleOperator, "req-jd-conflict")
	var conflictErr *GateConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("expected GateConflictError, got %v", err)
	}
	byGate := make(map[string]string, len(conflictErr.Conflicts))
	for _, conflict := range conflictErr.Conflicts {
		byGate[conflict.GateCode] = conflict.Reason
	}
	if byGate["GU-A"] != "已闭锁" {
		t.Fatalf("GU-A lock conflict missing: %#v", conflictErr.Conflicts)
	}
	if byGate["GU-B"] == "" {
		t.Fatalf("GU-B version conflict missing: %#v", conflictErr.Conflicts)
	}
	if byGate["GU-C"] != "已有执行中指令 OD-BUSY" {
		t.Fatalf("GU-C busy-directive conflict missing: %#v", conflictErr.Conflicts)
	}

	stored, getErr := service.Get(ctx, approved.ID)
	if getErr != nil {
		t.Fatalf("reload rejected order: %v", getErr)
	}
	if stored.Status != "approved" || stored.Version != approved.Version {
		t.Fatalf("rejected execution must not advance the order: %#v", stored)
	}
	if status, _ := gateStatus(t, db, "GU-B"); status != "closed" {
		t.Fatalf("rejected execution must not move any gate, GU-B is %s", status)
	}
}

func TestJointDispatchExecutionSucceedsOnlyOnce(t *testing.T) {
	service, db := newJointDispatchService(t)
	ctx := context.Background()
	created, err := service.Create(ctx, jointInput("JD-ONCE", "GU-A", "GU-B"), "operator", "req-create")
	if err != nil {
		t.Fatalf("create joint dispatch: %v", err)
	}
	approved := submitAndApprove(t, service, created.ID, created.Version)

	const attempts = 4
	var wg sync.WaitGroup
	results := make(chan error, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := service.Transition(ctx, approved.ID, dto.TransitionRequest{
				Status: "executing", ExpectedVersion: approved.Version, Reason: "并发执行只允许成功一次",
			}, "operator", model.RoleOperator, "req-jd-race")
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("concurrent execution must succeed exactly once, got %d", succeeded)
	}
	_, version := gateStatus(t, db, "GU-A")
	if version != 2 {
		t.Fatalf("gate version must advance exactly once, got v%d", version)
	}

	stored, err := service.Get(ctx, approved.ID)
	if err != nil {
		t.Fatalf("reload executing order: %v", err)
	}
	if _, err = service.Transition(ctx, stored.ID, dto.TransitionRequest{
		Status: "completed", ExpectedVersion: stored.Version, Reason: "第一次完成回执落定全部闸门",
	}, "operator", model.RoleOperator, "req-jd-settle-1"); err != nil {
		t.Fatalf("first settlement receipt: %v", err)
	}
	if _, err = service.Transition(ctx, stored.ID, dto.TransitionRequest{
		Status: "completed", ExpectedVersion: stored.Version, Reason: "重复回执不得再次落定",
	}, "operator", model.RoleOperator, "req-jd-settle-2"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("repeated settlement must be rejected, got %v", err)
	}
	if status, _ := gateStatus(t, db, "GU-B"); status != "open" {
		t.Fatalf("gate must settle exactly once to open, got %s", status)
	}
}

func TestJointDispatchFailureSettlesAllGatesLocked(t *testing.T) {
	service, db := newJointDispatchService(t)
	ctx := context.Background()
	created, err := service.Create(ctx, jointInput("JD-FAIL", "GU-A", "GU-B"), "operator", "req-create")
	if err != nil {
		t.Fatalf("create joint dispatch: %v", err)
	}
	approved := submitAndApprove(t, service, created.ID, created.Version)
	executing, err := service.Transition(ctx, approved.ID, dto.TransitionRequest{
		Status: "executing", ExpectedVersion: approved.Version, Reason: "开始执行",
	}, "operator", model.RoleOperator, "req-jd-execute")
	if err != nil {
		t.Fatalf("execute joint dispatch: %v", err)
	}
	failed, err := service.Transition(ctx, executing.ID, dto.TransitionRequest{
		Status: "failed", ExpectedVersion: executing.Version, Reason: "现场回执报告闸门卡阻",
	}, "operator", model.RoleOperator, "req-jd-fail")
	if err != nil {
		t.Fatalf("fail joint dispatch: %v", err)
	}
	if failed.Status != "failed" {
		t.Fatalf("order should be failed, got %s", failed.Status)
	}
	for _, code := range []string{"GU-A", "GU-B"} {
		if status, _ := gateStatus(t, db, code); status != "locked" {
			t.Fatalf("failure receipt must lock every gate, %s is %s", code, status)
		}
	}
}

func TestJointDispatchExecutionRollsBackWhenAuditFails(t *testing.T) {
	service, db := newJointDispatchService(t)
	ctx := context.Background()
	created, err := service.Create(ctx, jointInput("JD-ROLLBACK", "GU-A", "GU-B"), "operator", "req-create")
	if err != nil {
		t.Fatalf("create joint dispatch: %v", err)
	}
	approved := submitAndApprove(t, service, created.ID, created.Version)
	if err := db.Migrator().DropTable(&model.AuditLog{}); err != nil {
		t.Fatalf("drop audit table: %v", err)
	}
	if _, err = service.Transition(ctx, approved.ID, dto.TransitionRequest{
		Status: "executing", ExpectedVersion: approved.Version, Reason: "审计写入失败必须整体回滚",
	}, "operator", model.RoleOperator, "req-jd-rollback"); err == nil {
		t.Fatal("execution should fail when audit cannot persist")
	}
	for _, code := range []string{"GU-A", "GU-B"} {
		if status, _ := gateStatus(t, db, code); status != "closed" {
			t.Fatalf("rolled-back execution must leave %s closed, got %s", code, status)
		}
	}
	var stored model.JointDispatchOrder
	if err := db.First(&stored, approved.ID).Error; err != nil {
		t.Fatalf("reload rolled-back order: %v", err)
	}
	if stored.Status != "approved" || stored.Version != approved.Version {
		t.Fatalf("order must stay approved after rollback: %#v", stored)
	}
}

func TestJointDispatchRejectsGateHeldByAnotherExecutingOrder(t *testing.T) {
	service, _ := newJointDispatchService(t)
	ctx := context.Background()
	first, err := service.Create(ctx, jointInput("JD-FIRST", "GU-A", "GU-B"), "operator", "req-create-1")
	if err != nil {
		t.Fatalf("create first order: %v", err)
	}
	firstApproved := submitAndApprove(t, service, first.ID, first.Version)
	if _, err = service.Transition(ctx, firstApproved.ID, dto.TransitionRequest{
		Status: "executing", ExpectedVersion: firstApproved.Version, Reason: "第一条联合指令开始执行",
	}, "operator", model.RoleOperator, "req-jd-exec-1"); err != nil {
		t.Fatalf("execute first order: %v", err)
	}

	second, err := service.Create(ctx, jointInput("JD-SECOND", "GU-B", "GU-C"), "operator", "req-create-2")
	if err != nil {
		t.Fatalf("create second order: %v", err)
	}
	secondApproved := submitAndApprove(t, service, second.ID, second.Version)
	_, err = service.Transition(ctx, secondApproved.ID, dto.TransitionRequest{
		Status: "executing", ExpectedVersion: secondApproved.Version, Reason: "与执行中联合指令冲突",
	}, "operator", model.RoleOperator, "req-jd-exec-2")
	var conflictErr *GateConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("expected GateConflictError, got %v", err)
	}
	found := false
	for _, conflict := range conflictErr.Conflicts {
		if conflict.GateCode == "GU-B" && conflict.Reason == "联合指令 JD-FIRST 执行中" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected GU-B held-by-JD-FIRST conflict, got %#v", conflictErr.Conflicts)
	}
	stored, getErr := service.Get(ctx, secondApproved.ID)
	if getErr != nil {
		t.Fatalf("reload rejected order: %v", getErr)
	}
	if stored.Status != "approved" {
		t.Fatalf("second order must stay approved, got %s", stored.Status)
	}
}

func TestJointDispatchRequiresTwoPersonReview(t *testing.T) {
	service, _ := newJointDispatchService(t)
	ctx := context.Background()
	created, err := service.Create(ctx, jointInput("JD-TWO", "GU-A", "GU-B"), "operator", "req-create")
	if err != nil {
		t.Fatalf("create joint dispatch: %v", err)
	}
	submitted, err := service.Transition(ctx, created.ID, dto.TransitionRequest{
		Status: "pending", ExpectedVersion: created.Version, Reason: "提交复核",
	}, "operator", model.RoleOperator, "req-jd-submit")
	if err != nil {
		t.Fatalf("submit joint dispatch: %v", err)
	}
	if _, err = service.Transition(ctx, submitted.ID, dto.TransitionRequest{
		Status: "approved", ExpectedVersion: submitted.Version, Reason: "提交人不得自行复核",
	}, "operator", model.RoleReviewer, "req-jd-self"); !errors.Is(err, ErrTwoPersonRequired) {
		t.Fatalf("self approval must fail two-person rule, got %v", err)
	}
	if _, err = service.Transition(ctx, submitted.ID, dto.TransitionRequest{
		Status: "approved", ExpectedVersion: submitted.Version, Reason: "操作员无复核角色",
	}, "reviewer", model.RoleOperator, "req-jd-role"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("operator role must not approve, got %v", err)
	}
}
