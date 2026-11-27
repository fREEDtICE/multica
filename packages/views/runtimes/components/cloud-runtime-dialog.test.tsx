import type { ReactNode } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { I18nProvider } from "@multica/core/i18n/react";
import type { CloudRuntimeAccount, CloudRuntimeNode, CloudTrustBundle } from "@multica/core/runtimes";
import enCommon from "../../locales/en/common.json";
import enLayout from "../../locales/en/layout.json";
import enRuntimes from "../../locales/en/runtimes.json";
import { CloudRuntimeDialog } from "./cloud-runtime-dialog";

const TEST_RESOURCES = {
  en: { common: enCommon, layout: enLayout, runtimes: enRuntimes },
};
const WS = "ws-test";

const api = vi.hoisted(() => ({
  listCloudRuntimeNodes: vi.fn(),
  createCloudRuntimeNode: vi.fn(),
  deleteCloudRuntimeNode: vi.fn(),
  cancelCloudRuntimeNode: vi.fn(),
  listCloudAccounts: vi.fn(),
  getCloudTrustBundle: vi.fn(),
  verifyCloudAccount: vi.fn(),
  deleteCloudAccount: vi.fn(),
  listCloudRuntimeCleanup: vi.fn(),
  retryCloudRuntimeCleanup: vi.fn(),
  getDefaultCloudRuntime: vi.fn(),
  setDefaultCloudRuntime: vi.fn(),
}));

vi.mock("@multica/core/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/api")>()),
  api,
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => WS }));
vi.mock("@multica/ui/lib/clipboard", () => ({
  copyText: vi.fn().mockResolvedValue(true),
}));
vi.mock("motion/react", async (importOriginal) => ({
  ...(await importOriginal<typeof import("motion/react")>()),
  AnimatePresence: ({ children }: { children: ReactNode }) => children,
}));

const trust: CloudTrustBundle = {
  provider: "aws",
  identity_present: true,
  identity: { account: "123456789012" },
  trust_policy: '{"trust":true}',
  permissions_policy: '{"permissions":true}',
  console_guide_url: "https://example.com/guide",
  role_arn_example: "arn:aws:iam::123456789012:role/MulticaCloudRuntime",
};

const account: CloudRuntimeAccount = {
  id: "cca-1",
  provider: "aws",
  label: "production",
  role_arn: "arn:aws:iam::123456789012:role/MulticaCloudRuntime",
  region: "us-east-1",
  status: "verified",
  identity: { account: "123456789012" },
  created_at: "2026-11-26T00:00:00Z",
  updated_at: "2026-11-26T00:00:00Z",
};

const readyNode: CloudRuntimeNode = {
  id: "crn-1",
  owner_id: "user-1",
  provider: "aws",
  account_id: "cca-1",
  role_arn: account.role_arn,
  instance_id: "i-0123456789abcdef0",
  region: "us-east-1",
  instance_type: "t3.micro",
  image_id: "ami-helios",
  subnet_id: "subnet-helios",
  name: "route-b-node",
  status: "running",
  stage: "ready",
  tags: {},
  metadata: { instance_id: "i-0123456789abcdef0" },
  events: [],
  created_at: "2026-11-26T00:00:00Z",
  updated_at: "2026-11-26T00:00:00Z",
};

function renderDialog() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const onClose = vi.fn();
  const result = render(
    <QueryClientProvider client={qc}>
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <CloudRuntimeDialog onClose={onClose} />
      </I18nProvider>
    </QueryClientProvider>,
  );
  return { ...result, onClose, qc };
}

describe("CloudRuntimeDialog Route B wizard", () => {
  it("walks from a blank workspace through account connection to opening a runtime", async () => {
    api.listCloudRuntimeNodes.mockResolvedValue([]);
    api.listCloudAccounts.mockResolvedValue([]);
    api.listCloudRuntimeCleanup.mockResolvedValue([]);
    api.getDefaultCloudRuntime.mockResolvedValue("");
    api.getCloudTrustBundle.mockResolvedValue(trust);
    api.verifyCloudAccount.mockResolvedValue(account);
    api.createCloudRuntimeNode.mockResolvedValue(readyNode);
    const user = userEvent.setup();
    renderDialog();

    // Step 1: provider choice from a blank workspace.
    expect(await screen.findByText("Choose a cloud provider")).toBeInTheDocument();
    await user.click(screen.getAllByRole("button", { name: "Connect cloud account" })[0]!);

    // Step 2: trust information and role verification.
    expect(await screen.findByText("Connect cloud account")).toBeInTheDocument();
    await user.type(
      screen.getByLabelText("Role ARN"),
      "arn:aws:iam::123456789012:role/MulticaCloudRuntime",
    );
    await user.click(screen.getByRole("button", { name: "Verify authorization" }));
    await screen.findByText("Verified");
    await user.click(screen.getByRole("button", { name: "Continue" }));

    // Step 3: configure and open.
    expect(await screen.findByText("Confirm and open")).toBeInTheDocument();
    await user.clear(screen.getByLabelText("Runtime name"));
    await user.type(screen.getByLabelText("Runtime name"), "route-b-node");
    await user.click(screen.getByRole("button", { name: "Open runtime" }));

    await waitFor(() =>
      expect(api.createCloudRuntimeNode).toHaveBeenCalledWith(
        expect.objectContaining({
          provider: "aws",
          account_id: "cca-1",
          name: "route-b-node",
          disk_size_gb: 20,
        }),
      ),
    );
    // The wizard resets to the provider step and invalidates the node query.
    await waitFor(() => expect(screen.getByText("Choose a cloud provider")).toBeInTheDocument());
    expect(api.createCloudRuntimeNode).toHaveBeenCalledTimes(1);
  });

  it("shows a failed launch with its actionable hint and a retry", async () => {
    const failedNode: CloudRuntimeNode = {
      ...readyNode,
      id: "crn-2",
      status: "failed",
      stage: "provisioning",
      error: {
        Code: "provision_timeout",
        Message: "instance did not register in time",
        Actionable: "Check the network and retry; the same application is reused.",
      },
      events: [],
    };
    api.listCloudRuntimeNodes.mockResolvedValue([failedNode]);
    api.listCloudAccounts.mockResolvedValue([account]);
    api.listCloudRuntimeCleanup.mockResolvedValue([]);
    api.getDefaultCloudRuntime.mockResolvedValue("");
    api.createCloudRuntimeNode.mockResolvedValue(readyNode);
    const user = userEvent.setup();
    renderDialog();

    const row = await screen.findByText("route-b-node");
    const container = row.closest("div.rounded-md") ?? document.body;
    expect(
      within(container as HTMLElement).getByText("instance did not register in time"),
    ).toBeInTheDocument();
    await user.click(within(container as HTMLElement).getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(api.createCloudRuntimeNode).toHaveBeenCalledWith(
        expect.objectContaining({ account_id: "cca-1", name: "route-b-node" }),
      ),
    );
  });

  it("lists resources awaiting cleanup and retries their reclamation", async () => {
    const stranded: CloudRuntimeNode = {
      ...readyNode,
      id: "crn-3",
      name: "stranded",
      status: "cleanup_required",
      error: { Code: "cleanup_required", Message: "reclamation failed" },
      events: [],
    };
    api.listCloudRuntimeNodes.mockResolvedValue([stranded]);
    api.listCloudAccounts.mockResolvedValue([]);
    api.listCloudRuntimeCleanup.mockResolvedValue([stranded]);
    api.getDefaultCloudRuntime.mockResolvedValue("");
    api.retryCloudRuntimeCleanup.mockResolvedValue({ ...stranded, status: "terminated" });
    const user = userEvent.setup();
    renderDialog();

    expect(await screen.findByText("Resources awaiting cleanup (1)")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Retry cleanup" }));
    await waitFor(() =>
      expect(api.retryCloudRuntimeCleanup).toHaveBeenCalledWith("crn-3"),
    );
  });
});
