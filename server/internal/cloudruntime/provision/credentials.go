package provision

import (
	"context"
	"sync"
	"time"
)

// Fixed credentials used only by the in-test fake clouds. They authenticate
// solely against httptest servers started by the test process, never a real
// cloud account.
const (
	fakeAWSAccessKey    = "AKIAFAKEHELIOS00001"
	fakeAWSSecretKey    = "fake-aws-secret-helios-0001"
	fakeAliyunAccessKey = "LTAIfakeHelios00001"
	fakeAliyunSecretKey = "fake-aliyun-secret-00001"
	// Temp credentials the fake STS hands back on AssumeRole. The fake clouds
	// accept them only together with the matching session token, so a test
	// proves the launch path really runs on short-lived role credentials.
	fakeAWSSessionKey    = "ASIAFAKEHELIOSSESSION1"
	fakeAWSSessionSecret = "fake-aws-session-secret-0001"
	fakeAliyunSessionAK  = "STS.fakeHeliosSession0001"
	fakeAliyunSessionSK  = "fake-aliyun-session-secret-0001"
	fakeSessionToken     = "faketherolesessiontokenhelios"
)

// Credentials are the signing keys used for one cloud request. In Route B the
// deployment identity owns long-lived static keys, while every node operation
// runs on short-lived credentials minted by AssumeRole; neither kind is ever
// rendered into instance user-data.
type Credentials struct {
	AccessKey    string
	SecretKey    string
	SessionToken string
}

// CredentialProvider returns the credentials for the next signed request.
type CredentialProvider interface {
	Credentials(ctx context.Context) (Credentials, error)
}

// staticCredentials serves deployment keys (or the fake cloud's fixed keys).
type staticCredentials struct {
	creds Credentials
}

func (s staticCredentials) Credentials(context.Context) (Credentials, error) {
	if s.creds.AccessKey == "" || s.creds.SecretKey == "" {
		return Credentials{}, &DriverError{
			Code:       ErrIdentityMissing,
			Message:    "deployment cloud identity is not configured",
			Actionable: "Ask the deployment administrator to configure the cloud identity before connecting an account.",
		}
	}
	return s.creds, nil
}

// assumedCredentials caches one AssumeRole session and re-assumes shortly
// before it expires. The long-lived role ARN is the only identity material the
// provisioner persists; session keys never touch the state files.
type assumedCredentials struct {
	parent       roleAssumer
	roleARN      string
	sessionName  string
	duration     time.Duration
	refreshAhead time.Duration

	mu        sync.Mutex
	current   Credentials
	expiresAt time.Time
}

type roleAssumer interface {
	assumeRoleOnce(ctx context.Context, roleARN, sessionName string, duration time.Duration) (Credentials, time.Time, error)
}

func (a *assumedCredentials) Credentials(ctx context.Context) (Credentials, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.current.AccessKey != "" && time.Now().Add(a.refreshAhead).Before(a.expiresAt) {
		return a.current, nil
	}
	creds, expires, err := a.parent.assumeRoleOnce(ctx, a.roleARN, a.sessionName, a.duration)
	if err != nil {
		return Credentials{}, err
	}
	a.current, a.expiresAt = creds, expires
	return creds, nil
}
