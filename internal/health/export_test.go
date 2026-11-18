package health

import "net/http"

// SetHTTPClientForTest replaces the probe HTTP client so tests can route AWS
// host names to local servers.
func (c *Checker) SetHTTPClientForTest(client *http.Client) {
	c.client = client
}
