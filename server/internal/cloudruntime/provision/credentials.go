package provision

// Fixed credentials used only by the in-test fake clouds. They authenticate
// solely against httptest servers started by the test process, never a real
// cloud account.
const (
	fakeAWSAccessKey    = "AKIAFAKEHELIOS00001"
	fakeAWSSecretKey    = "fake-aws-secret-helios-0001"
	fakeAliyunAccessKey = "LTAIfakeHelios00001"
	fakeAliyunSecretKey = "fake-aliyun-secret-0001"
)
