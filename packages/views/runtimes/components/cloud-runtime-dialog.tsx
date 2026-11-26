"use client";

import { useMemo, useState } from "react";
import type { FormEvent } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  Cloud,
  Loader2,
  RefreshCw,
  Trash2,
  CheckCircle2,
  XCircle,
  Star,
  Ban,
} from "@multica/ui/icons";
import { toast } from "sonner";
import type { CloudRuntimeAccount, CloudRuntimeNode } from "@multica/core/runtimes";
import {
  CLOUD_PROVIDERS,
  cloudAccountListOptions,
  cloudDefaultRuntimeOptions,
  cloudRuntimeCleanupOptions,
  cloudRuntimeErrorMessage,
  cloudRuntimeNodeListOptions,
  isCloudRuntimeNodeFailed,
  isCloudRuntimeNodePending,
  useCancelCloudRuntimeNode,
  useCreateCloudRuntimeNode,
  useDeleteCloudRuntimeNode,
  useRetryCloudRuntimeCleanup,
  useSetDefaultCloudRuntime,
} from "@multica/core/runtimes";
import { useWorkspaceId } from "@multica/core/hooks";
import { Badge } from "@multica/ui/components/ui/badge";
import { Button } from "@multica/ui/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";
import { ConnectCloudAccountPanel } from "./cloud-account-connect";
import { CloudRuntimeConfigurePanel } from "./cloud-runtime-configure";

type WizardStep = "provider" | "connect" | "configure";

export function CloudRuntimeDialog({ onClose }: { onClose: () => void }) {
  const { t } = useT("runtimes");
  const wsId = useWorkspaceId();
  const [step, setStep] = useState<WizardStep>("provider");
  const [provider, setProvider] = useState<string>("");
  const [account, setAccount] = useState<CloudRuntimeAccount | null>(null);

  const nodesQuery = useQuery(cloudRuntimeNodeListOptions(wsId, { limit: 50, offset: 0 }));
  const accountsQuery = useQuery(cloudAccountListOptions(wsId));
  const cleanupQuery = useQuery(cloudRuntimeCleanupOptions(wsId));
  const defaultQuery = useQuery(cloudDefaultRuntimeOptions(wsId));

  const sortedNodes = useMemo(
    () =>
      (nodesQuery.data ?? []).toSorted(
        (a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime(),
      ),
    [nodesQuery.data],
  );

  const resetWizard = () => {
    setStep("provider");
    setProvider("");
    setAccount(null);
  };

  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
    >
      <DialogContent className="flex max-h-[88%] flex-col gap-0 p-0 sm:max-w-4xl">
        <DialogHeader className="border-b px-6 py-5">
          <DialogTitle className="flex items-center gap-2 text-title-sm">
            <Cloud className="h-4 w-4 text-muted-foreground" />
            {t(($) => $.cloud_runtime.title)}
          </DialogTitle>
          <DialogDescription className="text-caption">
            {t(($) => $.cloud_runtime.description)}
          </DialogDescription>
        </DialogHeader>

        <div className="min-h-0 flex-1 overflow-y-auto px-6 py-5">
          <div className="grid gap-5 lg:grid-cols-[minmax(0,1.05fr)_minmax(300px,0.95fr)]">
            <section className="space-y-4">
              {step === "provider" && (
                <ProviderStep
                  accounts={accountsQuery.data ?? []}
                  onPickProvider={(name) => {
                    setProvider(name);
                    const verified = (accountsQuery.data ?? []).filter(
                      (item) =>
                        item.provider === name && item.status === "verified",
                    );
                    if (verified.length > 0) {
                      setAccount(verified[0]!);
                      setStep("configure");
                    } else {
                      setStep("connect");
                    }
                  }}
                  onUseAccount={(item) => {
                    setProvider(item.provider);
                    setAccount(item);
                    setStep("configure");
                  }}
                  onConnectNew={(name) => {
                    setProvider(name);
                    setAccount(null);
                    setStep("connect");
                  }}
                />
              )}
              {step === "connect" && provider && (
                <ConnectCloudAccountPanel
                  provider={provider}
                  onBack={() => setStep("provider")}
                  onVerified={(item) => {
                    setAccount(item);
                    setStep("configure");
                  }}
                />
              )}
              {step === "configure" && provider && account && (
                <CloudRuntimeConfigurePanel
                  provider={provider}
                  account={account}
                  onBack={() => setStep("provider")}
                  onOpened={() => {
                    resetWizard();
                    void nodesQuery.refetch();
                  }}
                />
              )}
            </section>

            <section className="min-h-0 rounded-md border bg-muted/20">
              <div className="flex items-center justify-between border-b bg-background px-3 py-2.5">
                <h3 className="text-body font-medium">
                  {t(($) => $.cloud_runtime.nodes_title)}
                </h3>
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  onClick={() => void nodesQuery.refetch()}
                  disabled={nodesQuery.isFetching}
                  className="h-7 px-2"
                >
                  <RefreshCw
                    className={cn(
                      "h-3.5 w-3.5",
                      nodesQuery.isFetching && "animate-spin",
                    )}
                  />
                  {t(($) => $.cloud_runtime.refresh)}
                </Button>
              </div>

              {nodesQuery.isLoading ? (
                <div className="flex h-40 items-center justify-center">
                  <Loader2 className="h-5 w-5 animate-spin text-muted-foreground" />
                </div>
              ) : nodesQuery.isError ? (
                <div className="flex h-40 flex-col items-center justify-center px-5 text-center">
                  <p className="text-body font-medium">
                    {t(($) => $.cloud_runtime.nodes_failed)}
                  </p>
                  <p className="mt-1 text-caption text-muted-foreground">
                    {nodesQuery.error instanceof Error
                      ? nodesQuery.error.message
                      : t(($) => $.cloud_runtime.nodes_failed_hint)}
                  </p>
                </div>
              ) : sortedNodes.length === 0 ? (
                <div className="flex h-40 flex-col items-center justify-center px-5 text-center">
                  <Cloud className="h-7 w-7 text-faint-foreground" />
                  <p className="mt-3 text-body font-medium">
                    {t(($) => $.cloud_runtime.nodes_empty)}
                  </p>
                </div>
              ) : (
                <div className="max-h-[460px] space-y-2 overflow-y-auto p-2">
                  {(cleanupQuery.data ?? []).length > 0 && (
                    <CleanupBanner nodes={cleanupQuery.data ?? []} />
                  )}
                  {sortedNodes.map((node) => (
                    <CloudRuntimeNodeRow
                      key={node.id}
                      node={node}
                      isDefault={defaultQuery.data === node.id}
                    />
                  ))}
                </div>
              )}
            </section>
          </div>
        </div>

        <DialogFooter className="m-0 border-t bg-muted/30 px-6 py-3">
          <Button type="button" variant="outline" size="sm" onClick={onClose}>
            {t(($) => $.cloud_runtime.close)}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function ProviderStep({
  accounts,
  onPickProvider,
  onUseAccount,
  onConnectNew,
}: {
  accounts: CloudRuntimeAccount[];
  onPickProvider: (provider: string) => void;
  onUseAccount: (account: CloudRuntimeAccount) => void;
  onConnectNew: (provider: string) => void;
}) {
  const { t } = useT("runtimes");
  return (
    <div className="space-y-4">
      <h3 className="text-body font-medium">
        {t(($) => $.cloud_runtime.provider.select_title)}
      </h3>
      <div className="grid gap-3 sm:grid-cols-2">
        {CLOUD_PROVIDERS.map((item) => {
          const providerAccounts = accounts.filter(
            (account) => account.provider === item.value,
          );
          return (
            <div key={item.value} className="space-y-2 rounded-md border p-3">
              <Button
                type="button"
                variant="outline"
                className="h-auto w-full justify-start gap-2 py-3"
                onClick={() => onPickProvider(item.value)}
              >
                <Cloud className="h-4 w-4 text-muted-foreground" />
                <span className="text-body font-medium">{item.label}</span>
              </Button>
              {providerAccounts.length > 0 && (
                <div className="space-y-1.5">
                  <p className="text-micro text-muted-foreground">
                    {t(($) => $.cloud_runtime.provider.account_section)}
                  </p>
                  {providerAccounts.map((account) => (
                    <AccountRow
                      key={account.id}
                      account={account}
                      onUse={() => onUseAccount(account)}
                    />
                  ))}
                </div>
              )}
              <Button
                type="button"
                variant="ghost"
                size="sm"
                className="w-full"
                onClick={() => onConnectNew(item.value)}
              >
                {t(($) => $.cloud_runtime.provider.connect_new)}
              </Button>
            </div>
          );
        })}
      </div>
    </div>
  );
}

function AccountRow({
  account,
  onUse,
}: {
  account: CloudRuntimeAccount;
  onUse: () => void;
}) {
  const { t } = useT("runtimes");
  const verified = account.status === "verified";
  return (
    <div className="flex items-center gap-2 rounded-md border bg-background px-2.5 py-2">
      <div className="min-w-0 flex-1">
        <p className="truncate text-caption font-medium">
          {account.label || account.role_arn}
        </p>
        <code className="block truncate font-mono text-micro text-muted-foreground">
          {account.role_arn}
        </code>
      </div>
      {verified ? (
        <Badge variant="secondary" className="bg-success/10 text-success">
          <CheckCircle2 className="mr-1 h-3 w-3" />
          {t(($) => $.cloud_runtime.account.verified_badge)}
        </Badge>
      ) : (
        <Badge variant="secondary" className="bg-destructive/10 text-destructive">
          <XCircle className="mr-1 h-3 w-3" />
          {t(($) => $.cloud_runtime.account.failed_badge)}
        </Badge>
      )}
      {verified && (
        <Button type="button" size="sm" variant="outline" onClick={onUse}>
          {t(($) => $.cloud_runtime.provider.use_account)}
        </Button>
      )}
    </div>
  );
}

function CleanupBanner({ nodes }: { nodes: CloudRuntimeNode[] }) {
  const { t } = useT("runtimes");
  const retryCleanup = useRetryCloudRuntimeCleanup(useWorkspaceId());
  return (
    <div className="rounded-md border border-warning/40 bg-warning/5 px-3 py-2.5">
      <p className="text-caption font-medium text-warning">
        {t(($) => $.cloud_runtime.cleanup.title)} ({nodes.length})
      </p>
      <p className="mt-1 text-micro text-muted-foreground">
        {t(($) => $.cloud_runtime.cleanup.hint)}
      </p>
      {nodes.map((node) => (
        <div key={node.id} className="mt-2 flex items-center gap-2">
          <code className="min-w-0 flex-1 truncate font-mono text-micro">
            {node.instance_id || node.name}
          </code>
          <Button
            type="button"
            size="sm"
            variant="outline"
            disabled={retryCleanup.isPending}
            onClick={() => retryCleanup.mutate(node.id)}
          >
            {retryCleanup.isPending && <Loader2 className="h-3 w-3 animate-spin" />}
            {t(($) => $.cloud_runtime.cleanup.retry)}
          </Button>
        </div>
      ))}
    </div>
  );
}

function CloudRuntimeNodeRow({
  node,
  isDefault,
}: {
  node: CloudRuntimeNode;
  isDefault: boolean;
}) {
  const { t } = useT("runtimes");
  const wsId = useWorkspaceId();
  const deleteNode = useDeleteCloudRuntimeNode(wsId);
  const cancelNode = useCancelCloudRuntimeNode(wsId);
  const retryNode = useCreateCloudRuntimeNode(wsId);
  const setDefault = useSetDefaultCloudRuntime(wsId);
  const title =
    node.name.trim() ||
    node.instance_id.trim() ||
    t(($) => $.cloud_runtime.node_fallback_name);
  const failure = isCloudRuntimeNodeFailed(node)
    ? cloudRuntimeErrorMessage(node.error)
    : null;

  const handleRetry = (event: FormEvent) => {
    event.preventDefault();
    retryNode.mutate({
      provider: node.provider,
      account_id: node.account_id,
      name: node.name,
      instance_type: node.instance_type,
      region: node.region || undefined,
      subnet_id: node.subnet_id || undefined,
      disk_size_gb: node.disk_size_gb,
    });
  };

  return (
    <div className="rounded-md border bg-background px-3 py-2.5">
      <div className="flex min-w-0 items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="flex min-w-0 items-center gap-2">
            <span className="truncate text-body font-medium">{title}</span>
            <CloudRuntimeStatusBadge node={node} />
            {isDefault && (
              <Badge variant="secondary" className="h-5 gap-1 rounded-md px-1.5 text-micro">
                <Star className="h-3 w-3" />
                {t(($) => $.cloud_runtime.node.is_default)}
              </Badge>
            )}
          </div>
          <div className="mt-1 flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1 text-caption text-muted-foreground">
            <span>{node.instance_type}</span>
            <span className="text-faint-foreground">/</span>
            <span>{node.region}</span>
          </div>
        </div>
        <div className="flex shrink-0 items-center gap-1">
          {node.status === "running" && !isDefault && (
            <Button
              type="button"
              variant="ghost"
              size="sm"
              className="h-7 gap-1 px-2"
              onClick={() => setDefault.mutate(node.id)}
            >
              <Star className="h-3.5 w-3.5" />
              {t(($) => $.cloud_runtime.node.set_default)}
            </Button>
          )}
          {isCloudRuntimeNodePending(node.status) && (
            <Button
              type="button"
              variant="ghost"
              size="sm"
              className="h-7 gap-1 px-2 text-warning"
              onClick={() => {
                if (!confirm(t(($) => $.cloud_runtime.node.cancel_confirm))) return;
                cancelNode.mutate(node.id);
              }}
            >
              <Ban className="h-3.5 w-3.5" />
              {t(($) => $.cloud_runtime.node.cancel_launch)}
            </Button>
          )}
          {node.status === "failed" && (
            <Button
              type="button"
              variant="ghost"
              size="sm"
              className="h-7 gap-1 px-2"
              disabled={retryNode.isPending}
              onClick={handleRetry}
            >
              <RefreshCw className="h-3.5 w-3.5" />
              {t(($) => $.cloud_runtime.node.retry)}
            </Button>
          )}
          <Button
            type="button"
            variant="ghost"
            size="sm"
            className="h-7 w-7 shrink-0 p-0 text-muted-foreground hover:text-destructive"
            disabled={deleteNode.isPending}
            onClick={() => {
              if (!confirm(t(($) => $.cloud_runtime.delete_confirm))) return;
              deleteNode.mutate(node.instance_id || node.id, {
                onSuccess: () => toast.success(t(($) => $.cloud_runtime.toast_deleted)),
                onError: (err) =>
                  toast.error(
                    err instanceof Error
                      ? err.message
                      : t(($) => $.cloud_runtime.toast_delete_failed),
                  ),
              });
            }}
            aria-label={t(($) => $.cloud_runtime.delete)}
          >
            {deleteNode.isPending ? (
              <Loader2 className="h-3.5 w-3.5 animate-spin" />
            ) : (
              <Trash2 className="h-3.5 w-3.5" />
            )}
          </Button>
        </div>
      </div>

      {failure && (
        <div className="mt-2 rounded-md bg-destructive/5 px-2.5 py-2">
          <p className="text-caption font-medium text-destructive">
            {failure.message}
          </p>
          {failure.actionable && (
            <p className="mt-0.5 text-micro text-muted-foreground">
              {failure.actionable}
            </p>
          )}
        </div>
      )}
      {node.instance_id && (
        <div className="mt-2 truncate font-mono text-micro text-muted-foreground">
          {node.instance_id}
        </div>
      )}
    </div>
  );
}

function CloudRuntimeStatusBadge({ node }: { node: CloudRuntimeNode }) {
  const { t } = useT("runtimes");
  const normalized = node.status.toLowerCase();
  const active = new Set(["running", "success"]);
  const pending = new Set([
    "launching",
    "pending",
    "starting",
    "stopping",
    "rebooting",
    "terminating",
  ]);
  const failed = new Set(["failed", "cleanup_required", "error"]);
  const stageLabel =
    node.stage === "provisioning"
      ? t(($) => $.cloud_runtime.stage.provisioning)
      : node.stage === "connecting"
        ? t(($) => $.cloud_runtime.stage.connecting)
        : node.stage === "ready"
          ? t(($) => $.cloud_runtime.stage.ready)
          : "";
  return (
    <Badge
      variant="secondary"
      className={cn(
        "h-5 gap-1 rounded-md px-1.5 font-mono text-micro",
        active.has(normalized) && "bg-success/10 text-success",
        pending.has(normalized) && "bg-warning/10 text-warning",
        failed.has(normalized) && "bg-destructive/10 text-destructive",
      )}
    >
      {pending.has(normalized) && <Loader2 className="h-3 w-3 animate-spin" />}
      {stageLabel || node.status || "unknown"}
    </Badge>
  );
}
