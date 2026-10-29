package claude

import "net/http"

// NewClaudeAuthForTest lets external integration tests replace only HTTP I/O.
// The OAuth request, body parser and header receipt capture remain production code.
func NewClaudeAuthForTest(client *http.Client) *ClaudeAuth {
	return &ClaudeAuth{httpClient: client}
}
