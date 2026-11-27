package provision

import "net/http"

// NewFakeVendorHandler exposes the in-process fake vendor used by the Route B
// test suite over a real TCP listener. It exists for local acceptance only:
// the self-hosted server is configured with mode=fake and an endpoint served
// by cmd/fakecloud, so an administrator can click through account connection,
// verification and provisioning without real AWS/Aliyun credentials. The
// signature, STS and instance-state checks are identical to the tests.
func NewFakeVendorHandler(provider string) (http.Handler, error) {
	cloud := newFakeCloud(provider)
	if provider == ProviderAWS {
		return http.HandlerFunc(cloud.handleAWS), nil
	}
	if provider == ProviderAliyun {
		return http.HandlerFunc(cloud.handleAliyun), nil
	}
	return nil, &DriverError{
		Code:       ErrConfiguration,
		Message:    "unknown fake vendor provider: " + provider,
		Actionable: "Use one of: aws, aliyun.",
	}
}
