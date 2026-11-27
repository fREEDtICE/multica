package provision

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Account is a workspace-connected cloud account. Route B stores only the
// administrator-authorized role ARN and its verification result; no long-lived
// access key is ever persisted.
type Account struct {
	ID         string            `json:"id"`
	Provider   string            `json:"provider"`
	Label      string            `json:"label"`
	RoleARN    string            `json:"role_arn"`
	Region     string            `json:"region"`
	Status     string            `json:"status"`
	Identity   map[string]string `json:"identity"`
	LastError  *DriverError      `json:"last_error,omitempty"`
	VerifiedAt *time.Time        `json:"verified_at,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
}

// TrustBundle carries everything the account-connection screen renders:
// the deployment identity the role must trust, the minimum policy templates,
// and the vendor console guide.
type TrustBundle struct {
	Provider          string            `json:"provider"`
	IdentityPresent   bool              `json:"identity_present"`
	Identity          map[string]string `json:"identity,omitempty"`
	TrustPolicy       string            `json:"trust_policy"`
	PermissionsPolicy string            `json:"permissions_policy"`
	ConsoleGuideURL   string            `json:"console_guide_url"`
	RoleARNExample    string            `json:"role_arn_example"`
}

type settingsDocument struct {
	Accounts      []*Account `json:"accounts"`
	DefaultNodeID string     `json:"default_node_id"`
}

// AccountStore persists connected accounts and the workspace default cloud
// runtime node as one JSON document next to the node state file.
type AccountStore struct {
	mu            sync.Mutex
	byID          map[string]*Account
	defaultNodeID string
	path          string
}

// NewAccountStore loads the settings document, tolerating a missing file.
func NewAccountStore(path string) (*AccountStore, error) {
	s := &AccountStore{byID: map[string]*Account{}, path: path}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var doc settingsDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	for _, account := range doc.Accounts {
		s.byID[account.ID] = account
	}
	s.defaultNodeID = doc.DefaultNodeID
	return s, nil
}

// ListAccounts returns accounts ordered by creation time.
func (s *AccountStore) ListAccounts() []*Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Account, 0, len(s.byID))
	for _, account := range s.byID {
		out = append(out, account)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// VerifiedAccounts returns the usable accounts for one provider.
func (s *AccountStore) VerifiedAccounts(provider string) []*Account {
	var out []*Account
	for _, account := range s.ListAccounts() {
		if account.Provider == provider && account.Status == AccountVerified {
			out = append(out, account)
		}
	}
	return out
}

func (s *AccountStore) Get(id string) (*Account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	account, ok := s.byID[id]
	return account, ok
}

// lookupRole finds an account by provider + role ARN regardless of status.
func (s *AccountStore) lookupRole(provider, roleARN string) *Account {
	for _, account := range s.byID {
		if account.Provider == provider && account.RoleARN == roleARN {
			return account
		}
	}
	return nil
}

// Upsert stores the verification outcome, reusing the record created by an
// earlier failed attempt so re-verification never creates duplicates.
func (s *AccountStore) Upsert(account *Account) *Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if existing := s.lookupRole(account.Provider, account.RoleARN); existing != nil {
		existing.Label = account.Label
		existing.Region = account.Region
		existing.Status = account.Status
		existing.Identity = account.Identity
		existing.LastError = account.LastError
		existing.VerifiedAt = account.VerifiedAt
		existing.UpdatedAt = now
		s.persistLocked()
		return existing
	}
	if account.ID == "" {
		account.ID = "cca-" + randHex(12)
	}
	account.CreatedAt, account.UpdatedAt = now, now
	s.byID[account.ID] = account
	s.persistLocked()
	return account
}

// Delete removes an account; the provisioner checks node references first.
func (s *AccountStore) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[id]; !ok {
		return false
	}
	delete(s.byID, id)
	s.persistLocked()
	return true
}

func (s *AccountStore) SetDefaultNode(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.defaultNodeID = id
	s.persistLocked()
}

func (s *AccountStore) DefaultNode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.defaultNodeID
}

func (s *AccountStore) persistLocked() {
	if s.path == "" {
		return
	}
	doc := settingsDocument{DefaultNodeID: s.defaultNodeID}
	for _, account := range s.byID {
		doc.Accounts = append(doc.Accounts, account)
	}
	sort.Slice(doc.Accounts, func(i, j int) bool {
		return doc.Accounts[i].CreatedAt.Before(doc.Accounts[j].CreatedAt)
	})
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".cloud-accounts-*.tmp")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	_ = os.Rename(tmp.Name(), s.path)
}

// ValidateRoleARN checks the role identifier shape before any cloud call.
func ValidateRoleARN(provider, roleARN string) bool {
	roleARN = strings.TrimSpace(roleARN)
	switch provider {
	case ProviderAWS:
		// arn:aws:iam::123456789012:role/Path/Name
		if !strings.HasPrefix(roleARN, "arn:aws:iam::") {
			return false
		}
		rest := strings.TrimPrefix(roleARN, "arn:aws:iam::")
		parts := strings.SplitN(rest, ":role/", 2)
		return len(parts) == 2 && len(parts[0]) == 12 && isDigits(parts[0]) && parts[1] != ""
	case ProviderAliyun:
		// acs:ram::123456789012:role/name
		if !strings.HasPrefix(roleARN, "acs:ram::") {
			return false
		}
		rest := strings.TrimPrefix(roleARN, "acs:ram::")
		parts := strings.SplitN(rest, ":role/", 2)
		return len(parts) == 2 && isDigits(parts[0]) && parts[1] != ""
	default:
		return false
	}
}

func isDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// TrustBundleFor renders the copyable trust information and minimum-privilege
// policy for the provider, interpolating the deployment identity when present.
func TrustBundleFor(provider string, identityPresent bool, identity map[string]string) TrustBundle {
	switch provider {
	case ProviderAWS:
		principal := "REPLACE_WITH_DEPLOYMENT_IDENTITY_ARN"
		if arn := identity["arn"]; arn != "" {
			principal = arn
		}
		return TrustBundle{
			Provider:        ProviderAWS,
			IdentityPresent: identityPresent,
			Identity:        identity,
			TrustPolicy: `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": { "AWS": "` + principal + `" },
      "Action": "sts:AssumeRole"
    }
  ]
}`,
			PermissionsPolicy: `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ec2:RunInstances",
        "ec2:DescribeInstances",
        "ec2:DescribeSubnets",
        "ec2:DescribeSecurityGroups",
        "ec2:DescribeInstanceAttribute",
        "ec2:GetConsoleOutput",
        "ec2:TerminateInstances"
      ],
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": "iam:PassRole",
      "Resource": "*"
    }
  ]
}`,
			ConsoleGuideURL: "https://docs.aws.amazon.com/IAM/latest/UserGuide/id_roles_common-scenarios_third-party.html",
			RoleARNExample:  "arn:aws:iam::123456789012:role/MulticaCloudRuntime",
		}
	default:
		accountID := "REPLACE_WITH_DEPLOYMENT_ACCOUNT_ID"
		if account := identity["account"]; account != "" {
			accountID = account
		}
		return TrustBundle{
			Provider:        ProviderAliyun,
			IdentityPresent: identityPresent,
			Identity:        identity,
			TrustPolicy: `{
  "Statement": [
    {
      "Action": "sts:AssumeRole",
      "Effect": "Allow",
      "Principal": { "RAM": "` + accountID + `" }
    }
  ],
  "Version": "1"
}`,
			PermissionsPolicy: `{
  "Version": "1",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ecs:RunInstances",
        "ecs:DescribeInstances",
        "ecs:DescribeUserData",
        "ecs:DescribeVSwitches",
        "ecs:DescribeSecurityGroups",
        "ecs:DeleteInstance"
      ],
      "Resource": "*"
    }
  ]
}`,
			ConsoleGuideURL: "https://www.alibabacloud.com/help/en/ram/user-guide/assume-a-ram-role",
			RoleARNExample:  "acs:ram::123456789012:role/MulticaCloudRuntime",
		}
	}
}
