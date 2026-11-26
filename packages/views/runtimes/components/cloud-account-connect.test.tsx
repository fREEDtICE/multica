import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { I18nProvider } from "@multica/core/i18n/react";
import { ApiError } from "@multica/core/api";
import type { CloudRuntimeAccount, CloudTrustBundle } from "@multica/core/runtimes";
import enCommon from "../../locales/en/common.json";
import enRuntimes from "../../locales/en/runtimes.json";
import { ConnectCloudAccountPanel } from "./cloud-account-connect";

const TEST_RESOURCES = { en: { common: enCommon, runtimes: enRuntimes } };
const WS = "ws-test";

const api = vi.hoisted(() => ({
  getCloudTrustBundle: vi.fn(),
  verifyCloudAccount: vi.fn(),
}));

vi.mock("@multica/core/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/api")>()),
  api,
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => WS }));

const clipboard = vi.hoisted(() => ({ copyText: vi.fn() }));
vi.mock("@multica/ui/lib/clipboard", () => ({ copyText: clipboard.copyText }));

function trustBundle(overrides: Partial<CloudTrustBundle> = {}): CloudTrustBundle {
  return {
    provider: "aws",
    identity_present: true,
    identity: { account: "123456789012", arn: "arn:aws:iam::123456789012:user/helios" },
    trust_policy: '{"trust":"policy"}',
    permissions_policy: '{"permissions":"policy"}',
    console_guide_url: "https://example.com/guide",
    role_arn_example: "arn:aws:iam::123456789012:role/MulticaCloudRuntime",
    ...overrides,
  };
}

function verifiedAccount(overrides: Partial<CloudRuntimeAccount> = {}): CloudRuntimeAccount {
  return {
    id: "cca-1",
    provider: "aws",
    label: "test",
    role_arn: "arn:aws:iam::123456789012:role/MulticaCloudRuntime",
    region: "us-east-1",
    status: "verified",
    identity: { account: "123456789012" },
    created_at: "2026-11-26T00:00:00Z",
    updated_at: "2026-11-26T00:00:00Z",
    ...overrides,
  };
}

function renderPanel() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const onVerified = vi.fn();
  const onBack = vi.fn();
  const result = render(
    <QueryClientProvider client={qc}>
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <ConnectCloudAccountPanel
          provider="aws"
          onVerified={onVerified}
          onBack={onBack}
        />
      </I18nProvider>
    </QueryClientProvider>,
  );
  return { ...result, onVerified, onBack };
}

describe("ConnectCloudAccountPanel", () => {
  it("renders copyable trust information and verifies a role ARN", async () => {
    api.getCloudTrustBundle.mockResolvedValue(trustBundle());
    api.verifyCloudAccount.mockResolvedValue(verifiedAccount());
    clipboard.copyText.mockResolvedValue(true);
    const user = userEvent.setup();
    const { onVerified } = renderPanel();

    await waitFor(() =>
      expect(screen.getByText("Deployment identity verified")).toBeInTheDocument(),
    );
    expect(screen.getAllByRole("button", { name: "Copy" })).toHaveLength(2);
    await user.click(screen.getAllByRole("button", { name: "Copy" })[0]!);
    expect(clipboard.copyText).toHaveBeenCalledWith('{"trust":"policy"}');

    const arn = "arn:aws:iam::123456789012:role/MulticaCloudRuntime";
    await user.type(screen.getByLabelText("Role ARN"), arn);
    await user.click(screen.getByRole("button", { name: "Verify authorization" }));

    await waitFor(() => expect(api.verifyCloudAccount).toHaveBeenCalledWith({
      provider: "aws",
      role_arn: arn,
      label: undefined,
    }));
    await screen.findByText("Verified");
    await user.click(screen.getByRole("button", { name: "Continue" }));
    expect(onVerified).toHaveBeenCalledWith(verifiedAccount());
  });

  it("disables verification when the deployment identity is missing", async () => {
    api.getCloudTrustBundle.mockResolvedValue(
      trustBundle({ identity_present: false, identity: undefined }),
    );
    renderPanel();

    await waitFor(() =>
      expect(
        screen.getByText("Deployment cloud identity unavailable"),
      ).toBeInTheDocument(),
    );
    expect(screen.getByRole("button", { name: "Verify authorization" })).toBeDisabled();
    expect(screen.getByLabelText("Role ARN")).toBeDisabled();
  });

  it("shows the structured driver error and allows re-verification", async () => {
    api.getCloudTrustBundle.mockResolvedValue(trustBundle());
    api.verifyCloudAccount
      .mockRejectedValueOnce(
        new ApiError("forbidden", 403, "Forbidden", {
          id: "cca-2",
          provider: "aws",
          role_arn: "arn:aws:iam::123456789012:role/MulticaCloudRuntime",
          status: "failed",
          last_error: {
            Code: "authorization_failed",
            Message: "AWS AccessDenied: trust policy rejected",
            Actionable: "Fix the trust policy and verify again.",
          },
        }),
      )
      .mockResolvedValueOnce(verifiedAccount());
    const user = userEvent.setup();
    renderPanel();

    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Verify authorization" })).toBeEnabled(),
    );
    await user.type(
      screen.getByLabelText("Role ARN"),
      "arn:aws:iam::123456789012:role/MulticaCloudRuntime",
    );
    await user.click(screen.getByRole("button", { name: "Verify authorization" }));

    await screen.findByText("AWS AccessDenied: trust policy rejected");
    expect(screen.getByText("Fix the trust policy and verify again.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Verify again" })).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Verify again" }));
    await screen.findByText("Verified");
  });
});
