"use client";

import { useEffect, useState } from "react";
import type { FormEvent } from "react";
import { useQuery } from "@tanstack/react-query";
import { CheckCircle2, ExternalLink, Loader2, TriangleAlert } from "@multica/ui/icons";
import { toast } from "sonner";
import { ApiError } from "@multica/core/api";
import type {
  CloudRuntimeAccount,
  CloudRuntimeDriverError,
  CloudTrustBundle,
} from "@multica/core/runtimes";
import {
  cloudTrustBundleOptions,
  useVerifyCloudAccount,
} from "@multica/core/runtimes";
import { useWorkspaceId } from "@multica/core/hooks";
import { Alert, AlertDescription, AlertTitle } from "@multica/ui/components/ui/alert";
import { Button } from "@multica/ui/components/ui/button";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import { Textarea } from "@multica/ui/components/ui/textarea";
import { CopyCheckIcon } from "@multica/ui/components/common/copy-check-icon";
import { copyText } from "@multica/ui/lib/clipboard";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";

// ConnectCloudAccountPanel implements the "连接云账号" step: it renders the
// copyable trust policy and minimum-privilege template, collects the role ARN,
// and verifies it through short-lived STS credentials. A deployment without a
// cloud identity disables the whole panel instead of pretending to work.
export function ConnectCloudAccountPanel({
  provider,
  onVerified,
  onBack,
}: {
  provider: string;
  onVerified: (account: CloudRuntimeAccount) => void;
  onBack: () => void;
}) {
  const { t } = useT("runtimes");
  const wsId = useWorkspaceId();
  const trustQuery = useQuery(cloudTrustBundleOptions(wsId, provider));
  const verify = useVerifyCloudAccount(wsId);

  const [roleArn, setRoleArn] = useState("");
  const [label, setLabel] = useState("");
  const [verifiedAccount, setVerifiedAccount] = useState<CloudRuntimeAccount | null>(null);
  const [verifyError, setVerifyError] = useState<CloudRuntimeDriverError | null>(null);

  const bundle = trustQuery.data as CloudTrustBundle | undefined;
  const identityMissing = bundle ? !bundle.identity_present : false;

  const handleVerify = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const arn = roleArn.trim();
    if (!arn) {
      toast.error(t(($) => $.cloud_runtime.validation.role_arn_required));
      return;
    }
    try {
      const account = await verify.mutateAsync({
        provider,
        role_arn: arn,
        label: label.trim() || undefined,
      });
      setVerifiedAccount(account);
      setVerifyError(null);
      toast.success(t(($) => $.cloud_runtime.connect.verified));
    } catch (error) {
      // A 4xx carries the failed account record with its structured driver error.
      const body = error instanceof ApiError ? (error.body as { last_error?: CloudRuntimeDriverError } | undefined) : undefined;
      const driverError = body?.last_error;
      setVerifyError(
        driverError ?? {
          Message: error instanceof Error ? error.message : t(($) => $.cloud_runtime.connect.failed),
        },
      );
      setVerifiedAccount(null);
      toast.error(t(($) => $.cloud_runtime.connect.failed));
    }
  };

  const failedAccount = verifyError ? { last_error: verifyError } : null;

  return (
    <div className="space-y-4">
      <div>
        <h3 className="text-body font-medium">
          {t(($) => $.cloud_runtime.connect.title)}
        </h3>
        <p className="mt-1 text-caption text-muted-foreground">
          {t(($) => $.cloud_runtime.connect.subtitle)}
        </p>
      </div>

      {identityMissing && (
        <Alert variant="destructive">
          <TriangleAlert className="h-4 w-4" />
          <AlertTitle>
            {t(($) => $.cloud_runtime.connect.identity_missing_title)}
          </AlertTitle>
          <AlertDescription>
            {t(($) => $.cloud_runtime.connect.identity_missing_hint)}
          </AlertDescription>
        </Alert>
      )}

      {bundle?.identity_present && (
        <div className="flex items-center gap-2 rounded-md border bg-success/5 px-3 py-2 text-caption text-success">
          <CheckCircle2 className="h-4 w-4 shrink-0" />
          <span>{t(($) => $.cloud_runtime.connect.identity_present)}</span>
          {bundle.identity?.account && (
            <code className="ml-auto truncate font-mono text-micro">
              {bundle.identity.account}
            </code>
          )}
        </div>
      )}

      {trustQuery.isLoading ? (
        <div className="flex h-32 items-center justify-center">
          <Loader2 className="h-5 w-5 animate-spin text-muted-foreground" />
        </div>
      ) : bundle ? (
        <div className="space-y-3">
          <PolicyBlock
            label={t(($) => $.cloud_runtime.connect.trust_policy)}
            value={bundle.trust_policy}
            copyLabel={t(($) => $.cloud_runtime.connect.copy)}
            copiedLabel={t(($) => $.cloud_runtime.connect.copied)}
          />
          <PolicyBlock
            label={t(($) => $.cloud_runtime.connect.permissions_policy)}
            value={bundle.permissions_policy}
            copyLabel={t(($) => $.cloud_runtime.connect.copy)}
            copiedLabel={t(($) => $.cloud_runtime.connect.copied)}
          />
          <a
            href={bundle.console_guide_url}
            target="_blank"
            rel="noreferrer"
            className="inline-flex items-center gap-1.5 text-caption text-primary hover:underline"
          >
            <ExternalLink className="h-3.5 w-3.5" />
            {t(($) => $.cloud_runtime.connect.console_guide)}
          </a>
        </div>
      ) : null}

      <form onSubmit={handleVerify} className="space-y-3">
        <div className="space-y-1.5">
          <Label htmlFor="cloud-role-arn" className="text-caption text-muted-foreground">
            {t(($) => $.cloud_runtime.connect.role_arn)}
          </Label>
          <Input
            id="cloud-role-arn"
            value={roleArn}
            onChange={(event) => setRoleArn(event.target.value)}
            placeholder={t(($) => $.cloud_runtime.connect.role_arn_placeholder)}
            className="h-9 font-mono text-body"
            disabled={identityMissing}
          />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="cloud-account-label" className="text-caption text-muted-foreground">
            {t(($) => $.cloud_runtime.connect.account_label)}
          </Label>
          <Input
            id="cloud-account-label"
            value={label}
            onChange={(event) => setLabel(event.target.value)}
            className="h-9 text-body"
            disabled={identityMissing}
          />
        </div>

        {failedAccount?.last_error && (
          <Alert variant="destructive">
            <AlertTitle>{t(($) => $.cloud_runtime.connect.failed)}</AlertTitle>
            <AlertDescription className="space-y-1">
              <p className="font-mono text-micro">
                {failedAccount.last_error.Message ||
                  failedAccount.last_error.message}
              </p>
              {(failedAccount.last_error.Actionable ||
                failedAccount.last_error.actionable) && (
                <p>
                  {failedAccount.last_error.Actionable ||
                    failedAccount.last_error.actionable}
                </p>
              )}
            </AlertDescription>
          </Alert>
        )}

        {verifiedAccount && (
          <Alert>
            <CheckCircle2 className="h-4 w-4 text-success" />
            <AlertTitle>{t(($) => $.cloud_runtime.connect.verified)}</AlertTitle>
            <AlertDescription>
              <span className="font-mono text-micro">
                {t(($) => $.cloud_runtime.connect.verified_identity)}:{" "}
                {verifiedAccount.identity?.account ?? verifiedAccount.role_arn}
              </span>
            </AlertDescription>
          </Alert>
        )}

        <div className="flex items-center justify-between gap-2 pt-1">
          <Button type="button" variant="outline" size="sm" onClick={onBack}>
            {t(($) => $.cloud_runtime.back)}
          </Button>
          {verifiedAccount ? (
            <Button
              type="button"
              size="sm"
              onClick={() => onVerified(verifiedAccount)}
            >
              {t(($) => $.cloud_runtime.connect.continue)}
            </Button>
          ) : (
            <Button
              type="submit"
              size="sm"
              disabled={identityMissing || verify.isPending}
            >
              {verify.isPending ? (
                <Loader2 className="h-3.5 w-3.5 animate-spin" />
              ) : (
                <CheckCircle2 className="h-3.5 w-3.5" />
              )}
              {verify.isPending
                ? t(($) => $.cloud_runtime.connect.verifying)
                : failedAccount
                  ? t(($) => $.cloud_runtime.connect.retry)
                  : t(($) => $.cloud_runtime.connect.verify)}
            </Button>
          )}
        </div>
      </form>
    </div>
  );
}

function PolicyBlock({
  label,
  value,
  copyLabel,
  copiedLabel,
}: {
  label: string;
  value: string;
  copyLabel: string;
  copiedLabel: string;
}) {
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    if (!copied) return;
    const timer = setTimeout(() => setCopied(false), 2000);
    return () => clearTimeout(timer);
  }, [copied]);

  return (
    <div className="space-y-1.5">
      <div className="flex items-center justify-between">
        <Label className="text-caption text-muted-foreground">{label}</Label>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          className="h-7 gap-1.5 px-2"
          onClick={() => {
            void copyText(value).then((ok) => {
              if (ok) setCopied(true);
            });
          }}
          aria-label={copyLabel}
        >
          <CopyCheckIcon copied={copied} />
          <span className={cn("text-caption", copied && "text-success")}>
            {copied ? copiedLabel : copyLabel}
          </span>
        </Button>
      </div>
      <Textarea
        readOnly
        value={value}
        rows={7}
        className="resize-none bg-muted font-mono text-micro"
      />
    </div>
  );
}
