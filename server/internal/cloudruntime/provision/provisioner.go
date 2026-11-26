package provision

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/cloudruntime"
)

const leaseTTL = 600 * time.Second

// LocalProvisioner satisfies the handler's cloudRuntimeProxy interface while
// opening instances directly through the vendor APIs instead of proxying to
// the managed Multica Cloud Fleet. It is wired only when explicitly enabled,
// so deployments using MULTICA_CLOUD_URL keep the original proxy behavior.
type LocalProvisioner struct {
	cfg   Config
	store *Store
}

// New builds a LocalProvisioner from its configuration.
func New(cfg Config) (*LocalProvisioner, error) {
	store, err := NewStore(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	if cfg.ReadinessTimeout <= 0 {
		cfg.ReadinessTimeout = 60 * time.Second
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 200 * time.Millisecond
	}
	return &LocalProvisioner{cfg: cfg, store: store}, nil
}

// Enabled reports whether the built-in driver is switched on.
func (p *LocalProvisioner) Enabled() bool { return p.cfg.Enabled }

// createNodeRequest is the subset of the node contract the driver consumes.
type createNodeRequest struct {
	Provider        string            `json:"provider"`
	Name            string            `json:"name"`
	InstanceType    string            `json:"instance_type"`
	Region          string            `json:"region"`
	ImageID         string            `json:"image_id"`
	SubnetID        string            `json:"subnet_id"`
	InstanceProfile string            `json:"iam_instance_profile"`
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
	case req.Path == "/api/v1/nodes/status" && req.Method == http.MethodPost:
		return p.nodeStatus(ctx, req.Body)
	case req.Path == "/api/v1/nodes/stop" && req.Method == http.MethodPost:
		return p.stopNode(ctx, req.Body)
	case req.Path == "/api/v1/nodes/start" || req.Path == "/api/v1/nodes/reboot":
		return p.errorJSON(http.StatusNotImplemented, ErrUnavailable, "built-in driver does not support power cycling")
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

func (p *LocalProvisioner) createNode(ctx context.Context, ownerID string, body createNodeRequest) (*cloudruntime.Response, error) {
	providerName := body.Provider
	if providerName == "" {
		providerName = ProviderAWS
	}
	provider, ok := p.cfg.Providers[providerName]
	if !ok {
		return p.errorJSON(http.StatusBadRequest, ErrConfiguration, "provider not configured: "+providerName)
	}
	defaults, ok := p.cfg.Defaults[providerName]
	if !ok {
		return p.errorJSON(http.StatusBadRequest, ErrConfiguration, "missing account defaults for provider: "+providerName)
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
			n.Error = nil
			n.Events = append(n.Events, NodeEvent{Event: "retry_requested", At: nowISO()})
		})
	} else {
		node = p.store.Create(&Node{
			ID: "crn-" + randHex(12), OwnerID: ownerID, Provider: providerName, Region: region,
			InstanceType: instanceType, ImageID: imageID, SubnetID: subnetID, Name: runtimeName,
			Status: StatusLaunching, ClientToken: clientToken, Tags: body.Tags,
			Metadata: map[string]any{}, Events: []NodeEvent{{Event: "create_requested", At: nowISO()}},
		})
	}

	ready, err := p.provision(ctx, provider, node, Spec{
		Provider: providerName, Name: runtimeName, Region: region, ImageID: imageID,
		InstanceType: instanceType, SubnetID: subnetID, SecurityGroupIDs: defaults.SecurityGroupIDs,
		InstanceProfile: profile, ClientToken: clientToken, WorkspaceID: ownerID, RuntimeName: runtimeName,
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

func (p *LocalProvisioner) nodeFor(body []byte) (*Node, CloudProvider, error) {
	var ref nodeRef
	if err := json.Unmarshal(body, &ref); err != nil {
		return nil, nil, &DriverError{Code: ErrConfiguration, Message: "invalid node reference"}
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
		return nil, nil, &DriverError{Code: ErrConfiguration, Message: "unknown node"}
	}
	provider, ok := p.cfg.Providers[node.Provider]
	if !ok {
		return nil, nil, &DriverError{Code: ErrConfiguration, Message: "provider no longer configured: " + node.Provider}
	}
	return node, provider, nil
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

func (p *LocalProvisioner) stopNode(ctx context.Context, body []byte) (*cloudruntime.Response, error) {
	node, provider, err := p.nodeFor(body)
	if err != nil {
		return p.driverError(err)
	}
	if err := provider.TerminateInstance(ctx, node.InstanceID); err != nil {
		return p.driverError(err)
	}
	updated, _ := p.store.Update(node.ID, func(n *Node) {
		n.Status = StatusTerminated
		n.Events = append(n.Events, NodeEvent{Event: "runtime_terminated", At: nowISO(), Note: n.InstanceID})
	})
	return p.json(http.StatusOK, updated)
}

func (p *LocalProvisioner) deleteNode(ctx context.Context, body []byte) (*cloudruntime.Response, error) {
	node, provider, err := p.nodeFor(body)
	if err != nil {
		return p.driverError(err)
	}
	if node.Status != StatusTerminated {
		if err := provider.TerminateInstance(ctx, node.InstanceID); err != nil {
			return p.driverError(err)
		}
	}
	p.store.Delete(node.ID)
	return p.json(http.StatusOK, map[string]string{"deleted": node.ID})
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
	case ErrAuthorization:
		return http.StatusForbidden
	case ErrTimeout:
		return http.StatusGatewayTimeout
	case ErrConfiguration:
		return http.StatusBadRequest
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
