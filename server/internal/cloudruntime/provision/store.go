package provision

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Node is the persisted cloud runtime node. Its JSON field names match the
// CloudRuntimeNode contract the web client already consumes, with provider
// added for the multi-vendor selection.
type Node struct {
	ID           string            `json:"id"`
	OwnerID      string            `json:"owner_id"`
	Provider     string            `json:"provider"`
	InstanceID   string            `json:"instance_id"`
	Region       string            `json:"region"`
	InstanceType string            `json:"instance_type"`
	ImageID      string            `json:"image_id"`
	SubnetID     string            `json:"subnet_id"`
	Name         string            `json:"name"`
	Status       string            `json:"status"`
	ClientToken  string            `json:"client_token"`
	UserDataHash string            `json:"user_data_sha256"`
	LeaseHash    string            `json:"lease_sha256"`
	Tags         map[string]string `json:"tags"`
	Metadata     map[string]any    `json:"metadata"`
	Error        *DriverError      `json:"error,omitempty"`
	Events       []NodeEvent       `json:"events"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

// NodeEvent records one lifecycle transition for audit and operator review.
type NodeEvent struct {
	Event string `json:"event"`
	At    string `json:"at"`
	Note  string `json:"note,omitempty"`
}

// Store keeps node records in memory and optionally mirrors them to a JSON
// file so a restart resumes the same instance instead of opening another.
type Store struct {
	mu        sync.Mutex
	byID      map[string]*Node
	byToken   map[string]string
	statePath string
}

// NewStore loads any previously persisted records.
func NewStore(statePath string) (*Store, error) {
	s := &Store{byID: map[string]*Node{}, byToken: map[string]string{}, statePath: statePath}
	if statePath == "" {
		return s, nil
	}
	data, err := os.ReadFile(statePath)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var nodes []*Node
	if err := json.Unmarshal(data, &nodes); err != nil {
		return nil, err
	}
	for _, node := range nodes {
		s.byID[node.ID] = node
		s.byToken[node.ClientToken] = node.ID
	}
	return s, nil
}

func (s *Store) add(node *Node) {
	s.byID[node.ID] = node
	if node.ClientToken != "" {
		s.byToken[node.ClientToken] = node.ID
	}
}

func (s *Store) byClientToken(token string) *Node {
	if id, ok := s.byToken[token]; ok {
		return s.byID[id]
	}
	return nil
}

// LookupByClientToken returns the node previously created with this token.
func (s *Store) LookupByClientToken(token string) (*Node, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	node := s.byClientToken(token)
	return node, node != nil
}

// Create inserts a node; an existing record with the same client token is
// returned unchanged to keep retries idempotent.
func (s *Store) Create(node *Node) *Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	if node.ClientToken != "" {
		if id, ok := s.byToken[node.ClientToken]; ok {
			return s.byID[id]
		}
	}
	now := time.Now().UTC()
	node.CreatedAt, node.UpdatedAt = now, now
	s.add(node)
	s.persistLocked()
	return node
}

// Update applies a mutation under the lock and persists the result.
func (s *Store) Update(id string, mutate func(*Node)) (*Node, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	node, ok := s.byID[id]
	if !ok {
		return nil, false
	}
	mutate(node)
	node.UpdatedAt = time.Now().UTC()
	s.persistLocked()
	return node, true
}

func (s *Store) Get(id string) (*Node, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	node, ok := s.byID[id]
	return node, ok
}

func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	node, ok := s.byID[id]
	if !ok {
		return false
	}
	delete(s.byToken, node.ClientToken)
	delete(s.byID, id)
	s.persistLocked()
	return true
}

// List returns nodes ordered by creation time.
func (s *Store) List() []*Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Node, 0, len(s.byID))
	for _, node := range s.byID {
		out = append(out, node)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (s *Store) persistLocked() {
	if s.statePath == "" {
		return
	}
	nodes := make([]*Node, 0, len(s.byID))
	for _, node := range s.byID {
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].CreatedAt.Before(nodes[j].CreatedAt) })
	data, err := json.MarshalIndent(nodes, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(s.statePath)
	tmp, err := os.CreateTemp(dir, ".cloud-runtime-*.tmp")
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
	_ = os.Rename(tmp.Name(), s.statePath)
}
