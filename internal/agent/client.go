package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/imshodj/oceanic/internal/protocol"
)

// ErrGone means the server no longer knows this agent (device deleted).
var ErrGone = fmt.Errorf("agent was removed from the server")

type client struct {
	cfg  *Config
	http *http.Client
}

func newClient(cfg *Config) *client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyFromEnvironment
	return &client{cfg: cfg, http: &http.Client{Transport: tr, Timeout: 90 * time.Second}}
}

func (c *client) newReq(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.ServerURL+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", "OCeanic-Agent/"+protocol.AgentVersion)
	if c.cfg.CFAccessClientID != "" {
		req.Header.Set("CF-Access-Client-Id", c.cfg.CFAccessClientID)
		req.Header.Set("CF-Access-Client-Secret", c.cfg.CFAccessClientSecret)
	}
	if c.cfg.Enrolled() {
		req.Header.Set("Authorization", "Bearer "+c.cfg.AgentID+"."+c.cfg.AgentSecret)
	}
	return req, nil
}

func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	req, err := c.newReq(ctx, method, path, body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusGone {
		return ErrGone
	}
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(msg))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *client) enroll(ctx context.Context, req protocol.EnrollRequest) (*protocol.EnrollResponse, error) {
	var out protocol.EnrollResponse
	err := c.do(ctx, "POST", "/api/agent/enroll", req, &out)
	return &out, err
}

func (c *client) checkin(ctx context.Context, req protocol.CheckinRequest) (*protocol.CheckinResponse, error) {
	var out protocol.CheckinResponse
	err := c.do(ctx, "POST", "/api/agent/checkin", req, &out)
	return &out, err
}

func (c *client) jobs(ctx context.Context) ([]protocol.Job, error) {
	var out protocol.JobsResponse
	err := c.do(ctx, "GET", "/api/agent/jobs", nil, &out)
	return out.Jobs, err
}

func (c *client) result(ctx context.Context, r protocol.JobResult) error {
	return c.do(ctx, "POST", "/api/agent/result", r, nil)
}

// download fetches the latest agent binary for this platform.
func (c *client) download(ctx context.Context, platform string, w io.Writer) error {
	req, err := c.newReq(ctx, "GET", "/api/agent/binary/"+platform, nil)
	if err != nil {
		return err
	}
	hc := *c.http
	hc.Timeout = 15 * time.Minute
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}
