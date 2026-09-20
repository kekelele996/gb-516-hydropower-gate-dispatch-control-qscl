import { defineStore } from 'pinia';
import { ApiError } from '../api/client';
import { createJointDispatch, listJointDispatches, transitionJointDispatch } from '../api/joint-dispatch';
import type { GateConflict, JointDispatchOrder, PageMeta } from '../types/domain';

// Dedicated store (not the generic factory) because joint dispatch surfaces
// the conflicting gate list when an execution is rejected as a whole.
export const useJointDispatchStore = defineStore('jointDispatch', {
  state: () => ({
    items: [] as JointDispatchOrder[],
    meta: { page: 1, pageSize: 20, total: 0 } as PageMeta,
    loading: false,
    error: '',
    conflicts: [] as GateConflict[],
    search: '',
  }),
  actions: {
    async load(search?: string) {
      this.loading = true;
      this.error = '';
      if (search !== undefined) this.search = search;
      try {
        const result = await listJointDispatches(1, 20, this.search);
        this.items = result.data;
        this.meta = result.meta || { page: 1, pageSize: 20, total: result.data.length };
      } catch (error) {
        this.error = error instanceof Error ? error.message : String(error);
      } finally {
        this.loading = false;
      }
    },
    async createOrder(input: Record<string, unknown>) {
      this.loading = true;
      this.error = '';
      try {
        await createJointDispatch(input);
        await this.load();
      } catch (error) {
        this.error = error instanceof Error ? error.message : String(error);
      } finally {
        this.loading = false;
      }
    },
    async transition(item: JointDispatchOrder, status: string, reason: string) {
      this.loading = true;
      this.error = '';
      this.conflicts = [];
      try {
        await transitionJointDispatch(item.id, status, item.version, reason);
        await this.load();
      } catch (error) {
        if (error instanceof ApiError) this.conflicts = error.conflicts;
        this.error = error instanceof Error ? error.message : String(error);
      } finally {
        this.loading = false;
      }
    },
  },
});
