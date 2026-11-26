package provision

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/cloudruntime"
)

const leaseTTL = 600 * time.Second

// roleCapable is the vendor adapter contract Route B needs beyond the basic
// CloudProvider: report the deployment identity and assume the administrator-
// authorized role for short-lived credentials.
type roleCapable interface {
	CloudProvider
	IdentityReady() bool
	Identity(ctx context.Context) (map[string]string, error)
	AssumeRole(ctx context.Context, roleARN, sessionName string, durationSecs int) (CloudProvider, map[string]string, error)
}

// LocalProvisioner satisfies the handler's cloudRuntimeProxy interface while
// opening instances directly through the vendor APIs instead of proxying to
// the managed Multica Cloud Fleet. It is wired only when explicitly enabled,
// so deployments using MULTICA_CLOUD_URL keep the original proxy behavior.
type LocalProvisioner struct {
	cfg      Config
	store    *Store
	accounts *AccountStore
}

// New builds a LocalProvisioner from its configuration.
func New(cfg Config) (*LocalProvisioner, error) {
	store, err := NewStore(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	accountPath := cfg.AccountPath
	if accountPath == "" && cfg.StatePath != "" {
		accountPath = filepath.Join(filepath.Dir(cfg.StatePath), "cloud-accounts.json")
	}
	accounts, err := NewAccountStore(accountPath)
	if err != nil {
		return nil, err
	}
	if cfg.ReadinessTimeout <= 0 {
		cfg.ReadinessTimeout = 60 * time.Second
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 200 * time.Millisecond
	}
	return &LocalProvisioner{cfg: cfg, store: store, accounts: accounts}, nil
}

// Enabled reports whether the built-in driver is switched on.
func (p *LocalProvisioner) Enabled() bool { return p.cfg.Enabled }

// createNodeRequest is the subset of the node contract the driver consumes.
type createNodeRequest struct {
	Provider        string            `json:"provider"`
	AccountID       string            `json:"account_id"`
	Name            string            `json:"name"`
	InstanceType    string            `json:"instance_type"`
	Region          string            `json:"region"`
	ImageID         string            `json:"image_id"`
	SubnetID        string            `json:"subnet_id"`
	InstanceProfile string            `json:"iam_instance_profile"`
	DiskSizeGB      int               `json:"disk_size_gb"`
	Tags            map[string]string `json:"tags"`
}

// Do implements cloudRuntimeProxy by mapping the Fleet node REST contract to
// local vendor calls.
func (p *LocalProvisioner) Do(ctx context.Context, req cloudruntime.Request) (*cloudruntime.Response, error) {
	switch {
	case req.Path == "/healthz" || req.Path == "/readyz":
		return p.json(http.StatusOK, map[string]string{"status": "ok"})
	case req.Path == "/api/v1/":
		return p.json(http.StatusOK, map[string]any{"mode": "built-in", "providers": p.providerNames()})

	// Connected cloud accounts (Route B): trust information, role-based
	// verification and the workspace default runtime.
	case req.Path == "/api/v1/accounts" && req.Method == http.MethodGet:
		return p.json(http.StatusOK, map[string]any{"accounts": p.accounts.ListAccounts()})
	case req.Path == "/api/v1/accounts/trust" && req.Method == http.MethodGet:
		return p.trustBundle(ctx, req.Query)
	case req.Path == "/api/v1/accounts/verify" && req.Method == http.MethodPost:
		return p.verifyAccount(ctx, req.Body)
	case req.Path == "/api/v1/accounts" && req.Method == http.MethodDelete:
		return p.deleteAccount(req.Body)
	case req.Path == "/api/v1/default-runtime" && req.Method == http.MethodGet:
		return p.json(http.StatusOK, map[string]string{"node_id": p.accounts.DefaultNode()})
	case req.Path == "/api/v1/default-runtime" && req.Method == http.MethodPut:
		return p.setDefaultRuntime(req.Body)

	// Node lifecycle.
	case req.Path == "/api/v1/nodes" && req.Method == http.MethodGet:
		return p.json(http.StatusOK, map[string]any{"nodes": p.store.List()})
	case req.Path == "/api/v1/nodes" && req.Method == http.MethodPost:
		var body createNodeRequest
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return p.errorJSON(http.StatusBadRequest, ErrConfiguration, "invalid node request body")
		}
		return p.createNode(ctx, req.UserID, body)
	case req.Path == "/api/v1/nodes" && req.Method == http.MethodDelete:
		return p.deleteNode(ctx, req.Body)
	case req.Path == "/api/v1/nodes/cancel" && req.Method == http.MethodPost:
		return p.cancelNode(ctx, req.Body)
	case req.Path == "/api/v1/nodes/status" && req.Method == http.MethodPost:
		return p.nodeStatus(ctx, req.Body)
	case req.Path == "/api/v1/nodes/stop" && req.Method == http.MethodPost:
		return p.stopNode(ctx, req.Body)
	case req.Path == "/api/v1/nodes/start" || req.Path == "/api/v1/nodes/reboot":
		return p.errorJSON(http.StatusNotImplemented, ErrUnavailable, "built-in driver does not support power cycling")

	// Reclamation of resources whose authorization has lapsed.
	case req.Path == "/api/v1/cleanup" && req.Method == http.MethodGet:
		return p.json(http.StatusOK, map[string]any{"nodes": p.cleanupList()})
	case req.Path == "/api/v1/cleanup/retry" && req.Method == http.MethodPost:
		return p.retryCleanup(ctx, req.Body)
	default:
		return p.errorJSON(http.StatusNotFound, ErrConfiguration, "unknown cloud runtime path: "+req.Path)
	}
}

func (p *LocalProvisioner) providerNames() []string {
	names := make([]string, 0, len(p.cfg.Providers))
	for name := range p.cfg.Providers {
		names = append(names, name)
	}
	return names
}

func (p *LocalProvisioner) roleProvider(name string) (roleCapable, *cloudruntime.Response) {
	provider, ok := p.cfg.Providers[name]
	if !ok {
		resp, _ := p.errorJSON(http.StatusBadRequest, ErrConfiguration, "provider not configured: "+name)
		return nil, resp
	}
	role, ok := provider.(roleCapable)
	if !ok {
		resp, _ := p.errorJSON(http.StatusBadRequest, ErrConfiguration, "provider lacks role support: "+name)
		return nil, resp
	}
	return role, nil
}

// resolveAccount enforces the Route B gate: a node can only open under a
// verified connected account, and ambiguous requests name the account.
func (p *LocalProvisioner) resolveAccount(providerName string, accountID string) (*Account, *cloudruntime.Response) {
	if accountID != "" {
		account, ok := p.accounts.Get(accountID)
		if !ok || account.Provider != providerName {
			resp, _ := p.errorJSON(http.StatusBadRequest, ErrConfiguration, "unknown cloud account: "+accountID)
			return nil, resp
		}
		if account.Status != AccountVerified {
			resp, _ := p.errorJSON(http.StatusForbidden, ErrNoAccount, "the cloud account is not verified: "+accountID)
			return nil, resp
		}
		return account, nil
	}
	verified := p.accounts.VerifiedAccounts(providerName)
	switch len(verified) {
	case 0:
		resp, _ := p.errorJSON(http.StatusForbidden, ErrNoAccount,
			"connect and verify a cloud account before opening a runtime")
		return nil, resp
	case 1:
		return verified[0], nil
	default:
		resp, _ := p.errorJSON(http.StatusBadRequest, ErrConfiguration,
			"multiple verified accounts exist; pass account_id to select one")
		return nil, resp
	}
}

func (p *LocalProvisioner) createNode(ctx context.Context, ownerID string, body createNodeRequest) (*cloudruntime.Response, error) {
	providerName := body.Provider
	if providerName == "" {
		providerName = ProviderAWS
	}
	base, errResp := p.roleProvider(providerName)
	if errResp != nil {
		return errResp, nil
	}
	if !base.IdentityReady() {
		return p.errorJSON(http.StatusConflict, ErrIdentityMissing,
			"the self-hosted deployment has no cloud-recognised identity; ask the deployment administrator to configure it")
	}
	defaults, ok := p.cfg.Defaults[providerName]
	if !ok {
		return p.errorJSON(http.StatusBadRequest, ErrConfiguration, "missing account defaults for provider: "+providerName)
	}
	account, errResp := p.resolveAccount(providerName, body.AccountID)
	if errResp != nil {
		return errResp, nil
	}
	runtimeName := strings.TrimSpace(body.Name)
	if runtimeName == "" {
		runtimeName = "node-" + randHex(4)
	}
	instanceType := firstNonEmpty(body.InstanceType, defaults.InstanceType)
	imageID := firstNonEmpty(body.ImageID, defaults.ImageID)
	subnetID := firstNonEmpty(body.SubnetID, defaults.SubnetID)
	profile := firstNonEmpty(body.InstanceProfile, defaults.InstanceProfile)
	region := firstNonEmpty(body.Region, defaults.Region)

	// Every node operation runs on short-lived role credentials; the deployment
	// identity is only used to assume the administrator-authorized role.
	assumed, _, err := base.AssumeRole(ctx, account.RoleARN, "helios-"+providerName+"-"+randHex(4), MaxLeaseSeconds)
	if err != nil {
		return p.driverError(err)
	}

	clientToken := sha256Hex(ownerID + "|" + providerName + "|" + runtimeName)[:32]
	if existing, ok := p.store.LookupByClientToken(clientToken); ok && existing.Status != StatusFailed {
		// A successful or in-flight node with the same identity is returned as-is.
		return p.json(http.StatusOK, existing)
	}

	var node *Node
	if existing, ok := p.store.LookupByClientToken(clientToken); ok {
		// Retry a previously failed node: reuse its record and cloud token so
		// the vendor returns the same instance instead of opening another.
		node = existing
		p.store.Update(node.ID, func(n *Node) {
			n.Status = StatusLaunching
			n.Stage = StageProvisioning
			n.AccountID = account.ID
			n.RoleARN = account.RoleARN
			n.Error = nil
			n.Events = append(n.Events, NodeEvent{Event: "retry_requested", At: nowISO()})
		})
	} else {
		node = p.store.Create(&Node{
			ID: "crn-" + randHex(12), OwnerID: ownerID, Provider: providerName, Region: region,
			AccountID: account.ID, RoleARN: account.RoleARN,
			InstanceType: instanceType, ImageID: imageID, SubnetID: subnetID,
			DiskSizeGB: body.DiskSizeGB, Name: runtimeName,
			Status: StatusLaunching, Stage: StageProvisioning, ClientToken: clientToken, Tags: body.Tags,
			Metadata: map[string]any{}, Events: []NodeEvent{{Event: "create_requested", At: nowISO()}},
		})
	}

	ready, err := p.provision(ctx, assumed, node, Spec{
		Provider: providerName, Name: runtimeName, Region: region, ImageID: imageID,
		InstanceType: instanceType, SubnetID: subnetID, SecurityGroupIDs: defaults.SecurityGroupIDs,
		InstanceProfile: profile, DiskSizeGB: body.DiskSizeGB,
		ClientToken: clientToken, WorkspaceID: ownerID, RuntimeName: runtimeName,
	})
	if err != nil {
		driverErr, _ := AsDriverError(err)
		p.store.Update(node.ID, func(n *Node) {
			n.Status = StatusFailed
			n.Error = driverErr
			n.Events = append(n.Events, NodeEvent{Event: "failed", At: nowISO(), Note: string(driverErr.Code)})
		})
		return p.errorJSON(statusForError(driverErr), driverErr.Code, driverErr.Message)
	}
	return p.json(http.StatusCreated, ready)
}

func (p *LocalProvisioner) provision(ctx context.Context, provider CloudProvider, node *Node, spec Spec) (*Node, error) {
	if _, err := provider.Authorize(ctx); err != nil {
		return nil, err
	}
	if err := provider.EnsureNetwork(ctx, spec.SubnetID, spec.SecurityGroupIDs); err != nil {
		return nil, err
	}

	lease := randHex(32)
	expires := time.Now().Add(leaseTTL).Unix()
	userData, err := RenderUserData(BootstrapInput{
		WorkspaceURL: p.cfg.WorkspaceURL, Provider: spec.Provider, Region: spec.Region,
		RuntimeName: spec.RuntimeName, DaemonID: "helios-" + spec.Provider + "-" + spec.RuntimeName,
		InstanceProfile: spec.InstanceProfile, GatewayURL: firstNonEmpty(p.cfg.GatewayURL, p.cfg.WorkspaceURL),
		Lease: lease, LeaseExpiresAt: expires,
	})
	if err != nil {
		return nil, err
	}
	spec.UserData = userData
	p.store.Update(node.ID, func(n *Node) {
		n.UserDataHash = sha256Hex(userData)
		n.LeaseHash = sha256Hex(lease)
		n.Events = append(n.Events, NodeEvent{Event: "lease_issued", At: nowISO()})
	})

	instance, err := provider.RunInstance(ctx, spec)
	if err != nil {
		return nil, err
	}
	p.store.Update(node.ID, func(n *Node) {
		n.InstanceID = instance.ID
		n.Status = StatusPending
		n.Events = append(n.Events, NodeEvent{Event: "instance_launched", At: nowISO(), Note: instance.ID})
	})

	running, err := p.waitRunning(ctx, provider, instance.ID)
	if err != nil {
		return nil, err
	}
	// The machine exists; the remaining work is first-boot registration, so
	// the administrator sees 开通中 → 接入中 rather than a generic spinner.
	p.store.Update(node.ID, func(n *Node) {
		n.Stage = StageConnecting
		n.Events = append(n.Events, NodeEvent{Event: "connecting", At: nowISO()})
	})

	// The exact user-data stored by the cloud must match what we rendered,
	// proving the instance can only receive the short-lived lease we issued.
	storedUserData, err := provider.GetUserData(ctx, instance.ID)
	if err != nil {
		return nil, err
	}
	if sha256Hex(storedUserData) != node.UserDataHash {
		return nil, &DriverError{Code: ErrUnavailable, Message: "cloud returned different user-data"}
	}
	leaseDoc, err := ParseLease(storedUserData)
	if err != nil || leaseDoc.ExpiresAt != expires || leaseTTL > MaxLeaseSeconds*time.Second {
		return nil, &DriverError{Code: ErrConfiguration, Message: "embedded lease is not the issued short-lived lease"}
	}

	metadata := map[string]any{
		"provider": running.Provider, "region": running.Region, "instance_id": running.ID,
		"image_id": running.ImageID, "private_ip": running.PrivateIP, "instance_profile": running.Profile,
	}
	if provider.Name() == ProviderAWS {
		console, err := provider.ConsoleOutput(ctx, instance.ID)
		if err != nil {
			return nil, err
		}
		metadata["console_output"] = console
	}
	updated, _ := p.store.Update(node.ID, func(n *Node) {
		n.Status = StatusRunning
		n.Stage = StageReady
		n.Metadata = metadata
		n.Events = append(n.Events, NodeEvent{Event: "runtime_ready", At: nowISO(), Note: running.ID})
	})
	return updated, nil
}

func (p *LocalProvisioner) waitRunning(ctx context.Context, provider CloudProvider, instanceID string) (*Instance, error) {
	deadline := time.Now().Add(p.cfg.ReadinessTimeout)
	for time.Now().Before(deadline) {
		instance, err := provider.DescribeInstance(ctx, instanceID)
		if err != nil {
			return nil, err
		}
		if instance.State == StatusRunning {
			return instance, nil
		}
		if instance.State == StatusTerminated {
			return nil, &DriverError{Code: ErrTimeout, Message: "instance terminated during provisioning",
				Actionable: "Check account quota and instance type, then retry; the idempotency token prevents a duplicate."}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(p.cfg.PollInterval):
		}
	}
	return nil, &DriverError{Code: ErrTimeout, Message: "instance did not become running before the readiness timeout",
		Actionable: "Retry to resume the same instance; investigate region/quota issues if it persists."}
}

type nodeRef struct {
	ID         string `json:"id"`
	InstanceID string `json:"instance_id"`
}

func (p *LocalProvisioner) lookupNode(body []byte) (*Node, error) {
	var ref nodeRef
	if err := json.Unmarshal(body, &ref); err != nil {
		return nil, &DriverError{Code: ErrConfiguration, Message: "invalid node reference"}
	}
	node, ok := p.store.Get(firstNonEmpty(ref.ID, ref.InstanceID))
	if !ok {
		for _, candidate := range p.store.List() {
			if candidate.InstanceID == ref.InstanceID {
				node = candidate
				ok = true
				break
			}
		}
	}
	if !ok {
		return nil, &DriverError{Code: ErrConfiguration, Message: "unknown node"}
	}
	return node, nil
}

func (p *LocalProvisioner) nodeFor(body []byte) (*Node, CloudProvider, error) {
	node, err := p.lookupNode(body)
	if err != nil {
		return nil, nil, err
	}
	provider, err := p.providerForNode(context.Background(), node)
	if err != nil {
		return nil, nil, err
	}
	return node, provider, nil
}

// providerForNode returns the short-lived role provider bound to the node's
// connected account. Nodes created before Route B carry no account and fall
// back to the deployment identity provider.
func (p *LocalProvisioner) providerForNode(ctx context.Context, node *Node) (CloudProvider, error) {
	base, ok := p.cfg.Providers[node.Provider].(roleCapable)
	if !ok {
		return nil, &DriverError{Code: ErrConfiguration, Message: "provider no longer configured: " + node.Provider}
	}
	if node.RoleARN == "" {
		return p.cfg.Providers[node.Provider], nil
	}
	assumed, _, err := base.AssumeRole(ctx, node.RoleARN, "helios-"+node.Provider+"-"+randHex(4), MaxLeaseSeconds)
	if err != nil {
		return nil, err
	}
	return assumed, nil
}

func (p *LocalProvisioner) nodeStatus(ctx context.Context, body []byte) (*cloudruntime.Response, error) {
	node, provider, err := p.nodeFor(body)
	if err != nil {
		return p.driverError(err)
	}
	instance, err := provider.DescribeInstance(ctx, node.InstanceID)
	if err != nil {
		return p.driverError(err)
	}
	updated, _ := p.store.Update(node.ID, func(n *Node) {
		n.Status = instance.State
	})
	return p.json(http.StatusOK, updated)
}

// reclamationProvider resolves the role provider for a reclamation call. An
// authorization lapse (failed AssumeRole) is converted into a cleanup_required
// record so the resource stays visible instead of returning a generic 502 and
// orphaning a billable instance.
func (p *LocalProvisioner) reclamationProvider(body []byte) (*Node, CloudProvider, *cloudruntime.Response) {
	node, err := p.lookupNode(body)
	if err != nil {
		resp, _ := p.driverError(err)
		return nil, nil, resp
	}
	provider, err := p.providerForNode(context.Background(), node)
	if err != nil {
		resp, _ := p.markCleanupRequired(node, err)
		return nil, nil, resp
	}
	return node, provider, nil
}

func (p *LocalProvisioner) stopNode(ctx context.Context, body []byte) (*cloudruntime.Response, error) {
	node, provider, errResp := p.reclamationProvider(body)
	if errResp != nil {
		return errResp, nil
	}
	if err := provider.TerminateInstance(ctx, node.InstanceID); err != nil {
		return p.markCleanupRequired(node, err)
	}
	updated, _ := p.store.Update(node.ID, func(n *Node) {
		n.Status = StatusTerminated
		n.Events = append(n.Events, NodeEvent{Event: "runtime_terminated", At: nowISO(), Note: n.InstanceID})
	})
	return p.json(http.StatusOK, updated)
}

// cancelNode reclaims an in-flight or failed launch and keeps the record so
// the administrator can see the reclamation result.
func (p *LocalProvisioner) cancelNode(ctx context.Context, body []byte) (*cloudruntime.Response, error) {
	node, provider, errResp := p.reclamationProvider(body)
	if errResp != nil {
		return errResp, nil
	}
	if node.InstanceID != "" && node.Status != StatusTerminated {
		if err := provider.TerminateInstance(ctx, node.InstanceID); err != nil {
			return p.markCleanupRequired(node, err)
		}
	}
	updated, _ := p.store.Update(node.ID, func(n *Node) {
		n.Status = StatusTerminated
		n.Events = append(n.Events, NodeEvent{Event: "cancel_reclaimed", At: nowISO(), Note: n.InstanceID})
	})
	return p.json(http.StatusOK, updated)
}

func (p *LocalProvisioner) deleteNode(ctx context.Context, body []byte) (*cloudruntime.Response, error) {
	node, provider, errResp := p.reclamationProvider(body)
	if errResp != nil {
		return errResp, nil
	}
	if node.Status != StatusTerminated {
		if err := provider.TerminateInstance(ctx, node.InstanceID); err != nil {
			return p.markCleanupRequired(node, err)
		}
	}
	p.store.Delete(node.ID)
	return p.json(http.StatusOK, map[string]string{"deleted": node.ID})
}

// markCleanupRequired keeps the cloud handle when an authorization lapse blocks
// termination, so the resource stays visible for a later cleanup instead of
// becoming an orphaned billable instance.
func (p *LocalProvisioner) markCleanupRequired(node *Node, cause error) (*cloudruntime.Response, error) {
	driverErr, _ := AsDriverError(cause)
	if driverErr == nil {
		driverErr = &DriverError{Code: ErrUnavailable, Message: cause.Error()}
	}
	updated, _ := p.store.Update(node.ID, func(n *Node) {
		n.Status = StatusCleanupRequired
		n.Error = &DriverError{
			Code:       ErrCleanupRequired,
			Message:    "reclamation failed: " + driverErr.Message,
			Actionable: "Restore the role authorization and retry cleanup, or delete the resource in the cloud console.",
		}
		n.Events = append(n.Events, NodeEvent{Event: "cleanup_required", At: nowISO(), Note: string(driverErr.Code)})
	})
	return p.json(http.StatusConflict, updated)
}

// cleanupList returns nodes whose cloud resources could not be reclaimed.
func (p *LocalProvisioner) cleanupList() []*Node {
	var pending []*Node
	for _, node := range p.store.List() {
		if node.Status == StatusCleanupRequired {
			pending = append(pending, node)
		}
	}
	return pending
}

// retryCleanup retries termination after the role authorization is restored.
func (p *LocalProvisioner) retryCleanup(ctx context.Context, body []byte) (*cloudruntime.Response, error) {
	node, err := p.lookupNode(body)
	if err != nil {
		return p.driverError(err)
	}
	if node.Status != StatusCleanupRequired {
		return p.errorJSON(http.StatusBadRequest, ErrConfiguration, "node is not awaiting cleanup")
	}
	provider, err := p.providerForNode(ctx, node)
	if err != nil {
		return p.markCleanupRequired(node, err)
	}
	if err := provider.TerminateInstance(ctx, node.InstanceID); err != nil {
		return p.markCleanupRequired(node, err)
	}
	updated, _ := p.store.Update(node.ID, func(n *Node) {
		n.Status = StatusTerminated
		n.Error = nil
		n.Events = append(n.Events, NodeEvent{Event: "cleanup_recovered", At: nowISO(), Note: n.InstanceID})
	})
	return p.json(http.StatusOK, updated)
}

// --------------------------------------------------------------------------- //
// Connected cloud accounts (Route B)
// --------------------------------------------------------------------------- //

type verifyAccountRequest struct {
	Provider string `json:"provider"`
	RoleARN  string `json:"role_arn"`
	Label    string `json:"label"`
	Region   string `json:"region"`
}

func (p *LocalProvisioner) trustBundle(ctx context.Context, query url.Values) (*cloudruntime.Response, error) {
	providerName := query.Get("provider")
	if providerName == "" {
		providerName = ProviderAWS
	}
	base, errResp := p.roleProvider(providerName)
	if errResp != nil {
		return errResp, nil
	}
	identityPresent := base.IdentityReady()
	var identity map[string]string
	if identityPresent {
		identity, _ = base.Identity(ctx)
	}
	return p.json(http.StatusOK, TrustBundleFor(providerName, identityPresent, identity))
}

func (p *LocalProvisioner) verifyAccount(ctx context.Context, body []byte) (*cloudruntime.Response, error) {
	var req verifyAccountRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return p.errorJSON(http.StatusBadRequest, ErrConfiguration, "invalid account request body")
	}
	providerName := firstNonEmpty(req.Provider, ProviderAWS)
	base, errResp := p.roleProvider(providerName)
	if errResp != nil {
		return errResp, nil
	}
	if !base.IdentityReady() {
		return p.errorJSON(http.StatusConflict, ErrIdentityMissing,
			"the self-hosted deployment has no cloud-recognised identity; ask the deployment administrator to configure it")
	}
	roleARN := strings.TrimSpace(req.RoleARN)
	if !ValidateRoleARN(providerName, roleARN) {
		return p.errorJSON(http.StatusBadRequest, ErrConfiguration, "invalid role ARN for "+providerName)
	}

	// AssumeRole mints short-lived credentials and immediately proves them via
	// the identity/authorization call; nothing long-lived is stored.
	_, identity, err := base.AssumeRole(ctx, roleARN, "helios-verify-"+randHex(4), MaxLeaseSeconds)
	now := time.Now().UTC()
	if err != nil {
		driverErr, _ := AsDriverError(err)
		failed := p.accounts.Upsert(&Account{
			Provider: providerName, Label: req.Label, RoleARN: roleARN, Region: req.Region,
			Status: AccountFailed, LastError: driverErr,
		})
		return p.json(statusForError(driverErr), failed)
	}
	account := p.accounts.Upsert(&Account{
		Provider: providerName, Label: req.Label, RoleARN: roleARN, Region: req.Region,
		Status: AccountVerified, Identity: identity, VerifiedAt: &now,
	})
	return p.json(http.StatusOK, account)
}

func (p *LocalProvisioner) deleteAccount(body []byte) (*cloudruntime.Response, error) {
	var ref struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &ref); err != nil || ref.ID == "" {
		return p.errorJSON(http.StatusBadRequest, ErrConfiguration, "account id is required")
	}
	account, ok := p.accounts.Get(ref.ID)
	if !ok {
		return p.errorJSON(http.StatusNotFound, ErrConfiguration, "unknown cloud account")
	}
	for _, node := range p.store.List() {
		if node.AccountID == account.ID && node.Status != StatusTerminated {
			return p.errorJSON(http.StatusConflict, ErrConfiguration,
				"the account still has an active runtime; stop it before disconnecting the account")
		}
	}
	p.accounts.Delete(ref.ID)
	return p.json(http.StatusOK, map[string]string{"deleted": ref.ID})
}

func (p *LocalProvisioner) setDefaultRuntime(body []byte) (*cloudruntime.Response, error) {
	var ref struct {
		NodeID string `json:"node_id"`
	}
	if err := json.Unmarshal(body, &ref); err != nil {
		return p.errorJSON(http.StatusBadRequest, ErrConfiguration, "invalid default runtime request")
	}
	if ref.NodeID != "" {
		node, ok := p.store.Get(ref.NodeID)
		if !ok || node.Status != StatusRunning {
			return p.errorJSON(http.StatusBadRequest, ErrConfiguration, "only a ready runtime can be the workspace default")
		}
	}
	p.accounts.SetDefaultNode(ref.NodeID)
	return p.json(http.StatusOK, map[string]string{"node_id": ref.NodeID})
}

func (p *LocalProvisioner) driverError(err error) (*cloudruntime.Response, error) {
	driverErr, ok := AsDriverError(err)
	if !ok {
		return p.errorJSON(http.StatusBadGateway, ErrUnavailable, err.Error())
	}
	return p.errorJSON(statusForError(driverErr), driverErr.Code, driverErr.Message)
}

func statusForError(err *DriverError) int {
	switch err.Code {
	case ErrAuthorization, ErrNoAccount:
		return http.StatusForbidden
	case ErrTimeout:
		return http.StatusGatewayTimeout
	case ErrConfiguration:
		return http.StatusBadRequest
	case ErrIdentityMissing, ErrCleanupRequired:
		return http.StatusConflict
	default:
		return http.StatusBadGateway
	}
}

func (p *LocalProvisioner) json(status int, payload any) (*cloudruntime.Response, error) {
	data, _ := json.Marshal(payload)
	return &cloudruntime.Response{StatusCode: status, Body: data,
		Header: http.Header{"Content-Type": []string{"application/json"}}}, nil
}

func (p *LocalProvisioner) errorJSON(status int, code ErrCode, message string) (*cloudruntime.Response, error) {
	return p.json(status, map[string]any{"error": map[string]string{"code": string(code), "message": message}})
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func randHex(bytes int) string {
	buf := make([]byte, bytes)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func nowISO() string { return time.Now().UTC().Format(time.RFC3339) }
