package media

// The media tests serve files from httptest servers on 127.0.0.1, which the download client
// refuses in production (guardrail.SafeHTTPClient). The guard itself is tested in guardrail.
func init() { allowPrivateFetch = true }
