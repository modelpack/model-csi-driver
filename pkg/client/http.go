package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/pkg/errors"
)

type HTTPClient struct {
	baseURL url.URL
	client  *http.Client
}

func NewHTTPClient(addr string) (*HTTPClient, error) {
	url, err := url.Parse(addr)
	if err != nil {
		return nil, errors.Wrapf(err, "parse addr %s", addr)
	}

	baseAddr := "http://unix"
	baseURL, err := url.Parse(baseAddr)
	if err != nil {
		return nil, errors.Wrapf(err, "parse base addr %s", addr)
	}

	client := http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial(url.Scheme, url.Path)
			},
		},
	}

	return &HTTPClient{
		baseURL: *baseURL,
		client:  &client,
	}, nil
}

func (client *HTTPClient) request(ctx context.Context, method, endpoint string, body interface{}, query map[string]string, ret interface{}) ([]byte, error) {
	var payload io.Reader
	if body != nil {
		var err error
		payload, err = dumpPayload(body)
		if err != nil {
			return nil, err
		}
	}

	url := client.baseURL
	url.Path = path.Join(url.Path, endpoint)
	for k, v := range query {
		q := url.Query()
		q.Set(k, v)
		url.RawQuery = q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, url.String(), payload)
	if err != nil {
		return nil, errors.Wrap(err, "new request")
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.client.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "do request")
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		msg, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, errors.Wrap(err, "read from body for error message")
		}
		return nil, errors.New(string(msg))
	}

	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		return nil, fmt.Errorf("broken api endpoint")
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(err, "read from body")
	}

	if ret != nil {
		if err := json.Unmarshal(data, ret); err != nil {
			return nil, errors.Wrap(err, "unmarshal body")
		}
	}

	return data, nil
}

func dumpPayload(obj interface{}) (io.Reader, error) {
	payload, err := json.Marshal(obj)
	if err != nil {
		return nil, errors.Wrap(err, "marshal request payload")
	}
	return bytes.NewReader(payload), nil
}
