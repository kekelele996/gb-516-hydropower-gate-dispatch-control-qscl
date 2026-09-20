<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue';
import { Plus, Refresh, Search } from '@element-plus/icons-vue';
import { request } from '../api/client';
import type { DomainRecord, JointDispatchOrder } from '../types/domain';
import { useJointDispatchStore } from '../stores/joint-dispatch';
import { allowedTransitions } from '../types/status';
import { formatDate, riskLabel, statusLabel } from '../utils/format';
import { useAuth } from '../hooks/useAuth';
import { usePolling } from '../hooks/usePolling';
import StatusBadge from '../components/common/StatusBadge.vue';
import GateStateBadge from '../components/common/GateStateBadge.vue';
import MetricCard from '../components/common/MetricCard.vue';
import ConfirmDialog from '../components/common/ConfirmDialog.vue';

const store = useJointDispatchStore();
const { session, can } = useAuth();
const search = ref('');
const showCreate = ref(false);
const reservoirs = ref<DomainRecord[]>([]);
const gateOptions = ref<DomainRecord[]>([]);
const pending = ref<{ item: JointDispatchOrder; status: string } | null>(null);
const transitionReason = ref('');

const createForm = reactive({
  code: '', name: '', description: '', owner: '', category: '联合调度', riskLevel: 'medium',
  evidence: '', reservoirCode: '', targetState: 'open', gateCodes: [] as string[],
});

const executingCount = computed(() => store.items.filter((item) => item.status === 'executing').length);
const coveredGates = computed(() => new Set(store.items.flatMap((item) => item.gates.map((gate) => gate.gateCode))).size);
const reservoirGates = computed(() => gateOptions.value.filter((gate) => gate.relatedCode === createForm.reservoirCode));
const createReady = computed(() => Boolean(
  createForm.code.trim() && createForm.name.trim() && createForm.owner.trim() && createForm.category.trim() &&
  createForm.evidence.trim() && createForm.reservoirCode &&
  createForm.gateCodes.length >= 2 && createForm.gateCodes.length <= 5,
));

async function load(): Promise<void> {
  await store.load(search.value);
}

onMounted(() => void load());
usePolling(load, 30_000);

async function prepareCreate(): Promise<void> {
  Object.assign(createForm, {
    code: `JD-${String(Date.now()).slice(-6)}`, name: '', description: '', category: '联合调度',
    riskLevel: 'medium', evidence: '', targetState: 'open', gateCodes: [] as string[],
    owner: session.value?.displayName || '现场操作员', reservoirCode: '',
  });
  store.conflicts = [];
  try {
    const [reservoirResult, gateResult] = await Promise.all([
      request<DomainRecord[]>('/reservoirs?page=1&pageSize=100'),
      request<DomainRecord[]>('/gates?page=1&pageSize=100'),
    ]);
    reservoirs.value = reservoirResult.data;
    gateOptions.value = gateResult.data;
    createForm.reservoirCode = reservoirs.value[0]?.code || '';
    showCreate.value = true;
  } catch (reason) {
    store.error = reason instanceof Error ? reason.message : String(reason);
  }
}

function selectReservoir(code: string): void {
  createForm.reservoirCode = code;
  createForm.gateCodes = [];
}

function toggleGate(code: string): void {
  const index = createForm.gateCodes.indexOf(code);
  if (index >= 0) {
    createForm.gateCodes.splice(index, 1);
  } else if (createForm.gateCodes.length < 5) {
    createForm.gateCodes.push(code);
  }
}

async function createOrder(): Promise<void> {
  if (!createReady.value) {
    store.error = '请选择同一库区的 2-5 个闸门，并完整填写必填业务字段和现场证据';
    return;
  }
  await store.createOrder({ ...createForm, effectiveAt: new Date().toISOString() });
  if (!store.error) showCreate.value = false;
}

function transitionsFor(item: JointDispatchOrder): readonly string[] {
  return allowedTransitions('jointDispatch', item.status).filter((target) => {
    if (target === 'pending') return can('operator', 'admin');
    if (target === 'approved') return can('reviewer', 'admin') && item.submittedBy !== session.value?.username;
    if (target === 'executing' || target === 'completed' || target === 'failed') return can('operator', 'admin');
    if (target === 'aborted') return can('operator', 'reviewer', 'admin');
    return false;
  });
}

function selectTransition(item: JointDispatchOrder, status: string): void {
  store.conflicts = [];
  pending.value = { item, status };
  const gateText = `${item.gates.length} 个闸门`;
  transitionReason.value = status === 'approved'
    ? '已复核闸门清单、水位窗口、设备闭锁和现场证据'
    : status === 'executing'
      ? `双人许可完成，${gateText}原子进入动作中`
      : status === 'completed'
        ? `现场回执确认${gateText}全部到位`
        : status === 'failed'
          ? `现场回执报告执行失败，${gateText}同时闭锁`
          : `值班人员确认将状态由 ${item.status} 推进至 ${status}`;
}

async function confirmTransition(): Promise<void> {
  if (!pending.value || transitionReason.value.trim().length < 3) return;
  await store.transition(pending.value.item, pending.value.status, transitionReason.value.trim());
  if (!store.error) pending.value = null;
}
</script>

<template>
  <main class="workspace">
    <header class="page-header">
      <div>
        <p class="eyebrow">业务工作台</p>
        <h1>闸门联合调度许可</h1>
        <p>一条指令关联同库区 2-5 个闸门，提交复核后全部闸门原子进入动作中，回执同时落定。</p>
      </div>
      <el-button v-if="can('operator', 'admin')" type="primary" :icon="Plus" @click="prepareCreate">新增联合调度</el-button>
    </header>

    <section class="metrics" aria-label="业务统计">
      <MetricCard label="许可总数" :value="store.meta.total" detail="当前筛选范围" />
      <MetricCard label="执行中" :value="executingCount" detail="闸门处于动作中" />
      <MetricCard label="覆盖闸门" :value="coveredGates" detail="当前页涉及闸门" />
    </section>

    <section class="toolbar" aria-label="筛选工具栏">
      <el-input v-model="search" :prefix-icon="Search" placeholder="搜索联合调度编码或名称" clearable @keyup.enter="load" />
      <el-button type="primary" :icon="Search" @click="load">查询</el-button>
      <el-button :icon="Refresh" @click="search = ''; load()">重置</el-button>
    </section>
    <el-alert v-if="store.error" :title="store.error" type="error" show-icon closable @close="store.error = ''; store.conflicts = []">
      <template v-if="store.conflicts.length">
        <ul class="conflict-list">
          <li v-for="conflict in store.conflicts" :key="conflict.gateCode">
            <strong>{{ conflict.gateCode }}</strong>：{{ conflict.reason }}
          </li>
        </ul>
      </template>
    </el-alert>

    <section class="table-shell">
      <el-table v-loading="store.loading" :data="store.items" empty-text="暂无符合条件的记录">
        <el-table-column prop="code" label="编码" width="120" />
        <el-table-column label="名称" min-width="180">
          <template #default="{ row }"><strong>{{ row.name }}</strong><small>{{ row.reservoirCode }} · {{ row.facility }}</small></template>
        </el-table-column>
        <el-table-column label="闸门清单" min-width="230">
          <template #default="{ row }">
            <div class="gate-chip-list">
              <span v-for="gate in row.gates" :key="gate.id" class="gate-chip">
                <strong>{{ gate.gateCode }}</strong>
                <GateStateBadge v-if="gate.currentStatus" :state="gate.currentStatus" />
              </span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="目标状态" width="110">
          <template #default="{ row }"><GateStateBadge :state="row.targetState" /></template>
        </el-table-column>
        <el-table-column label="整体状态" width="120">
          <template #default="{ row }"><StatusBadge :status="row.status" /></template>
        </el-table-column>
        <el-table-column label="双人许可" min-width="130">
          <template #default="{ row }">
            <small v-if="row.submittedBy">提交 {{ row.submittedBy }}</small>
            <small v-if="row.approvedBy">复核 {{ row.approvedBy }}</small>
            <span v-if="!row.submittedBy" class="muted">待提交</span>
          </template>
        </el-table-column>
        <el-table-column label="风险" width="80"><template #default="{ row }">{{ riskLabel(row.riskLevel) }}</template></el-table-column>
        <el-table-column label="更新时间" width="165"><template #default="{ row }">{{ formatDate(row.updatedAt) }}</template></el-table-column>
        <el-table-column label="操作" min-width="220" fixed="right">
          <template #default="{ row }">
            <div v-if="transitionsFor(row).length" class="row-actions">
              <el-button v-for="target in transitionsFor(row)" :key="target" link type="primary" @click="selectTransition(row, target)">推进至{{ statusLabel(target) }}</el-button>
            </div>
            <span v-else class="muted">当前角色无可执行动作</span>
          </template>
        </el-table-column>
      </el-table>
    </section>

    <ConfirmDialog v-model="showCreate" title="新增闸门联合调度许可" confirm-label="创建许可" :confirm-disabled="!createReady" :loading="store.loading" @confirm="createOrder">
      <el-form class="record-form" label-position="top">
        <el-alert v-if="!reservoirGates.length" title="当前库区没有可选闸门" type="warning" show-icon />
        <div class="form-grid">
          <el-form-item label="业务编码"><el-input v-model="createForm.code" /></el-form-item>
          <el-form-item label="名称"><el-input v-model="createForm.name" /></el-form-item>
          <el-form-item label="所属库区">
            <el-select :model-value="createForm.reservoirCode" @update:model-value="selectReservoir">
              <el-option v-for="item in reservoirs" :key="item.id" :label="`${item.code} · ${item.name}`" :value="item.code" />
            </el-select>
          </el-form-item>
          <el-form-item label="目标闸门状态">
            <el-select v-model="createForm.targetState">
              <el-option v-for="state in ['open', 'closed']" :key="state" :label="statusLabel(state)" :value="state" />
            </el-select>
          </el-form-item>
          <el-form-item label="责任人"><el-input v-model="createForm.owner" /></el-form-item>
          <el-form-item label="业务类别"><el-input v-model="createForm.category" /></el-form-item>
          <el-form-item label="风险等级">
            <el-select v-model="createForm.riskLevel">
              <el-option v-for="risk in ['low', 'medium', 'high', 'critical']" :key="risk" :label="riskLabel(risk)" :value="risk" />
            </el-select>
          </el-form-item>
        </div>
        <el-form-item :label="`联动闸门（同库区 2-5 个，已选 ${createForm.gateCodes.length} 个）`">
          <div class="gate-picker">
            <button
              v-for="gate in reservoirGates" :key="gate.id" type="button"
              :class="['gate-option', { selected: createForm.gateCodes.includes(gate.code) }]"
              :disabled="!createForm.gateCodes.includes(gate.code) && createForm.gateCodes.length >= 5"
              @click="toggleGate(gate.code)"
            >
              <strong>{{ gate.code }}</strong><span>{{ gate.name }}</span>
              <GateStateBadge :state="gate.status" />
            </button>
          </div>
        </el-form-item>
        <el-form-item label="业务说明"><el-input v-model="createForm.description" type="textarea" :rows="2" maxlength="1000" show-word-limit /></el-form-item>
        <el-form-item label="现场证据"><el-input v-model="createForm.evidence" type="textarea" :rows="3" /></el-form-item>
      </el-form>
    </ConfirmDialog>

    <ConfirmDialog :model-value="Boolean(pending)" title="确认状态迁移" confirm-label="确认并记录审计" @update:model-value="pending = null" @confirm="confirmTransition">
      <p>此次操作会校验角色、版本与全部联动闸门状态，任一闸门冲突将整次拒绝并列出清单。</p>
      <div class="transition-summary"><StatusBadge :status="pending?.item.status || ''" /><span>到</span><StatusBadge :status="pending?.status || ''" /></div>
      <div v-if="pending" class="gate-chip-list transition-gates">
        <span v-for="gate in pending.item.gates" :key="gate.id" class="gate-chip">
          <strong>{{ gate.gateCode }}</strong>
          <GateStateBadge v-if="gate.currentStatus" :state="gate.currentStatus" />
        </span>
      </div>
      <el-input v-model="transitionReason" type="textarea" :rows="3" maxlength="500" show-word-limit aria-label="迁移原因" />
    </ConfirmDialog>
  </main>
</template>
