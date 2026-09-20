import { request } from './client';
import type { JointDispatchOrder } from '../types/domain';

export async function listJointDispatches(page = 1, pageSize = 20, search = '') {
  return request<JointDispatchOrder[]>(`/joint-dispatches?page=${page}&pageSize=${pageSize}&search=${encodeURIComponent(search)}`);
}
export async function createJointDispatch(input: Record<string, unknown>) {
  return request<JointDispatchOrder>('/joint-dispatches', { method: 'POST', body: JSON.stringify(input) });
}
export async function transitionJointDispatch(id: number, status: string, expectedVersion: number, reason: string) {
  return request<JointDispatchOrder>(`/joint-dispatches/${id}/transition`, {
    method: 'POST', body: JSON.stringify({ status, expectedVersion, reason }),
  });
}
