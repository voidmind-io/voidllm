package admin

import (
	"net/http"
	"net/url"
	"regexp"
)

// awsTestRegion is the only region AWSTestURL produces hosts for.
const awsTestRegion = "eu-central-1"

// awsTestHostRe matches exactly the hosts produced by AWSTestURL: a Bedrock
// Runtime VPC endpoint name whose endpoint ID is the local server port.
var awsTestHostRe = regexp.MustCompile(`^vpce-([0-9]+)\.bedrock-runtime\.` + awsTestRegion + `\.vpce\.amazonaws\.com$`)

// AWSTestURL returns a valid Bedrock VPC endpoint URL for eu-central-1 that the
// test-connection client routes to the local test server at serverURL. IAM
// test-connection requires a Bedrock Runtime host, so tests cannot point it at
// an httptest URL directly.
func AWSTestURL(serverURL string) string {
	u, err := url.Parse(serverURL)
	if err != nil {
		return serverURL
	}
	return "https://vpce-" + u.Port() + ".bedrock-runtime." + awsTestRegion + ".vpce.amazonaws.com"
}

// awsTestRouter rewrites AWSTestURL hosts to the local server and passes every
// other request through unchanged.
type awsTestRouter struct{}

func (awsTestRouter) RoundTrip(req *http.Request) (*http.Response, error) {
	if m := awsTestHostRe.FindStringSubmatch(req.URL.Hostname()); m != nil {
		r := req.Clone(req.Context())
		r.URL.Scheme = "http"
		r.URL.Host = "127.0.0.1:" + m[1]
		r.Host = r.URL.Host
		return http.DefaultTransport.RoundTrip(r)
	}
	return http.DefaultTransport.RoundTrip(req)
}

func init() {
	testClient.Transport = awsTestRouter{}
}
