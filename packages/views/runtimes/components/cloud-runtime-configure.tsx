"use client";

import { useState } from "react";
import type { FormEvent } from "react";
import { Loader2, Rocket } from "@multica/ui/icons";
import { toast } from "sonner";
import type {
  CloudRuntimeAccount,
  CreateCloudRuntimeNodeRequest,
} from "@multica/core/runtimes";
import { useCreateCloudRuntimeNode } from "@multica/core/runtimes";
import { useWorkspaceId } from "@multica/core/hooks";
import { Button } from "@multica/ui/components/ui/button";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@multica/ui/components/ui/select";
import { useT } from "../../i18n";

const AWS_INSTANCE_TYPES = ["t4g.medium", "t4g.large", "t3.micro"] as const;
const ALIYUN_INSTANCE_TYPES = [
  "ecs.t6-c1m1.large",
  "ecs.t6-c1m2.large",
] as const;
const DEFAULT_DISK_SIZE_GB = 20;

// CloudRuntimeConfigurePanel is the "确认并开通" step: region, network,
// instance type and disk, with an explicit resource and billing review before
// the first billable call is made.
export function CloudRuntimeConfigurePanel({
  provider,
  account,
  onBack,
  onOpened,
}: {
  provider: string;
  account: CloudRuntimeAccount;
  onBack: () => void;
  onOpened: () => void;
}) {
  const { t } = useT("runtimes");
  const wsId = useWorkspaceId();
  const createNode = useCreateCloudRuntimeNode(wsId);

  const instanceTypes =
    provider === "aliyun" ? ALIYUN_INSTANCE_TYPES : AWS_INSTANCE_TYPES;
  const [name, setName] = useState("");
  const [instanceType, setInstanceType] = useState(instanceTypes[0]);
  const [region, setRegion] = useState(account.region || "");
  const [subnetId, setSubnetId] = useState("");
  const [diskSizeGB, setDiskSizeGB] = useState(String(DEFAULT_DISK_SIZE_GB));

  const handleSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const trimmedName = name.trim();
    if (!trimmedName) {
      toast.error(t(($) => $.cloud_runtime.validation.name_required));
      return;
    }
    const disk = diskSizeGB.trim()
      ? Number(diskSizeGB.trim())
      : DEFAULT_DISK_SIZE_GB;
    if (!Number.isInteger(disk) || disk <= 0) {
      toast.error(t(($) => $.cloud_runtime.validation.disk_size_invalid));
      return;
    }
    const payload: CreateCloudRuntimeNodeRequest = {
      provider,
      account_id: account.id,
      name: trimmedName,
      instance_type: instanceType,
      disk_size_gb: disk,
    };
    if (region.trim()) payload.region = region.trim();
    if (subnetId.trim()) payload.subnet_id = subnetId.trim();
    try {
      await createNode.mutateAsync(payload);
      toast.success(t(($) => $.cloud_runtime.toast_created));
      onOpened();
    } catch (error) {
      toast.error(
        error instanceof Error
          ? error.message
          : t(($) => $.cloud_runtime.toast_create_failed),
      );
    }
  };

  return (
    <form onSubmit={handleSubmit} className="space-y-4">
      <div>
        <h3 className="text-body font-medium">
          {t(($) => $.cloud_runtime.configure.title)}
        </h3>
        <p className="mt-1 text-caption text-muted-foreground">
          <code className="font-mono text-micro">{account.role_arn}</code>
        </p>
      </div>

      <div className="grid gap-3 sm:grid-cols-2">
        <div className="space-y-1.5">
          <Label htmlFor="cloud-runtime-name" className="text-caption text-muted-foreground">
            {t(($) => $.cloud_runtime.configure.name)}
          </Label>
          <Input
            id="cloud-runtime-name"
            value={name}
            onChange={(event) => setName(event.target.value)}
            placeholder={t(($) => $.cloud_runtime.placeholders.name)}
            className="h-9 text-body"
            required
          />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="cloud-runtime-type" className="text-caption text-muted-foreground">
            {t(($) => $.cloud_runtime.configure.instance_type)}
          </Label>
          <Select
            items={instanceTypes.map((type) => ({ value: type, label: type }))}
            value={instanceType}
            onValueChange={(next) => next && setInstanceType(next)}
          >
            <SelectTrigger id="cloud-runtime-type" className="h-9 w-full text-body">
              <SelectValue>
                {() => <span className="truncate">{instanceType}</span>}
              </SelectValue>
            </SelectTrigger>
            <SelectContent align="start">
              {instanceTypes.map((type) => (
                <SelectItem key={type} value={type}>
                  {type}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="cloud-runtime-region" className="text-caption text-muted-foreground">
            {t(($) => $.cloud_runtime.configure.region)}
          </Label>
          <Input
            id="cloud-runtime-region"
            value={region}
            onChange={(event) => setRegion(event.target.value)}
            // Region identifiers are technical values, not translatable copy.
            // eslint-disable-next-line no-restricted-syntax -- placeholder is a cloud region ID
            placeholder={account.region || "us-east-1"}
            className="h-9 text-body"
          />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="cloud-runtime-subnet" className="text-caption text-muted-foreground">
            {t(($) => $.cloud_runtime.configure.subnet)}
          </Label>
          <Input
            id="cloud-runtime-subnet"
            value={subnetId}
            onChange={(event) => setSubnetId(event.target.value)}
            className="h-9 font-mono text-body"
          />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="cloud-runtime-disk" className="text-caption text-muted-foreground">
            {t(($) => $.cloud_runtime.configure.disk_size)}
          </Label>
          <Input
            id="cloud-runtime-disk"
            type="number"
            inputMode="numeric"
            min={1}
            value={diskSizeGB}
            onChange={(event) => setDiskSizeGB(event.target.value)}
            className="h-9 text-body"
          />
        </div>
      </div>

      <div className="rounded-md border bg-muted/30 px-3 py-2.5">
        <h4 className="text-caption font-medium">
          {t(($) => $.cloud_runtime.configure.review_title)}
        </h4>
        <ul className="mt-1.5 space-y-1 text-caption text-muted-foreground">
          <li>{instanceType}</li>
          <li>
            {t(($) => $.cloud_runtime.configure.disk_size)}: {diskSizeGB}
          </li>
          <li>{account.role_arn}</li>
        </ul>
        <p className="mt-2 text-caption text-warning">
          {t(($) => $.cloud_runtime.configure.billing_notice)}
        </p>
      </div>

      <div className="flex items-center justify-between gap-2 pt-1">
        <Button type="button" variant="outline" size="sm" onClick={onBack}>
          {t(($) => $.cloud_runtime.back)}
        </Button>
        <Button type="submit" size="sm" disabled={createNode.isPending}>
          {createNode.isPending ? (
            <Loader2 className="h-3.5 w-3.5 animate-spin" />
          ) : (
            <Rocket className="h-3.5 w-3.5" />
          )}
          {createNode.isPending
            ? t(($) => $.cloud_runtime.configure.submitting)
            : t(($) => $.cloud_runtime.configure.submit)}
        </Button>
      </div>
    </form>
  );
}
