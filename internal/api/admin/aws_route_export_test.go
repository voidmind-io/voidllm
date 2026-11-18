package admin

import (
	"net/http"
	"net/url"
	"strings"
)

const awsTestHostSuffix = ".bedrock-runtime.eu-central-1.amazonaws.com"

// AWSTestURL returns an AWS-looking https endpoint that the test-connection
// client routes to the local test server at serverURL. IAM test-connection
// requires an AWS host, so tests cannot point it at an httptest URL directly.
func AWSTestURL(serverURL string) string {
	u, err := url.Parse(serverURL)
	if err != nil {
		return serverURL
	}
	return "https://p" + u.Port() + awsTestHostSuffix
}

// awsTestRouter rewrites AWSTestURL hosts to the local server and passes every
// other request through unchanged.
type awsTestRouter struct{}

func (awsTestRouter) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Hostname()
	if strings.HasPrefix(host, "p") && strings.HasSuffix(host, awsTestHostSuffix) {
		r := req.Clone(req.Context())
		r.URL.Scheme = "http"
		r.URL.Host = "127.0.0.1:" + strings.TrimSuffix(strings.TrimPrefix(host, "p"), awsTestHostSuffix)
		r.Host = r.URL.Host
		return http.DefaultTransport.RoundTrip(r)
	}
	return http.DefaultTransport.RoundTrip(req)
}

func init() {
	testClient.Transport = awsTestRouter{}
}
