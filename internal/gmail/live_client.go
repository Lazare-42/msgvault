package gmail

// LiveClient exposes composed drafts and sending without changing the native
// draft API used by the daemon's managed draft lifecycle.
type LiveClient struct{ *Client }

func NewLiveClient(client *Client) *LiveClient { return &LiveClient{Client: client} }

var _ LiveAPI = (*LiveClient)(nil)
