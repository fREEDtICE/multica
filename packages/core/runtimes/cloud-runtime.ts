import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";

export interface CloudRuntimeDriverError {
  Code?: string;
  code?: string;
  Message?: string;
  message?: string;
  Actionable?: string;
  actionable?: string;
}

export interface CloudRuntimeNodeEvent {
  event: string;
  at: string;
  note: string;
}

export interface CloudRuntimeNode {
  id: string;
  owner_id: string;
  // Optional for backends that predate multi-vendor support; absent means AWS.
  provider?: string;
  account_id: string;
  role_arn: string;
  instance_id: string;
  region: string;
  instance_type: string;
  image_id: string;
  subnet_id: string;
  disk_size_gb?: number;
  name: string;
  status: string;
  // Administrator-facing phase: "provisioning" | "connecting" | "ready".
  stage: string;
  tags: Record<string, string>;
  metadata: Record<string, unknown>;
  error?: CloudRuntimeDriverError | null;
  events: CloudRuntimeNodeEvent[];
  created_at: string;
  updated_at: string;
}

export interface CloudRuntimeAccount {
  id: string;
  provider: string;
  label: string;
  role_arn: string;
  region: string;
  // "pending" | "verified" | "failed"
  status: string;
  identity: Record<string, string> | null;
  last_error?: CloudRuntimeDriverError | null;
  verified_at?: string | null;
  created_at: string;
  updated_at: string;
}

export interface CloudTrustBundle {
  provider: string;
  identity_present: boolean;
  identity?: Record<string, string>;
  trust_policy: string;
  permissions_policy: string;
  console_guide_url: string;
  role_arn_example: string;
}

export interface VerifyCloudAccountRequest {
  provider: string;
  role_arn: string;
  label?: string;
  region?: string;
}

export interface ListCloudRuntimeNodesParams {
  limit?: number;
  offset?: number;
}

export interface CreateCloudRuntimeNodeRequest {
  // "aws" (default) or "aliyun"; selects the built-in provisioning driver.
  provider?: string;
  // The connected, verified cloud account that authorizes the role.
  account_id?: string;
  instance_type: string;
  name?: string;
  region?: string;
  image_id?: string;
  subnet_id?: string;
  key_name?: string;
  iam_instance_profile?: string;
  disk_size_gb?: number;
  tags?: Record<string, string>;
}

export const CLOUD_PROVIDERS = [
  { value: "aws", label: "AWS EC2" },
  { value: "aliyun", label: "Alibaba Cloud ECS" },
] as const;

export type CloudProviderName = (typeof CLOUD_PROVIDERS)[number]["value"];

export const cloudRuntimeKeys = {
  all: (wsId: string) => ["cloud-runtime", wsId] as const,
  nodes: (wsId: string) => [...cloudRuntimeKeys.all(wsId), "nodes"] as const,
  accounts: (wsId: string) => [...cloudRuntimeKeys.all(wsId), "accounts"] as const,
  trust: (wsId: string, provider: string) =>
    [...cloudRuntimeKeys.all(wsId), "trust", provider] as const,
  cleanup: (wsId: string) => [...cloudRuntimeKeys.all(wsId), "cleanup"] as const,
  defaultRuntime: (wsId: string) =>
    [...cloudRuntimeKeys.all(wsId), "default-runtime"] as const,
};

const PENDING_NODE_STATUSES = new Set([
  "launching",
  "pending",
  "starting",
  "stopping",
  "rebooting",
  "terminating",
]);

export function isCloudRuntimeNodePending(status: string): boolean {
  return PENDING_NODE_STATUSES.has(status.toLowerCase());
}

export function isCloudRuntimeNodeFailed(node: Pick<CloudRuntimeNode, "status">): boolean {
  return ["failed", "cleanup_required", "error"].includes(node.status.toLowerCase());
}

// Normalizes the driver error across Go JSON (capitalized fields) and
// lowercase shapes so the UI has one readable message and one hint.
export function cloudRuntimeErrorMessage(
  error: CloudRuntimeDriverError | null | undefined,
): { message: string; actionable: string } {
  if (!error) return { message: "", actionable: "" };
  return {
    message: error.message || error.Message || "",
    actionable: error.actionable || error.Actionable || "",
  };
}

export function cloudRuntimeNodeListOptions(
  wsId: string,
  params?: ListCloudRuntimeNodesParams,
) {
  const limit = params?.limit ?? 50;
  const offset = params?.offset ?? 0;
  return queryOptions({
    queryKey: [...cloudRuntimeKeys.nodes(wsId), { limit, offset }] as const,
    queryFn: () => api.listCloudRuntimeNodes({ limit, offset }),
    refetchInterval: (query) =>
      query.state.data?.some((node) => isCloudRuntimeNodePending(node.status))
        ? 5000
        : false,
    staleTime: 15 * 1000,
  });
}

export function cloudAccountListOptions(wsId: string) {
  return queryOptions({
    queryKey: cloudRuntimeKeys.accounts(wsId),
    queryFn: () => api.listCloudAccounts(),
    staleTime: 15 * 1000,
  });
}

export function cloudTrustBundleOptions(wsId: string, provider: string) {
  return queryOptions({
    queryKey: cloudRuntimeKeys.trust(wsId, provider),
    queryFn: () => api.getCloudTrustBundle(provider),
    staleTime: 5 * 60 * 1000,
  });
}

export function cloudRuntimeCleanupOptions(wsId: string) {
  return queryOptions({
    queryKey: cloudRuntimeKeys.cleanup(wsId),
    queryFn: () => api.listCloudRuntimeCleanup(),
    refetchInterval: (query) =>
      (query.state.data?.length ?? 0) > 0 ? 10000 : false,
  });
}

export function cloudDefaultRuntimeOptions(wsId: string) {
  return queryOptions({
    queryKey: cloudRuntimeKeys.defaultRuntime(wsId),
    queryFn: () => api.getDefaultCloudRuntime(),
    staleTime: 30 * 1000,
  });
}

export function useCreateCloudRuntimeNode(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (data: CreateCloudRuntimeNodeRequest) =>
      api.createCloudRuntimeNode(data),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: cloudRuntimeKeys.all(wsId) });
    },
  });
}

export function useDeleteCloudRuntimeNode(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (instanceId: string) => api.deleteCloudRuntimeNode(instanceId),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: cloudRuntimeKeys.all(wsId) });
    },
  });
}

export function useVerifyCloudAccount(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (data: VerifyCloudAccountRequest) => api.verifyCloudAccount(data),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: cloudRuntimeKeys.accounts(wsId) });
    },
  });
}

export function useDeleteCloudAccount(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (accountId: string) => api.deleteCloudAccount(accountId),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: cloudRuntimeKeys.accounts(wsId) });
    },
  });
}

export function useCancelCloudRuntimeNode(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (nodeId: string) => api.cancelCloudRuntimeNode(nodeId),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: cloudRuntimeKeys.all(wsId) });
    },
  });
}

export function useRetryCloudRuntimeCleanup(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (nodeId: string) => api.retryCloudRuntimeCleanup(nodeId),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: cloudRuntimeKeys.all(wsId) });
    },
  });
}

export function useSetDefaultCloudRuntime(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (nodeId: string) => api.setDefaultCloudRuntime(nodeId),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: cloudRuntimeKeys.defaultRuntime(wsId) });
    },
  });
}
