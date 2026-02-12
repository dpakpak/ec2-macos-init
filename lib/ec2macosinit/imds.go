package ec2macosinit

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	imdsProbeTimeout      = 200 * time.Millisecond
	imdsTokenTTL          = 21600
	tokenEndpoint         = "latest/api/token"
	tokenRequestTTLHeader = "X-aws-ec2-metadata-token-ttl-seconds"
	tokenHeader           = "X-aws-ec2-metadata-token"
	imdsEndpointModeEnv   = "EC2_METADATA_SERVICE_ENDPOINT_MODE"
)

// baseURL returns an IMDS base URL for the given host.
func baseURL(host string) *url.URL {
	return &url.URL{
		Scheme: "http",
		Host:   host,
	}
}

var (
	imdsIPv4Base = baseURL("169.254.169.254")
	imdsIPv6Base = baseURL("[fd00:ec2::254]")
)

// IMDSConfig contains the current instance ID and a place for the IMDSv2 token to be stored.
// Using IMDSv2:
// https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/configuring-instance-metadata-service.html
type IMDSConfig struct {
	token      string
	imdsBase   *url.URL
	InstanceID string
}

// getIMDSBase returns the IMDS base URL, probing for the correct endpoint
// if not yet determined. If neither endpoint is reachable (e.g. during early
// boot before the network interface is configured), it returns the IPv4
// default without caching, so the existing init retry loop in setup.go will
// re-probe on the next attempt.
//
// The probe timeout is kept short (200ms) to avoid significantly increasing
// the per-retry cost in setup.go's 1-second retry loop (up to 600 attempts).
// IMDS is link-local and responds in sub-millisecond when reachable, so
// 200ms is more than sufficient to detect availability.
func (i *IMDSConfig) getIMDSBase() *url.URL {
	if i.imdsBase != nil {
		return i.imdsBase
	}

	// Honor explicit override if set (matches SDK convention)
	if mode := os.Getenv(imdsEndpointModeEnv); mode != "" {
		switch strings.ToLower(mode) {
		case "ipv6":
			i.imdsBase = imdsIPv6Base
			return i.imdsBase
		case "ipv4":
			i.imdsBase = imdsIPv4Base
			return i.imdsBase
		}
	}

	// Auto-detect: try IPv4 first, fall back to IPv6
	for _, candidate := range []struct {
		addr string
		base *url.URL
	}{
		{"169.254.169.254:80", imdsIPv4Base},
		{"[fd00:ec2::254]:80", imdsIPv6Base},
	} {
		conn, err := net.DialTimeout("tcp", candidate.addr, imdsProbeTimeout)
		if err == nil {
			conn.Close()
			i.imdsBase = candidate.base
			return i.imdsBase
		}
	}

	// Neither endpoint confirmed reachable yet — return IPv4 default
	// without caching so we re-probe on the next call.
	return imdsIPv4Base
}

// getIMDSProperty gets a given endpoint property from IMDS.
func (i *IMDSConfig) getIMDSProperty(endpoint string) (value string, httpResponseCode int, err error) {
	// Check that an IMDSv2 token exists - get one if it doesn't
	if i.token == "" {
		err = i.getNewToken()
		if err != nil {
			return "", 0, fmt.Errorf("ec2macosinit: error while getting new IMDS token: %w\n", err)
		}
	}

	// Create request
	imdsURL := i.getIMDSBase().JoinPath("latest", endpoint)
	client := &http.Client{}
	req, err := http.NewRequest(http.MethodGet, imdsURL.String(), nil)
	if err != nil {
		return "", 0, fmt.Errorf("ec2macosinit: error while creating new HTTP request: %w\n", err)
	}
	req.Header.Set(tokenHeader, i.token)

	// Make request
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("ec2macosinit: error while requesting IMDS property: %w\n", err)
	}
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}

	// Read response body
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, fmt.Errorf("ec2macosinit: error reading response body: %w\n", err)
	}

	return string(data), resp.StatusCode, nil
}

// getNewToken gets a new IMDSv2 token from the IMDS API.
func (i *IMDSConfig) getNewToken() (err error) {
	// Create request
	tokenURL := i.getIMDSBase().JoinPath(tokenEndpoint)
	client := &http.Client{}
	req, err := http.NewRequest(http.MethodPut, tokenURL.String(), nil)
	if err != nil {
		return fmt.Errorf("ec2macosinit: error while creating new HTTP request: %w\n", err)
	}
	req.Header.Set(tokenRequestTTLHeader, strconv.FormatInt(int64(imdsTokenTTL), 10))

	// Make request
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("ec2macosinit: error while requesting new token: %w\n", err)
	}
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}

	// Validate response code
	if resp.StatusCode != 200 {
		return fmt.Errorf("ec2macosinit: received a non-200 status code from IMDS: %d - %s\n",
			resp.StatusCode,
			resp.Status,
		)
	}

	// Read returned value
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("ec2macosinit: error reading response body: %w\n", err)
	}
	i.token = string(data)

	return nil
}

// UpdateInstanceID gets the current instance ID from IMDS.
func (i *IMDSConfig) UpdateInstanceID() (err error) {
	if i.InstanceID != "" {
		return nil
	}

	i.InstanceID, _, err = i.getIMDSProperty("meta-data/instance-id")
	if err != nil {
		return fmt.Errorf("ec2macosinit: error getting instance ID from IMDS: %w\n", err)
	}

	if i.InstanceID == "" {
		return fmt.Errorf("ec2macosinit: an empty instance ID was returned from IMDS\n")
	}

	return nil
}
